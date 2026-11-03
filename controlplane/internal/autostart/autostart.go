// Package autostart defines how `einsora up` runs in the background, starting at login and again
// after a crash (ADR-0023): a launchd agent on macOS, which runs in the user's session, where Metal
// is, and a systemd user service on Linux. Definitions are built by a pure function and tested as
// data; the einsora service command writes them and runs the OS's service manager.
package autostart

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Label names the service to launchd; Unit names it to systemd.
const (
	Label = "com.einsora.up"
	Unit  = "einsora.service"
)

// Options describe the einsora to run, as whom, and with what.
type Options struct {
	GOOS string
	// Home is the user's home directory; ConfigHome is XDG_CONFIG_HOME, if it's set (Linux).
	Home, ConfigHome string
	UID              int
	// Binary is the absolute path of the einsora to run. DataDir is its data directory, where its
	// output goes on macOS.
	Binary, DataDir string
	// Env is the environment it runs with: PATH, and any variables the user named. Values may be
	// secrets, so the definition is readable by its owner only.
	Env []Var
}

// Var is one environment variable.
type Var struct{ Name, Value string }

// Definition is a service: its file, and the commands that start, stop, and describe it.
type Definition struct {
	Path    string
	Content []byte
	// Reset stops a copy that's already loaded, so installing again picks up a new definition.
	// Its commands fail when there's none; that's ignored.
	Reset [][]string
	// Load starts it now, and at every login from then on.
	Load [][]string
	// Unload stops it and keeps it from starting again; Forget runs once its file is gone.
	Unload, Forget [][]string
	Status         []string
	// Logs says where its output goes.
	Logs string
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Define builds the definition for o.GOOS.
func Define(o Options) (Definition, error) {
	if !filepath.IsAbs(o.Binary) || !filepath.IsAbs(o.Home) || !filepath.IsAbs(o.DataDir) {
		return Definition{}, errors.New("einsora, the home directory, and the data directory need absolute paths")
	}
	for _, s := range []string{o.Binary, o.DataDir} {
		if !safe(s) {
			return Definition{}, fmt.Errorf("can't use the path %q in a service", s)
		}
	}
	seen := map[string]bool{}
	for _, v := range o.Env {
		if !envName.MatchString(v.Name) || seen[v.Name] || !safe(v.Value) {
			return Definition{}, fmt.Errorf("can't use the environment variable %q in a service", v.Name)
		}
		seen[v.Name] = true
	}
	switch o.GOOS {
	case "darwin":
		return launchd(o), nil
	case "linux":
		return systemd(o), nil
	}
	return Definition{}, fmt.Errorf("einsora service runs on macOS and Linux, not %s", o.GOOS)
}

// safe reports whether s can go into a service definition: valid text without control characters.
func safe(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// launchd is a launch agent in the user's GUI session: it starts at login, and again if einsora
// exits with an error (KeepAlive), but not after a clean stop. Its output goes to the data
// directory, which only the user can read.
func launchd(o Options) Definition {
	domain := "gui/" + strconv.Itoa(o.UID)
	logs := filepath.Join(o.DataDir, "service.log")
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	key := func(k string) { b.WriteString("\t<key>" + k + "</key>\n") }
	str := func(indent, s string) {
		b.WriteString(indent + "<string>")
		_ = xml.EscapeText(&b, []byte(s))
		b.WriteString("</string>\n")
	}
	key("Label")
	str("\t", Label)
	key("ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, a := range []string{o.Binary, "up", "-no-open"} {
		str("\t\t", a)
	}
	b.WriteString("\t</array>\n")
	if len(o.Env) > 0 {
		key("EnvironmentVariables")
		b.WriteString("\t<dict>\n")
		for _, v := range o.Env {
			b.WriteString("\t\t<key>")
			_ = xml.EscapeText(&b, []byte(v.Name))
			b.WriteString("</key>\n")
			str("\t\t", v.Value)
		}
		b.WriteString("\t</dict>\n")
	}
	key("RunAtLoad")
	b.WriteString("\t<true/>\n")
	key("KeepAlive")
	b.WriteString("\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	key("StandardOutPath")
	str("\t", logs)
	key("StandardErrorPath")
	str("\t", logs)
	b.WriteString("</dict>\n</plist>\n")
	plist := filepath.Join(o.Home, "Library", "LaunchAgents", Label+".plist")
	return Definition{
		Path: plist, Content: b.Bytes(),
		Reset:  [][]string{{"launchctl", "bootout", domain + "/" + Label}},
		Load:   [][]string{{"launchctl", "enable", domain + "/" + Label}, {"launchctl", "bootstrap", domain, plist}},
		Unload: [][]string{{"launchctl", "bootout", domain + "/" + Label}},
		Status: []string{"launchctl", "print", domain + "/" + Label},
		Logs:   logs + ", and the complete log, einsora.log, beside it",
	}
}

// systemd is a user service: it starts when the user's service manager does (at login, or at boot
// with lingering), and again if einsora fails. Its output goes to the journal. Docker's
// containers, the only workloads on Linux, aren't in its cgroup, so stopping it leaves them running,
// as stopping `einsora up` does.
func systemd(o Options) Definition {
	config := o.ConfigHome
	if !filepath.IsAbs(config) {
		config = filepath.Join(o.Home, ".config")
	}
	unit := Unit
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=Einsora: a self-hosted GPU cloud on this machine (einsora up)\n")
	b.WriteString("Documentation=https://einsora.com/runbooks/00-quickstart/\n\n[Service]\nType=exec\n")
	b.WriteString("ExecStart=" + strings.Join([]string{systemdQuote(o.Binary, true), "up", "-no-open"}, " ") + "\n")
	for _, v := range o.Env {
		b.WriteString("Environment=" + systemdQuote(v.Name+"="+v.Value, false) + "\n")
	}
	b.WriteString("Restart=on-failure\nRestartSec=10\n\n[Install]\nWantedBy=default.target\n")
	return Definition{
		Path: filepath.Join(config, "systemd", "user", unit), Content: []byte(b.String()),
		Load: [][]string{
			{"systemctl", "--user", "daemon-reload"},
			{"systemctl", "--user", "enable", unit},
			{"systemctl", "--user", "restart", unit},
		},
		Unload: [][]string{{"systemctl", "--user", "disable", "--now", unit}},
		Forget: [][]string{{"systemctl", "--user", "daemon-reload"}},
		Status: []string{"systemctl", "--user", "status", "--no-pager", unit},
		Logs:   "journalctl --user -u " + unit + ", and the complete log, einsora.log, in the data directory",
	}
}

// systemdQuote quotes s as one word for systemd: within double quotes, with backslashes and quotes
// escaped and % doubled (specifiers are expanded everywhere). In a command line, $ is doubled too:
// systemd expands variables there, but not in Environment=.
func systemdQuote(s string, command bool) string {
	pairs := []string{`\`, `\\`, `"`, `\"`, "%", "%%"}
	if command {
		pairs = append(pairs, "$", "$$")
	}
	return `"` + strings.NewReplacer(pairs...).Replace(s) + `"`
}

// SearchPath is a PATH for the service: the absolute directories of path, in order and once each.
// launchd and systemd start services with a minimal PATH, without what einsora finds there in a
// terminal (uv for the macos-native driver; docker on Linux).
func SearchPath(path string) string {
	var dirs []string
	for _, d := range filepath.SplitList(path) {
		if filepath.IsAbs(d) && safe(d) && !slices.Contains(dirs, filepath.Clean(d)) {
			dirs = append(dirs, filepath.Clean(d))
		}
	}
	return strings.Join(dirs, string(filepath.ListSeparator))
}
