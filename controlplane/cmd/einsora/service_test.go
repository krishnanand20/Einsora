package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

// fakeServices records the commands einsora service runs, and fails the ones named in fail.
type fakeServices struct {
	ran  [][]string
	fail map[string]string // first two words → output
}

func (f *fakeServices) run(_ context.Context, argv []string) ([]byte, error) {
	f.ran = append(f.ran, argv)
	if out, ok := f.fail[strings.Join(argv[:2], " ")]; ok {
		return []byte(out), errors.New("exit status 1")
	}
	return []byte("state = running\n"), nil
}

func serviceTestEnv(t *testing.T, goos string, vars map[string]string) (serviceEnv, *fakeServices, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	vars["PATH"] = "/opt/homebrew/bin:relative:/usr/bin"
	if vars["EINSORA_HTTP_ADDR"] == "" {
		vars["EINSORA_HTTP_ADDR"] = "127.0.0.1:0" // always free: never this machine's own einsora
	}
	f := &fakeServices{fail: map[string]string{}}
	return serviceEnv{goos: goos, uid: 501, executable: func() (string, error) { return "/usr/local/bin/einsora", nil },
		getenv: func(k string) string { return vars[k] }, run: f.run, addressWait: 300 * time.Millisecond}, f, home
}

func TestServiceInstall(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			e, f, home := serviceTestEnv(t, goos, map[string]string{"HF_TOKEN": "hf_not_a_real_token"})
			var out bytes.Buffer
			if err := runService(context.Background(), []string{"install", "-env", "HF_TOKEN"}, e, &out); err != nil {
				t.Fatal(err)
			}
			_, dir, err := soloConfig(e.getenv)
			if err != nil {
				t.Fatal(err)
			}
			def, _, err := serviceDefinition(e, []string{"HF_TOKEN"})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(def.Path, home) {
				t.Fatalf("definition outside the user's home: %s", def.Path)
			}
			fi, err := os.Stat(def.Path)
			if err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("definition %s: %v, %v; want mode 0600 (it can hold secrets)", def.Path, fi, err)
			}
			content, _ := os.ReadFile(def.Path)
			for _, want := range []string{"/usr/local/bin/einsora", "/opt/homebrew/bin:/usr/bin", dir, "hf_not_a_real_token"} {
				if !strings.Contains(string(content), want) {
					t.Errorf("definition lacks %q:\n%s", want, content)
				}
			}
			if strings.Contains(string(content), "relative") {
				t.Error("a relative PATH entry reached the service")
			}
			// The old copy is stopped first, then the new one loaded.
			want := len(def.Reset) + len(def.Load)
			if len(f.ran) != want || strings.Join(f.ran[len(f.ran)-1], " ") != strings.Join(def.Load[len(def.Load)-1], " ") {
				t.Errorf("ran %v", f.ran)
			}
			if !strings.Contains(out.String(), "No account yet") || !strings.Contains(out.String(), def.Path) {
				t.Errorf("output:\n%s", out.String())
			}
			if goos == "linux" && !strings.Contains(out.String(), "enable-linger") {
				t.Error("no word on lingering")
			}
		})
	}
}

func TestServiceInstallRefusals(t *testing.T) {
	for name, tt := range map[string]struct {
		args   []string
		binary string
		err    string
	}{
		"unset variable":    {[]string{"install", "-env", "HF_TOKEN"}, "/usr/local/bin/einsora", "isn't set"},
		"PATH again":        {[]string{"install", "-env", "PATH"}, "/usr/local/bin/einsora", "always gets it"},
		"temporary build":   {[]string{"install"}, filepath.Join(os.TempDir(), "go-build123", "einsora"), "temporary build"},
		"unknown":           {[]string{"start"}, "/usr/local/bin/einsora", "usage"},
		"stray argument":    {[]string{"status", "now"}, "/usr/local/bin/einsora", "usage"},
		"-env on status":    {[]string{"status", "-env", "X"}, "/usr/local/bin/einsora", "usage"},
		"no subcommand":     {nil, "/usr/local/bin/einsora", "usage"},
		"unsupported OS":    {[]string{"install"}, "/usr/local/bin/einsora", "macOS and Linux"},
		"bad variable name": {[]string{"install", "-env", "A-B"}, "/usr/local/bin/einsora", "isn't set"},
	} {
		t.Run(name, func(t *testing.T) {
			goos := "darwin"
			if name == "unsupported OS" {
				goos = "windows"
			}
			e, f, _ := serviceTestEnv(t, goos, map[string]string{})
			e.executable = func() (string, error) { return tt.binary, nil }
			err := runService(context.Background(), tt.args, e, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Fatalf("err = %v, want %q", err, tt.err)
			}
			if len(f.ran) != 0 {
				t.Errorf("ran %v anyway", f.ran)
			}
		})
	}
}

func TestServiceUninstallAndStatus(t *testing.T) {
	e, f, _ := serviceTestEnv(t, "linux", map[string]string{})
	ctx := context.Background()
	var out bytes.Buffer
	f.fail["systemctl --user"] = "Unit einsora.service could not be found."
	if err := runService(ctx, []string{"status"}, e, &out); err == nil || !strings.Contains(err.Error(), "isn't installed") {
		t.Fatalf("status before install: %v", err)
	}
	if err := runService(ctx, []string{"uninstall"}, e, &out); err != nil || !strings.Contains(out.String(), "isn't installed") || len(f.ran) != 1 {
		t.Fatalf("uninstall before install: %v, %q, ran %v", err, out.String(), f.ran)
	}
	delete(f.fail, "systemctl --user")
	if err := runService(ctx, []string{"install"}, e, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runService(ctx, []string{"status"}, e, &out); err != nil || out.String() != "state = running\n" {
		t.Fatalf("status: %v, %q", err, out.String())
	}
	def, _, _ := serviceDefinition(e, nil)
	f.ran = nil
	if err := runService(ctx, []string{"uninstall"}, e, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(def.Path); !errors.Is(err, os.ErrNotExist) {
		t.Error("the unit file survived uninstall")
	}
	if len(f.ran) != len(def.Unload)+len(def.Forget) {
		t.Errorf("ran %v", f.ran)
	}
	f.fail["systemctl --user"] = "inactive"
	if err := runService(ctx, []string{"status"}, e, &out); err == nil || !strings.Contains(err.Error(), "isn't installed") {
		t.Errorf("status after uninstall: %v", err)
	}
}

// A service that can't take its address would crash and be restarted forever: install refuses.
func TestServiceInstallRefusesATakenAddress(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	e, f, _ := serviceTestEnv(t, "darwin", map[string]string{"EINSORA_HTTP_ADDR": l.Addr().String()})
	err = runService(context.Background(), []string{"install"}, e, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("err = %v", err)
	}
	def, _, _ := serviceDefinition(e, nil)
	if _, statErr := os.Stat(def.Path); statErr == nil {
		t.Error("wrote the definition anyway")
	}
	for _, argv := range f.ran {
		if argv[1] == "bootstrap" {
			t.Error("loaded the service anyway")
		}
	}
}

func TestServiceLoadFailureIsReported(t *testing.T) {
	e, f, _ := serviceTestEnv(t, "darwin", map[string]string{})
	f.fail["launchctl bootstrap"] = "Bootstrap failed: 5: Input/output error"
	err := runService(context.Background(), []string{"install"}, e, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "Input/output error") {
		t.Fatalf("err = %v", err)
	}
	tries := 0
	for _, argv := range f.ran {
		if argv[1] == "bootstrap" {
			tries++
		}
	}
	if tries != 3 {
		t.Errorf("tried bootstrap %d times, want 3", tries)
	}
}

func TestVersionLine(t *testing.T) {
	info := &debug.BuildInfo{GoVersion: "go1.27.0", Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "b602335a1f0e9c2d7e4a8b6c5d3e2f1a0b9c8d7e"}, {Key: "vcs.modified", Value: "true"},
	}}
	got := versionLine("0.1.0", info)
	if !strings.HasPrefix(got, "einsora 0.1.0 (commit b602335a1f0e, modified, go1.27.0, ") {
		t.Errorf("version = %q", got)
	}
	if got := versionLine("dev", nil); !strings.HasPrefix(got, "einsora dev (unknown commit, go") {
		t.Errorf("version without build info = %q", got)
	}
}
