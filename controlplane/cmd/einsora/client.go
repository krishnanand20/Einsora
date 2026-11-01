package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/krishnanand20/einsora/controlplane/internal/client"
)

func isClientCommand(cmd string) bool {
	switch cmd {
	case "workloads", "events", "export", "machines":
		return true
	}
	return false
}

// runClient implements the CLI commands that talk to a running API.
func runClient(ctx context.Context, cmd string, args []string, stdout io.Writer) error {
	c := client.FromEnv(os.Getenv)
	switch cmd {
	case "workloads":
		fs := flag.NewFlagSet("workloads", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		all := fs.Bool("all", false, "include deleted workloads")
		if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
			return errors.New("usage: einsora workloads [-all]")
		}
		ws, err := c.ListWorkloads(ctx, *all)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		// tabwriter buffers: write errors surface from Flush below.
		_, _ = fmt.Fprintln(tw, "ID\tNAME\tKIND\tSTATE\tGPUS\tREGION\tUPDATED")
		for _, w := range ws {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d × %s\t%s\t%s\n", printable(w.ID), printable(w.Name), printable(w.Kind),
				printable(w.State), w.GPUCount, printable(classShown(w)), printable(w.Region), w.UpdatedAt.Format(time.RFC3339))
		}
		return tw.Flush()

	case "machines":
		if len(args) > 0 {
			return errors.New("usage: einsora machines")
		}
		ms, err := c.ListMachines(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "NAME\tSTATUS\tREGION\tSYSTEM\tCPUS\tMEMORY\tGPUS\tWORKLOADS\tLAST SEEN")
		for _, m := range ms {
			seen := "never"
			if m.LastSeenAt != nil {
				seen = m.LastSeenAt.Format(time.RFC3339)
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s/%s\t%d\t%d GiB\t%s\t%d\t%s\n", printable(m.Name), printable(m.Status),
				printable(m.Region), printable(m.OS), printable(m.Arch), m.CPUs, m.MemoryMiB/1024, printable(gpus(m)), m.Workloads, seen)
		}
		return tw.Flush()

	case "events":
		fs := flag.NewFlagSet("events", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "print the raw JSON events")
		id, err := parseWithID(fs, args, "einsora events <workload-id> [-json]")
		if err != nil {
			return err
		}
		events, err := c.Events(ctx, id)
		if err != nil {
			return err
		}
		if *asJSON {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(events)
		}
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "TIME (UTC)\tCHANGE\tREASON") // errors surface from Flush
		for _, e := range events {
			from := e.From
			if from == "" {
				from = "∅"
			}
			change := printable(from) + " → " + printable(e.To)
			if e.From == e.To {
				change = "(still " + printable(e.To) + ")" // a note: no state change
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", e.At.UTC().Format(time.RFC3339), change, printable(e.Reason))
		}
		return tw.Flush()

	case "export":
		fs := flag.NewFlagSet("export", flag.ContinueOnError)
		out := fs.String("o", "", "file to write (default: einsora-<name>-<id>.json; - for stdout)")
		force := fs.Bool("f", false, "replace the file if it exists")
		id, err := parseWithID(fs, args, "einsora export <workload-id> [-o file] [-f]")
		if err != nil {
			return err
		}
		body, filename, err := c.Export(ctx, id)
		if err != nil {
			return err
		}
		if *out == "-" {
			_, err := stdout.Write(body)
			return err
		}
		path := *out
		if path == "" {
			path = filepath.Base(filename) // the client checked the name; never let a server pick a directory
		}
		if err := writePrivate(path, body, *force); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "wrote %s (%d bytes)\n", path, len(body))
		return err
	}
	return fmt.Errorf("unknown client command %q", cmd)
}

// writePrivate writes an export readable by its owner only (0600). It never follows a symlink or
// replaces an existing file unless asked; a replacement is written to a temporary file and renamed
// into place, so the result is always a fresh 0600 file.
func writePrivate(path string, data []byte, replace bool) error {
	if !replace {
		f, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists; use -f to replace it, or -o to choose another file", path)
		}
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		return errors.Join(err, f.Close())
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".einsora-export-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // fails harmlessly once renamed
	_, err = tmp.Write(data)
	if err = errors.Join(err, tmp.Chmod(0o600), tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// classShown is what a workload runs on: the placed class, saying so when it differs from the
// class requested (an opt-in fallback, ADR-0010), or the requested class before it's admitted.
func classShown(w client.Workload) string {
	if w.PlacedClass == "" || w.PlacedClass == w.GPUClass {
		return w.GPUClass
	}
	return w.PlacedClass + " (requested " + w.GPUClass + ")"
}

// printable is client.Printable, for the tables printed here.
func printable(s string) string { return client.Printable(s) }

// gpus describes a machine's GPUs, such as "1 × Apple M3 Pro", or "none".
func gpus(m client.Machine) string {
	var parts []string
	for _, a := range m.Accelerators {
		parts = append(parts, fmt.Sprintf("%d × %s", a.Count, a.Model))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// parseWithID accepts flags before or after the workload ID (`export <id> -o f` and `export -o f <id>`).
func parseWithID(fs *flag.FlagSet, args []string, usage string) (string, error) {
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return "", fmt.Errorf("%w\nusage: %s", err, usage)
	}
	if fs.NArg() == 0 {
		return "", errors.New("missing workload ID\nusage: " + usage)
	}
	id, rest := fs.Arg(0), fs.Args()[1:]
	if err := fs.Parse(rest); err != nil {
		return "", fmt.Errorf("%w\nusage: %s", err, usage)
	}
	if fs.NArg() > 0 {
		return "", fmt.Errorf("unexpected argument %q\nusage: %s", fs.Arg(0), usage)
	}
	return id, nil
}
