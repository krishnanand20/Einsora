//go:build linux || darwin

package docker

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func privateDirectory(path string, uid int) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("docker state paths must be absolute")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", fmt.Errorf("docker state directory: %w", err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 || int(stat.Uid) != uid {
		return "", fmt.Errorf("docker state directories must be real directories owned by their user with mode 0700")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func makeWorkDirectory(path string, uid, gid int) error {
	if err := os.Mkdir(path, 0o700); err == nil {
		if os.Getuid() == 0 {
			if err := os.Chown(path, uid, gid); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	_, err := privateDirectory(path, uid)
	return err
}

func reserveRun(root string, uid, gid int) (string, error) {
	for range 10 {
		var random [4]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", err
		}
		name := hex.EncodeToString(random[:])
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, 0o700); err == nil {
			if os.Getuid() == 0 {
				if err := os.Chown(path, uid, gid); err != nil {
					return "", err
				}
			}
			return name, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("couldn't reserve a private Docker run directory")
}

func readJSON(path string, out any) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) //nolint:gosec // bounded driver-owned state or status, never follows a symlink
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("docker state must be a regular file")
	}
	return json.NewDecoder(io.LimitReader(f, 1<<16)).Decode(out)
}

func writeJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path+".tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // private driver-owned path
	if err != nil {
		return err
	}
	_, err = f.Write(raw)
	if closed := f.Close(); err == nil {
		err = closed
	}
	if err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func currentUser() (int, int) {
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		return 1000, 1000
	} // containers never run as root, including under a root-owned agent
	return uid, gid
}

func writeArtifact(path string, content []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o444) //nolint:gosec // public source, never credentials
	if errors.Is(err, os.ErrExist) {
		existing, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) //nolint:gosec // immutable source path derived from its checksum
		if err != nil {
			return err
		}
		defer func() { _ = existing.Close() }()
		info, err := existing.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o004 == 0 {
			return fmt.Errorf("runtime source must be a readable regular file")
		}
		data, err := io.ReadAll(io.LimitReader(existing, int64(len(content))+1))
		if err != nil {
			return err
		}
		if !bytes.Equal(data, content) {
			return fmt.Errorf("runtime source does not match its checksum")
		}
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.Write(content)
	if closed := f.Close(); err == nil {
		err = closed
	}
	return err
}
