package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/krishnanand20/einsora/controlplane/internal/catalog"
	"github.com/krishnanand20/einsora/controlplane/internal/config"
	"github.com/krishnanand20/einsora/controlplane/internal/metering"
	"github.com/krishnanand20/einsora/controlplane/internal/service"
	"github.com/krishnanand20/einsora/controlplane/internal/store"
	"github.com/krishnanand20/einsora/controlplane/internal/webui"
)

// runUp starts Einsora on this machine, the Solo profile (ADR-0012): the API and dashboard, the
// controller with this machine's agent and the access proxy, and usage accounting, in one process,
// on this machine's loopback address, with its data in one private directory. The first time, it
// asks for the owner's account. Every EINSORA_* setting still overrides its default.
func runUp(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	noOpen := fs.Bool("no-open", false, "don't open the dashboard in a browser")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return errors.New("usage: einsora up [-no-open]")
	}
	cfg, dir, err := soloConfig(os.Getenv)
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
	interactive := isTerminal(stdin)
	if err := firstRun(ctx, st, cat, cfg.MachineOwner, stdin, stdout, interactive); err != nil {
		return err
	}
	runController, err := newController(cfg, log, st, cat)
	if err != nil {
		return err
	}
	l, err := listen(cfg.HTTPAddr)
	if err != nil {
		return err
	}
	url := "http://" + l.Addr().String()
	_, _ = fmt.Fprintf(stdout, "\nEinsora %s is running at %s\n  data: %s\n  log:  %s\nPress Ctrl+C to stop.\n\n", version, url, dir, cfg.LogFile)
	log.Info("einsora up", "version", version, "address", url)
	if !webui.Built(webui.Files()) {
		_, _ = fmt.Fprintln(stdout, "This binary was built without its dashboard (make build adds it); the API works.")
	} else if interactive && !*noOpen {
		openBrowser(url)
	}
	return runAll(ctx,
		func(ctx context.Context) error { return serveAPI(ctx, cfg, log, st, cat, l) },
		runController,
		func(ctx context.Context) error { return metering.New(st, log, nil).Run(ctx, cfg.MeteringInterval) },
	)
}

// soloConfig is the configuration for this machine as a Solo install: Solo's defaults for whatever
// EINSORA_* doesn't set, with the data in dataDir. It returns the configuration and the directory.
func soloConfig(getenv func(string) string) (config.Config, string, error) {
	dir, err := dataDir(runtime.GOOS, getenv, os.UserHomeDir)
	if err != nil {
		return config.Config{}, "", err
	}
	if err := privateDir(dir); err != nil {
		return config.Config{}, "", err
	}
	defaults := map[string]string{
		"EINSORA_ENV":             "production",
		"EINSORA_AUTH_MODE":       "local",
		"EINSORA_CLUSTER_BACKEND": "agent",
		"EINSORA_DATABASE_URL":    "sqlite:" + filepath.Join(dir, "einsora.db"),
		"EINSORA_LOG_FILE":        filepath.Join(dir, "einsora.log"),
	}
	cfg, err := config.FromEnv(func(key string) string {
		if v := getenv(key); strings.TrimSpace(v) != "" {
			return v
		}
		return defaults[key]
	})
	return cfg, dir, err
}

// dataDir is where a Solo install keeps its data: EINSORA_DATA_DIR, or the usual place for an
// application's data on this OS, which file-syncing services leave alone.
func dataDir(goos string, getenv func(string) string, home func() (string, error)) (string, error) {
	if d := getenv("EINSORA_DATA_DIR"); d != "" {
		if !filepath.IsAbs(d) {
			return "", fmt.Errorf("EINSORA_DATA_DIR must be an absolute path, got %q", d)
		}
		return filepath.Clean(d), nil
	}
	h, err := home()
	if err != nil || h == "" {
		return "", errors.New("can't find your home directory: set EINSORA_DATA_DIR")
	}
	if goos == "darwin" {
		return filepath.Join(h, "Library", "Application Support", "Einsora"), nil
	}
	if x := getenv("XDG_DATA_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "einsora"), nil
	}
	return filepath.Join(h, ".local", "share", "einsora"), nil
}

// privateDir makes dir, readable by its owner only, and refuses one that others can use: it holds
// the database, the machine's identity, and workloads' data.
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	if !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("data directory %s must be a directory only you can use: run chmod 700 %q", dir, dir)
	}
	return nil
}

// firstRun creates the owner's account the first time, asking for it on the terminal. There's no
// web form to claim an installation with (setup.go): without a terminal, it says how to make one.
func firstRun(ctx context.Context, st store.Store, cat *catalog.Catalog, tenant string, stdin io.Reader, stdout io.Writer, interactive bool) error {
	n, err := countUsers(ctx, st)
	if err != nil || n > 0 {
		return err
	}
	if !interactive {
		_, err := fmt.Fprintln(stdout, "No account yet: create the owner with `einsora setup -email you@example.com`, then sign in.")
		return err
	}
	_, _ = fmt.Fprintln(stdout, "Welcome to Einsora. Create the owner account for this machine.")
	_, _ = fmt.Fprint(stdout, "Email: ")
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	password, err := readPassword(stdin, stdout, false)
	if err != nil {
		return err
	}
	u, err := service.New(st, cat, nil).SetupOwner(ctx, service.SetupInput{
		Email: strings.TrimSpace(line), Password: password, TenantSlug: tenant,
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Created %s. Sign in on the dashboard with it.\n", u.Email)
	return err
}

// listen takes the API's address now, so a port already in use is a clear error before anything
// else starts.
func listen(addr string) (net.Listener, error) {
	l, err := net.Listen("tcp", addr)
	if errors.Is(err, syscall.EADDRINUSE) {
		return nil, fmt.Errorf("something is already listening on %s: is einsora already running? "+
			"(EINSORA_HTTP_ADDR picks another address)", addr)
	}
	return l, err
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && term.IsTerminal(int(f.Fd())) //nolint:gosec // a file descriptor fits in an int
}

// openBrowser opens url in the default browser, if this OS has a way to; it's only a convenience.
func openBrowser(url string) {
	name := map[string]string{"darwin": "open", "linux": "xdg-open"}[runtime.GOOS]
	if name == "" {
		return
	}
	cmd := exec.Command(name, url) //nolint:gosec // a fixed program, given our own loopback URL
	if err := cmd.Start(); err == nil {
		go func() { _ = cmd.Wait() }()
	}
}
