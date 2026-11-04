package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/krishnanand20/einsora/controlplane/internal/client"
)

func isClientCommand(cmd string) bool {
	switch cmd {
	case "workloads", "machines", "catalog", "launch", "stop", "start", "delete", "events", "logs", "export":
		return true
	}
	return false
}

// cli is where a client command reads and writes: the terminal, or a test's buffers.
type cli struct {
	in          io.Reader
	out         io.Writer
	errOut      io.Writer // prompts, so they never mix with output a script reads
	interactive bool      // in is a terminal someone can answer a prompt at
}

// runClient implements the CLI commands that talk to a running API.
func runClient(ctx context.Context, cmd string, args []string, term cli) error {
	c := client.FromEnv(os.Getenv)
	stdout := term.out
	switch cmd {
	case "workloads":
		fs := flag.NewFlagSet("workloads", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		all := fs.Bool("all", false, "include deleted workloads")
		asJSON := fs.Bool("json", false, "print the workloads as JSON")
		if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
			return errors.New("usage: einsora workloads [-all] [-json]")
		}
		ws, err := c.ListWorkloads(ctx, *all)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSONList(stdout, ws, func(w client.Workload) json.RawMessage { return w.JSON })
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
		fs := flag.NewFlagSet("machines", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		asJSON := fs.Bool("json", false, "print the machines as JSON")
		if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
			return errors.New("usage: einsora machines [-json]")
		}
		ms, err := c.ListMachines(ctx)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSONList(stdout, ms, func(m client.Machine) json.RawMessage { return m.JSON })
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

	case "catalog":
		fs := flag.NewFlagSet("catalog", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		asJSON := fs.Bool("json", false, "print the catalog as JSON")
		if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
			return errors.New("usage: einsora catalog [-json]")
		}
		cat, err := c.Catalog(ctx)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(stdout, cat.JSON)
		}
		return printCatalog(stdout, cat)

	case "launch":
		fs := flag.NewFlagSet("launch", flag.ContinueOnError)
		image := fs.String("image", "", "the image to run (einsora catalog lists them)")
		class := fs.String("class", "", "the GPU or CPU class to run on")
		gpus := fs.Int("gpus", 1, "how many of the class")
		region := fs.String("region", "", "where it runs and its data stays (default: the catalog's first)")
		model := fs.String("model", "", "an endpoint's model: hf://org/model[:TAG] or s3://bucket/prefix")
		sshKey := fs.String("ssh-key", "", "a VM's SSH public key, or a file holding one")
		asJSON := fs.Bool("json", false, "print the new workload as JSON")
		const usage = "einsora launch <notebook|endpoint|vm> <name> -image <id> -class <id> [-gpus n] [-region id] " +
			"[-model uri] [-ssh-key key-or-file] [-json]"
		pos, err := parseArgs(fs, args, 2, usage)
		if err != nil {
			return err
		}
		if *image == "" || *class == "" {
			return errors.New("-image and -class are required (einsora catalog lists them)\nusage: " + usage)
		}
		key, err := sshPublicKey(*sshKey)
		if err != nil {
			return err
		}
		w, err := c.Launch(ctx, client.LaunchInput{Kind: pos[0], Name: pos[1], ImageID: *image, GPUClass: *class,
			GPUCount: *gpus, Region: *region, ModelURI: *model, SSHPublicKey: key})
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(stdout, w.JSON)
		}
		_, err = fmt.Fprintf(stdout, "Launched %s (%s): %s, %d × %s in %s.\nFollow it with: einsora events %s\n",
			printable(w.Name), printable(w.ID), printable(w.State), w.GPUCount, printable(w.GPUClass), printable(w.Region), printable(w.ID))
		return err

	case "stop", "start", "delete":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "print the workload as JSON")
		yes := fs.Bool("y", false, "delete without asking")
		usage := "einsora " + cmd + " <workload> [-json]"
		if cmd == "delete" {
			usage = "einsora delete <workload> [-y] [-json]"
		}
		ref, err := parseWithID(fs, args, usage)
		if err != nil {
			return err
		}
		if *yes && cmd != "delete" {
			return errors.New("-y is only for delete\nusage: " + usage)
		}
		id, err := c.WorkloadID(ctx, ref, false)
		if err != nil {
			return err
		}
		if cmd == "delete" && !*yes {
			if err := confirmDelete(ctx, c, id, term); err != nil {
				return err
			}
		}
		act := map[string]func(context.Context, string) (client.Workload, error){"stop": c.Stop, "start": c.Start, "delete": c.Delete}[cmd]
		w, err := act(ctx, id)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(stdout, w.JSON)
		}
		_, err = fmt.Fprintf(stdout, "%s (%s): %s\n", printable(w.Name), printable(w.ID), printable(w.State))
		return err

	case "events":
		fs := flag.NewFlagSet("events", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "print the raw JSON events")
		ref, err := parseWithID(fs, args, "einsora events <workload> [-json]")
		if err != nil {
			return err
		}
		id, err := c.WorkloadID(ctx, ref, true)
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

	case "logs":
		fs := flag.NewFlagSet("logs", flag.ContinueOnError)
		follow := fs.Bool("f", false, "keep printing new output until interrupted")
		asJSON := fs.Bool("json", false, "print each piece of output as a line of JSON")
		ref, err := parseWithID(fs, args, "einsora logs <workload> [-f] [-json]")
		if err != nil {
			return err
		}
		id, err := c.WorkloadID(ctx, ref, true)
		if err != nil {
			return err
		}
		return printOutput(ctx, c, id, *follow, *asJSON, term)

	case "export":
		fs := flag.NewFlagSet("export", flag.ContinueOnError)
		out := fs.String("o", "", "file to write (default: einsora-<name>-<id>.json; - for stdout)")
		force := fs.Bool("f", false, "replace the file if it exists")
		ref, err := parseWithID(fs, args, "einsora export <workload> [-o file] [-f]")
		if err != nil {
			return err
		}
		id, err := c.WorkloadID(ctx, ref, true)
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

// parseWithID accepts flags before or after the workload's name or ID (`export <id> -o f` and
// `export -o f <id>`).
func parseWithID(fs *flag.FlagSet, args []string, usage string) (string, error) {
	pos, err := parseArgs(fs, args, 1, usage)
	if err != nil {
		if errors.Is(err, errMissingArgs) {
			return "", errors.New("missing workload name or ID\nusage: " + usage)
		}
		return "", err
	}
	return pos[0], nil
}

var errMissingArgs = errors.New("missing arguments")

// parseArgs parses flags anywhere among want positional arguments, which it returns in order.
func parseArgs(fs *flag.FlagSet, args []string, want int, usage string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, fmt.Errorf("%w\nusage: %s", err, usage)
		}
		if fs.NArg() == 0 {
			break
		}
		if len(pos) == want {
			return nil, fmt.Errorf("unexpected argument %q\nusage: %s", printable(fs.Arg(0)), usage)
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) < want {
		return nil, fmt.Errorf("%w\nusage: %s", errMissingArgs, usage)
	}
	return pos, nil
}

// confirmDelete asks before a delete, since a workload's disk and data go with it. With no
// terminal to ask at, the command needs -y instead.
func confirmDelete(ctx context.Context, c *client.Client, id string, term cli) error {
	if !term.interactive {
		return errors.New("delete removes the workload with its disk and data; add -y to confirm")
	}
	w, err := c.GetWorkload(ctx, id)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(term.errOut, "Delete %s (%s)? It's removed with its disk and data; its timeline and export stay. [y/N] ",
		printable(w.Name), printable(w.ID))
	answer, _ := bufio.NewReader(term.in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	}
	return errors.New("not deleted")
}

// sshPublicKey is -ssh-key's value: a public key line, or a file holding one. A file that isn't
// one public key line is refused before anything is sent, so a private key never leaves this
// machine, and its contents are never printed.
func sshPublicKey(v string) (string, error) {
	isKey := func(s string) bool {
		for _, prefix := range []string{"ssh-", "ecdsa-", "sk-"} {
			if strings.HasPrefix(s, prefix) {
				return !strings.ContainsAny(s, "\r\n")
			}
		}
		return false
	}
	if v == "" || isKey(v) {
		return v, nil
	}
	path := v
	if rest, ok := strings.CutPrefix(v, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, rest)
	}
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("-ssh-key: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 16<<10))
	if err != nil {
		return "", fmt.Errorf("-ssh-key: %w", err)
	}
	key := strings.TrimSpace(string(data))
	switch {
	case bytes.Contains(data, []byte("PRIVATE KEY")) || bytes.Contains(data, []byte("Private-Lines")):
		return "", fmt.Errorf("-ssh-key: %s is a private key; give its public key instead (usually %s.pub)", path, path)
	case !isKey(key):
		return "", fmt.Errorf("-ssh-key: %s isn't one OpenSSH public key line", path)
	}
	return key, nil
}

// printJSON prints JSON as the server sent it, indented for reading.
func printJSON(out io.Writer, raw json.RawMessage) error {
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		return fmt.Errorf("the server's response isn't valid JSON: %w", err)
	}
	b.WriteByte('\n')
	_, err := out.Write(b.Bytes())
	return err
}

// printJSONList prints items as a JSON array of the objects the server sent.
func printJSONList[T any](out io.Writer, items []T, raw func(T) json.RawMessage) error {
	parts := make([][]byte, len(items))
	for i, item := range items {
		parts[i] = raw(item)
	}
	return printJSON(out, append(append([]byte{'['}, bytes.Join(parts, []byte{','})...), ']'))
}

// printCatalog prints the regions, classes, and images the caller can launch.
func printCatalog(out io.Writer, cat client.Catalog) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "REGION\tNAME") // errors surface from Flush
	for i, r := range cat.Regions {
		name := printable(r.DisplayName)
		if i == 0 {
			name += " (default)"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", printable(r.ID), name)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	tw = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "\nCLASS\tACCELERATOR\tMEMORY\tVMS\tREGIONS")
	for _, g := range cat.GPUClasses {
		memory, vms := "-", "no"
		if g.VRAMGB > 0 {
			memory = fmt.Sprintf("%d GB", g.VRAMGB)
		}
		if g.SupportsVM {
			vms = "yes"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", printable(g.ID), printable(g.Accelerator), memory, vms,
			printable(strings.Join(g.Regions, ", ")))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	tw = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "\nIMAGE\tKIND\tRUNS ON")
	for _, img := range cat.Images {
		var runs []string
		for _, rt := range img.Runtimes {
			r := strings.Join(rt.Accelerators, ", ")
			if rt.Server != "" {
				r += ": " + rt.Server
				if rt.ModelFormat != "" {
					r += " (" + rt.ModelFormat + ")"
				}
			}
			runs = append(runs, r)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", printable(img.ID), printable(img.Kind), printable(strings.Join(runs, "; ")))
	}
	return tw.Flush()
}

// printOutput prints a workload's kept output, and with follow, what comes after, until
// interrupted. Notes, such as output that isn't collected or was removed, go to errOut once each.
func printOutput(ctx context.Context, c *client.Client, id string, follow, asJSON bool, term cli) error {
	noted := map[string]bool{}
	var after int64
	waiting := false
	for {
		page, err := c.Output(ctx, id, after, waiting)
		select {
		case <-ctx.Done():
			return nil // interrupted: Ctrl+C ends -f
		default:
		}
		if err != nil {
			return err
		}
		for _, note := range page.Notes {
			if !noted[note] {
				noted[note] = true
				_, _ = fmt.Fprintln(term.errOut, "einsora: "+printable(note))
			}
		}
		for _, chunk := range page.Items {
			var line string
			switch {
			case asJSON:
				line = string(chunk.JSON) + "\n"
			case chunk.Stream == "einsora":
				line = "[einsora] " + terminalText(chunk.Text)
			default:
				line = terminalText(chunk.Text)
			}
			if _, err := io.WriteString(term.out, line); err != nil {
				return err
			}
		}
		after = page.Next
		switch {
		case len(page.Items) > 0:
			waiting = false // there may be more already: read on without waiting
		case !follow || !page.Collected:
			return nil
		default:
			waiting = true
		}
	}
}

// terminalEscape matches the escape sequences programs print for colours, cursor movement, and
// window titles, which output shouldn't get to send to the terminal reading it.
var terminalEscape = regexp.MustCompile(`\x1b(\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|[@-Z\\-_])`)

// terminalText makes a workload's output safe to print: escape sequences are removed, carriage
// returns (progress bars redrawing a line) become newlines, and other control characters go.
// Newlines and tabs stay.
func terminalText(s string) string {
	s = terminalEscape.ReplaceAllString(s, "")
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}
