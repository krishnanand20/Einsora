package docker

import "testing"

func TestNVIDIAInventoryUsesUniquePhysicalUUIDs(t *testing.T) {
	line := "GPU-11111111-2222-3333-4444-555555555555, NVIDIA L40S, 46068\n"
	got := parseNVIDIA(line)
	if len(got) != 1 || got[0].Model != "NVIDIA L40S" || got[0].MemoryMiB != 46068 {
		t.Fatalf("inventory: %+v", got)
	}
	for _, raw := range []string{line + line, "MIG-11111111-2222-3333-4444-555555555555, NVIDIA L40S, 46068", "0, NVIDIA L40S, 46068", "GPU-11111111-2222-3333-4444-555555555555, NVIDIA L40S, -1"} {
		if len(parseNVIDIA(raw)) != 0 {
			t.Errorf("accepted malformed or duplicate physical devices: %q", raw)
		}
	}
}
