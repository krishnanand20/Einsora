package inventory

import (
	"context"
	"runtime"
	"testing"

	"github.com/krishnanand20/einsora/controlplane/internal/domain"
)

func TestParseMeminfo(t *testing.T) {
	mib, ok := parseMeminfo("MemTotal:       65859072 kB\nMemFree:  100 kB\n")
	if !ok || mib != 64315 {
		t.Errorf("parseMeminfo = %d, %v", mib, ok)
	}
	if _, ok := parseMeminfo("MemFree: 100 kB\n"); ok {
		t.Error("no MemTotal must not parse")
	}
}

func TestParseNvidiaSMI(t *testing.T) {
	got := parseNvidiaSMI("NVIDIA L40S, 46068\nNVIDIA L40S, 46068\nNVIDIA GeForce RTX 4090, 24564\ngarbage\n, 12\n")
	want := []domain.Accelerator{
		{Kind: domain.AcceleratorNVIDIA, Model: "NVIDIA L40S", Count: 2, MemoryMiB: 46068},
		{Kind: domain.AcceleratorNVIDIA, Model: "NVIDIA GeForce RTX 4090", Count: 1, MemoryMiB: 24564},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("parseNvidiaSMI = %+v", got)
	}
}

func TestAppleGPU(t *testing.T) {
	if got := appleGPU("Apple M3 Max\x00", 131072); len(got) != 1 || got[0].Model != "Apple M3 Max" || got[0].MemoryMiB != 131072 {
		t.Errorf("appleGPU = %+v", got)
	}
	if got := appleGPU("Intel(R) Core(TM) i9", 32768); got != nil {
		t.Errorf("an Intel Mac has no Apple GPU: %+v", got)
	}
}

// Detect works on the machine running the tests, and keeps headroom for the owner.
func TestDetect(t *testing.T) {
	inv, err := Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("this machine: %+v", inv)
	if inv.OS != runtime.GOOS || inv.CPUs < 1 || inv.UsableCPUs < 1 || inv.UsableCPUs > inv.CPUs ||
		inv.UsableMemoryMiB > inv.MemoryMiB || (runtime.GOOS != "windows" && inv.MemoryMiB <= 0) {
		t.Errorf("Detect = %+v", inv)
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" && (len(inv.Accelerators) != 1 || inv.Accelerators[0].Kind != domain.AcceleratorApple) {
		t.Errorf("an Apple Silicon Mac reports its GPU: %+v", inv.Accelerators)
	}
}
