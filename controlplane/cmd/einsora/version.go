package main

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
)

// version is the release this binary is, set when it's built: make build and the release workflow
// pass -ldflags "-X main.version=…" (ADR-0023). A build without it says dev.
var version = "dev"

// versionLine describes this binary: its release, the commit it was built from (and whether that
// had uncommitted changes), its Go version, and its platform.
func versionLine(v string, info *debug.BuildInfo) string {
	commit, modified, goVersion := "unknown commit", "", runtime.Version()
	if info != nil {
		goVersion = info.GoVersion
		for _, s := range info.Settings {
			switch {
			case s.Key == "vcs.revision" && len(s.Value) >= 12:
				commit = "commit " + s.Value[:12]
			case s.Key == "vcs.modified" && s.Value == "true":
				modified = ", modified"
			}
		}
	}
	return fmt.Sprintf("einsora %s (%s%s, %s, %s/%s)", v, commit, modified, goVersion, runtime.GOOS, runtime.GOARCH)
}

func runVersion(stdout io.Writer) error {
	info, _ := debug.ReadBuildInfo()
	_, err := fmt.Fprintln(stdout, versionLine(version, info))
	return err
}
