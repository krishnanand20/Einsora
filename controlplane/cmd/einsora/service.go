package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/krishnanand20/einsora/controlplane/internal/autostart"
)

// serviceEnv is what einsora service uses of this machine, so tests can supply their own: the OS,
// the user, the einsora to run, the environment, and the OS's service manager.
type serviceEnv struct {
	goos       string
	uid        int
	executable func() (string, error)
	getenv     func(string) string
	run        func(ctx context.Context, argv []string) ([]byte, error)
	// addressWait is how long install waits for einsora's address to be free; retryDelay is
	// how long it waits before trying a load command again.
	addressWait, retryDelay time.Duration
}

func osServiceEnv(goos string) serviceEnv {
	return serviceEnv{goos: goos, uid: os.Getuid(), executable: os.Executable, getenv: os.Getenv, addressWait: 5 * time.Second, retryDelay: time.Second,
		run: func(ctx context.Context, argv []string) ([]byte, error) {
			return exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput() //nolint:gosec // fixed launchctl and systemctl commands
		}}
}

type envNames []string

func (e *envNames) String() string     { return strings.Join(*e, ",") }
func (e *envNames) Set(v string) error { *e = append(*e, v); return nil }

const serviceUsage = "usage: einsora service install [-env NAME]... | uninstall | status"

// runService runs `einsora up` in the background, at login and after a crash, or stops doing so,
// or says whether it does (ADR-0023): a launchd agent on macOS, a systemd user service on Linux. It
// changes this machine's service manager only when asked to, and only for this user.
func runService(ctx context.Context, args []string, e serviceEnv, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(serviceUsage)
	}
	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var names envNames
	if args[0] == "install" {
		fs.Var(&names, "env", "copy this environment variable into the service (repeatable)")
	}
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 {
		return errors.New(serviceUsage)
	}
	def, cfgDir, err := serviceDefinition(e, names)
	if err != nil {
		return err
	}
	switch args[0] {
	case "install":
		return installService(ctx, e, def, cfgDir, stdout)
	case "uninstall":
		return uninstallService(ctx, e, def, stdout)
	case "status":
		out, err := e.run(ctx, def.Status)
		if err != nil {
			if _, statErr := os.Stat(def.Path); errors.Is(statErr, os.ErrNotExist) {
				return errors.New("einsora isn't installed as a service (einsora service install sets it up)")
			}
			return fmt.Errorf("einsora is installed as a service (%s), but isn't loaded: %s", def.Path, strings.TrimSpace(string(out)))
		}
		_, err = stdout.Write(out)
		return err
	}
	return errors.New(serviceUsage)
}

// serviceDefinition is the service for this einsora and user: it runs with this shell's PATH, and
// the data directory `einsora up` uses here, named explicitly, so the two never differ.
func serviceDefinition(e serviceEnv, names []string) (autostart.Definition, string, error) {
	binary, err := e.executable()
	if err != nil {
		return autostart.Definition{}, "", err
	}
	if binary, err = filepath.Abs(binary); err != nil {
		return autostart.Definition{}, "", err
	}
	if tmp := filepath.Clean(os.TempDir()); strings.HasPrefix(binary, tmp+string(filepath.Separator)) || strings.Contains(binary, "go-build") {
		return autostart.Definition{}, "", fmt.Errorf("%s is a temporary build: install einsora first, then run einsora service from it", binary)
	}
	_, dir, err := soloConfig(e.getenv)
	if err != nil {
		return autostart.Definition{}, "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return autostart.Definition{}, "", err
	}
	env := []autostart.Var{{Name: "PATH", Value: autostart.SearchPath(e.getenv("PATH"))}, {Name: "EINSORA_DATA_DIR", Value: dir}}
	for _, name := range names {
		value := e.getenv(name)
		if value == "" {
			return autostart.Definition{}, "", fmt.Errorf("-env %s: it isn't set in this shell", name)
		}
		if name == "PATH" || name == "EINSORA_DATA_DIR" {
			return autostart.Definition{}, "", fmt.Errorf("-env %s: the service always gets it from this shell", name)
		}
		env = append(env, autostart.Var{Name: name, Value: value})
	}
	def, err := autostart.Define(autostart.Options{GOOS: e.goos, Home: home, ConfigHome: e.getenv("XDG_CONFIG_HOME"),
		UID: e.uid, Binary: binary, DataDir: dir, Env: env})
	return def, dir, err
}

func installService(ctx context.Context, e serviceEnv, def autostart.Definition, dir string, stdout io.Writer) error {
	for _, argv := range def.Reset {
		_, _ = e.run(ctx, argv) // nothing to stop the first time
	}
	cfg, _, err := soloConfig(e.getenv)
	if err != nil {
		return err
	}
	// A service that can't take its address would fail and be restarted over and over.
	if err := addressFree(ctx, cfg.HTTPAddr, e.addressWait); err != nil {
		return err
	}
	// Readable by this user only: it can hold the values of variables named with -env.
	if err := os.MkdirAll(filepath.Dir(def.Path), 0o700); err != nil {
		return err
	}
	if err := writePrivate(def.Path, def.Content, true); err != nil {
		return err
	}
	for _, argv := range def.Load {
		// launchd can refuse to load a service for a moment after unloading the old copy.
		var out []byte
		var err error
		for try := range 3 {
			if out, err = e.run(ctx, argv); err == nil {
				break
			}
			if try < 2 {
				time.Sleep(e.retryDelay)
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
		}
	}
	_, _ = fmt.Fprintf(stdout, "Einsora runs in the background now, and starts again at every login.\n"+
		"  definition: %s\n  data:       %s\n  output:     %s\nStop it with `einsora service uninstall`.\n", def.Path, dir, def.Logs)
	if e.goos == "linux" {
		_, _ = fmt.Fprintln(stdout, "To keep it running after you log out, and start it at boot: sudo loginctl enable-linger \"$USER\"")
	}
	// The service has no terminal to ask for the owner's account on.
	st, err := openStore(ctx, cfg, slog.New(slog.DiscardHandler), false)
	if err != nil {
		return err
	}
	defer st.Close()
	if n, err := countUsers(ctx, st); err == nil && n == 0 {
		_, _ = fmt.Fprintln(stdout, "No account yet: create the owner with `einsora setup -email you@example.com`, then sign in.")
	}
	return nil
}

func uninstallService(ctx context.Context, e serviceEnv, def autostart.Definition, stdout io.Writer) error {
	if _, err := os.Stat(def.Path); errors.Is(err, os.ErrNotExist) {
		_, err := fmt.Fprintln(stdout, "einsora isn't installed as a service.")
		return err
	}
	for _, argv := range def.Unload {
		_, _ = e.run(ctx, argv) // it may already be stopped
	}
	if err := os.Remove(def.Path); err != nil {
		return err
	}
	for _, argv := range def.Forget {
		if out, err := e.run(ctx, argv); err != nil {
			return fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
		}
	}
	_, err := fmt.Fprintf(stdout, "Stopped einsora's service and removed %s. Running notebooks and endpoints keep running, as when einsora up stops; einsora up finds them again.\n", def.Path)
	return err
}

// addressFree waits up to wait for addr to be free, as it is once a service just stopped has
// exited, and says what to do if something else keeps it.
func addressFree(ctx context.Context, addr string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
		if err == nil {
			return l.Close()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("something is already listening on %s, where the service would: stop einsora up in its terminal (Ctrl+C) first, or set EINSORA_HTTP_ADDR and pass -env EINSORA_HTTP_ADDR", addr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
