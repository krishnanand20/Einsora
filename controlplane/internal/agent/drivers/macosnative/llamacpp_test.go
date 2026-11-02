//go:build darwin

package macosnative

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name, link string
	kind       byte
	mode       int64
	body       string
}

func tarball(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: e.kind, Mode: e.mode, Size: int64(len(e.body))}
		if e.kind != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.kind == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// release is laid out as llama.cpp publishes its macOS binaries: one directory of programs, dylibs,
// and links between them.
func release(t *testing.T, extra ...entry) []byte {
	return tarball(t, append([]entry{
		{name: "llama-b1/", kind: tar.TypeDir, mode: 0o755},
		{name: "llama-b1/llama-server", kind: tar.TypeReg, mode: 0o755, body: "server"},
		{name: "llama-b1/libllama.0.5.0.dylib", kind: tar.TypeReg, mode: 0o755, body: "library"},
		{name: "llama-b1/libllama.0.dylib", kind: tar.TypeSymlink, link: "libllama.0.5.0.dylib"},
		{name: "llama-b1/LICENSE", kind: tar.TypeReg, mode: 0o644, body: "MIT"},
	}, extra...)...)
}

func serve(t *testing.T, body []byte) (*httptest.Server, llamaRelease) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(body)
	return srv, llamaRelease{URL: srv.URL + "/llama.tar.gz", SHA256: hex.EncodeToString(sum[:]), Dir: "llama-b1"}
}

func TestInstallLlamaCpp(t *testing.T) {
	srv, rel := serve(t, release(t))
	dir := t.TempDir()
	if err := installLlamaCpp(context.Background(), srv.Client(), rel, dir); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{"llama-server": 0o700, "libllama.0.5.0.dylib": 0o700, "LICENSE": 0o600} {
		fi, err := os.Lstat(filepath.Join(dir, "llama", name))
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v, %v; want a regular file with mode %v", name, fi, err, mode)
		}
	}
	if link, err := os.Readlink(filepath.Join(dir, "llama", "libllama.0.dylib")); err != nil || link != "libllama.0.5.0.dylib" {
		t.Errorf("link = %q, %v", link, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the downloaded archive was left behind: %v", entries)
	}
}

func TestInstallLlamaCppUsesOnlyThePinnedArchive(t *testing.T) {
	srv, rel := serve(t, release(t))
	rel.SHA256 = strings.Repeat("0", 64)
	dir := t.TempDir()
	err := installLlamaCpp(context.Background(), srv.Client(), rel, dir)
	if err == nil || !strings.Contains(err.Error(), "not the pinned") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("an unverified archive was unpacked: %v", entries)
	}
}

func TestUnpackLlamaCppRefusesAnythingButItsOwnFiles(t *testing.T) {
	for name, bad := range map[string]entry{
		"parent":          {name: "llama-b1/../evil", kind: tar.TypeReg, body: "x"},
		"absolute":        {name: "/tmp/evil", kind: tar.TypeReg, body: "x"},
		"other directory": {name: "other/llama-server", kind: tar.TypeReg, body: "x"},
		"subdirectory":    {name: "llama-b1/sub/", kind: tar.TypeDir},
		"nested file":     {name: "llama-b1/sub/x", kind: tar.TypeReg, body: "x"},
		"link out":        {name: "llama-b1/out", kind: tar.TypeSymlink, link: "../out"},
		"absolute link":   {name: "llama-b1/passwd", kind: tar.TypeSymlink, link: "/etc/passwd"},
		"hard link":       {name: "llama-b1/hard", kind: tar.TypeLink, link: "llama-b1/llama-server"},
		"device":          {name: "llama-b1/dev", kind: tar.TypeChar},
		"fifo":            {name: "llama-b1/fifo", kind: tar.TypeFifo},
		"twice":           {name: "llama-b1/LICENSE", kind: tar.TypeReg, body: "again"},
		"control":         {name: "llama-b1/a\nb", kind: tar.TypeReg, body: "x"},
	} {
		t.Run(name, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "llama")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := unpackLlamaCpp(bytes.NewReader(release(t, bad)), "llama-b1", target); err == nil {
				t.Fatal("unpacked")
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(target), "evil")); err == nil {
				t.Fatal("wrote outside its directory")
			}
		})
	}
	target := t.TempDir()
	noServer := tarball(t, entry{name: "llama-b1/llama-cli", kind: tar.TypeReg, mode: 0o755, body: "cli"})
	if err := unpackLlamaCpp(bytes.NewReader(noServer), "llama-b1", target); err == nil || !strings.Contains(err.Error(), "no llama-server") {
		t.Fatalf("err = %v", err)
	}
}

// The pin itself: an HTTPS download from llama.cpp's own releases, at a fixed build, with a full
// SHA-256.
func TestLlamaCppIsPinned(t *testing.T) {
	if !strings.HasPrefix(llamaCpp.URL, "https://github.com/ggml-org/llama.cpp/releases/download/"+strings.TrimPrefix(llamaCpp.Dir, "llama-")+"/"+llamaCpp.Dir+"-bin-macos-arm64.tar.gz") {
		t.Errorf("URL %s doesn't match build %s", llamaCpp.URL, llamaCpp.Dir)
	}
	if sum, err := hex.DecodeString(llamaCpp.SHA256); err != nil || len(sum) != sha256.Size {
		t.Errorf("SHA-256 %q", llamaCpp.SHA256)
	}
}
