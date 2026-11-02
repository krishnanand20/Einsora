//go:build darwin

package macosnative

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// llamaRuntime is the runtime ref of GGUF endpoints: llama.cpp's server, with the Python worker that
// fetches and validates their models.
const llamaRuntime = "llama-server"

// llamaRelease is a llama.cpp release's macOS arm64 binaries, as published on GitHub: a gzipped tar
// of one directory, Dir, holding the programs, their dylibs, and links between them.
type llamaRelease struct {
	URL, SHA256, Dir string
}

// llamaCpp is the release that serves GGUF endpoints on Macs (ADR-0022): v0.5.0, build b11146. Its
// Metal shaders are built into the dylibs. The archive is checked against this SHA-256 before
// anything in it is used; a new release means a new pin here, and new environments.
var llamaCpp = llamaRelease{
	URL:    "https://github.com/ggml-org/llama.cpp/releases/download/b11146/llama-b11146-bin-macos-arm64.tar.gz",
	SHA256: "1ad3f9eff80edb9dbef4259ad564d1720612ef7eea48fa4afed0e54f5f3d5711",
	Dir:    "llama-b11146",
}

// Bounds on the release archive and what it unpacks to: b11146 is 11 MB, unpacking to 28 MB in 61
// entries.
const (
	maxLlamaArchive  = 256 << 20
	maxLlamaUnpacked = 1 << 30
	maxLlamaEntries  = 1000
)

// installLlamaCpp downloads release into dir/llama. It runs outside the sandbox, so it uses the
// archive only after its checksum matches, and unpacks only regular files and links to files beside
// them, into a directory of its own.
func installLlamaCpp(ctx context.Context, client *http.Client, release llamaRelease, dir string) error {
	archive, err := os.CreateTemp(dir, ".llama-*.tar.gz")
	if err != nil {
		return err
	}
	defer func() { _ = archive.Close(); _ = os.Remove(archive.Name()) }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, release.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download llama.cpp: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download llama.cpp: HTTP %d", resp.StatusCode)
	}
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(archive, sum), io.LimitReader(resp.Body, maxLlamaArchive+1))
	if err != nil {
		return fmt.Errorf("download llama.cpp: %w", err)
	}
	if n > maxLlamaArchive {
		return errors.New("llama.cpp archive is larger than expected")
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != release.SHA256 {
		return fmt.Errorf("llama.cpp archive has SHA-256 %s, not the pinned %s", got, release.SHA256)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	target := filepath.Join(dir, "llama")
	if err := os.Mkdir(target, 0o700); err != nil {
		return err
	}
	return unpackLlamaCpp(archive, release.Dir, target)
}

// unpackLlamaCpp writes the entries of dir in a gzipped tar into target. Anything else (a path
// outside dir, a deeper directory, a link anywhere but beside it, a device) is refused.
func unpackLlamaCpp(r io.Reader, dir, target string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("llama.cpp archive: %w", err)
	}
	tr := tar.NewReader(gz)
	var unpacked int64
	server := false
	for entries := 0; ; entries++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("llama.cpp archive: %w", err)
		}
		if entries >= maxLlamaEntries {
			return errors.New("llama.cpp archive has too many entries")
		}
		if hdr.Typeflag == tar.TypeDir && strings.TrimSuffix(hdr.Name, "/") == dir {
			continue
		}
		name, ok := strings.CutPrefix(hdr.Name, dir+"/")
		if !ok || !plainName(name) {
			return fmt.Errorf("llama.cpp archive: unexpected entry %q", hdr.Name)
		}
		path := filepath.Join(target, name)
		switch hdr.Typeflag {
		case tar.TypeReg:
			if unpacked += hdr.Size; hdr.Size < 0 || unpacked > maxLlamaUnpacked {
				return errors.New("llama.cpp archive unpacks to more than expected")
			}
			mode := os.FileMode(0o600)
			if hdr.Mode&0o111 != 0 {
				mode = 0o700
			}
			if err := writeEntry(path, mode, tr, hdr.Size); err != nil {
				return err
			}
			server = server || name == "llama-server"
		case tar.TypeSymlink:
			if !plainName(hdr.Linkname) {
				return fmt.Errorf("llama.cpp archive: %q links outside its directory", hdr.Name)
			}
			if err := os.Symlink(hdr.Linkname, path); err != nil {
				return err
			}
		default:
			return fmt.Errorf("llama.cpp archive: %q isn't a file or a link", hdr.Name)
		}
	}
	if !server {
		return errors.New("llama.cpp archive has no llama-server")
	}
	return nil
}

// plainName is a file name with no directory in it: never ".", "..", or empty.
func plainName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\") &&
		!strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

func writeEntry(path string, mode os.FileMode, r io.Reader, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) //nolint:gosec // a plain name in the driver's own new directory
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, size))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != size {
		err = errors.New("llama.cpp archive is truncated")
	}
	return err
}
