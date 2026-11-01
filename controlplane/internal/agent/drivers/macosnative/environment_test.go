//go:build darwin

package macosnative

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildLockOutlivesItsParentDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "environment.lock")
	lock, err := lockEnvironment(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(self, "fake-env-lock")
	child.ExtraFiles = []*os.File{lock}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	_ = lock.Close() // exactly what happens to this descriptor if the control plane crashes
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if next, err := lockEnvironment(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		if next != nil {
			_ = next.Close()
		}
		t.Fatalf("orphan build lost its lock: %v", err)
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	next, err := lockEnvironment(ctx, path)
	if err != nil {
		t.Fatalf("finished child kept its build lock: %v", err)
	}
	_ = next.Close()
}
