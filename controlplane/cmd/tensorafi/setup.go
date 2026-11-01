package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/tensora/tensora/controlplane/internal/catalog"
	"github.com/tensora/tensora/controlplane/internal/config"
	"github.com/tensora/tensora/controlplane/internal/service"
	"github.com/tensora/tensora/controlplane/internal/store"
)

// runSetup creates the first account, the machine's owner (ADR-0012). It works on the store
// directly, so only someone with access to this machine's database can do it: there's no
// first-come web form for a stranger to claim an installation with.
func runSetup(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	email := fs.String("email", "", "the owner's email (required)")
	tenant := fs.String("tenant", "home", "the owner's tenant (created, trusted, if it doesn't exist)")
	name := fs.String("name", "", "the tenant's display name")
	fromStdin := fs.Bool("password-stdin", false, "read the password from the first line of stdin")
	reset := fs.Bool("reset-password", false, "set a new password for an existing account and sign it out everywhere")
	usage := "usage: tensora setup -email you@example.com [-tenant home] [-name \"My lab\"] [-password-stdin]\n" +
		"       tensora setup -reset-password -email you@example.com [-password-stdin]"
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || *email == "" {
		return errors.New(usage)
	}
	password, err := readPassword(stdin, stdout, *fromStdin)
	if err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log, closeLog, err := newLogger(cfg)
	if err != nil {
		return err
	}
	defer closeLog()
	cat, err := catalog.Load(cfg.CatalogPath)
	if err != nil {
		return err
	}
	st, err := openStore(ctx, cfg, log, false)
	if err != nil {
		return err
	}
	defer st.Close()
	svc := service.New(st, cat, nil)
	if *reset {
		u, err := svc.ResetPassword(ctx, *email, password)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "Set a new password for %s and signed it out everywhere. Its API tokens still work;\n"+
			"revoke any you don't trust on the dashboard's API tokens page.\n", u.Email)
		return err
	}
	u, err := svc.SetupOwner(ctx, service.SetupInput{
		Email: *email, Password: password, TenantSlug: *tenant, TenantName: *name,
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Created the owner account %s in tenant %q (trusted).\n"+
		"Run the server with TENSORA_AUTH_MODE=local, then sign in on the dashboard, or create an API\n"+
		"token there for the CLI (TENSORA_TOKEN).\n", u.Email, *tenant)
	return err
}

// readPassword reads the password without echoing it, asking twice, or from stdin's first line.
func readPassword(stdin io.Reader, stdout io.Writer, fromStdin bool) (string, error) {
	if fromStdin {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	f, ok := stdin.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) { //nolint:gosec // a file descriptor fits in an int
		return "", errors.New("no terminal to ask for the password on: use -password-stdin")
	}
	ask := func(prompt string) (string, error) {
		if _, err := fmt.Fprint(stdout, prompt); err != nil {
			return "", err
		}
		b, err := term.ReadPassword(int(f.Fd())) //nolint:gosec // a file descriptor fits in an int
		_, _ = fmt.Fprintln(stdout)
		return string(b), err
	}
	first, err := ask("Password (at least 12 characters): ")
	if err != nil {
		return "", err
	}
	second, err := ask("Again: ")
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errors.New("the passwords don't match")
	}
	return first, nil
}

// countUsers reports how many local accounts exist, for the first-run hint.
func countUsers(ctx context.Context, st store.Store) (int, error) {
	var n int
	err := st.InTx(ctx, func(q store.Queries) error {
		var err error
		n, err = q.CountUsers(ctx)
		return err
	})
	return n, err
}
