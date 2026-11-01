package docker

import (
	"fmt"
	"path/filepath"
	"slices"

	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
)

// Labels prove which requested spec a container belongs to, but not which settings Docker actually
// applied. Check effective limits and isolation before any first start or recovered start.
func (d *Driver) checkProtection(info containerInfo, st state) error {
	h := info.HostConfig
	mem := st.Desired.MemoryMiB * 1024 * 1024
	if info.Config.User != d.user || h.Privileged || h.PidMode != "" || h.IpcMode != "" && h.IpcMode != "private" ||
		h.NetworkMode != d.network(st.Desired.ID) || len(h.PortBindings) > 0 || h.PublishAllPorts || !h.ReadonlyRootfs || !h.Init || h.AutoRemove ||
		h.RestartPolicy["Name"] != "no" || !slices.Contains(h.CapDrop, "ALL") || !slices.Contains(h.SecurityOpt, "no-new-privileges:true") {
		return fmt.Errorf("docker did not apply the requested isolation")
	}
	if h.Memory != mem || h.MemorySwap != mem || h.PidsLimit != 512 || h.NanoCpus != int64(st.Desired.VCPUs)*1_000_000_000 {
		return fmt.Errorf("docker did not apply the requested resource limits")
	}
	expected := map[string]mount{
		"/work":           {Source: filepath.Join(d.dir(st.Desired.ID), "data")},
		"/home/tensorafi": {Source: filepath.Join(d.dir(st.Desired.ID), "home")},
		"/run/tensorafi":  {Source: d.run(st)},
	}
	if st.Desired.Kind == domain.KindEndpoint {
		expected["/opt/tensorafi/runtime.zip"] = mount{Source: d.bundle, ReadOnly: true}
	}
	if len(h.Mounts) != len(expected) {
		return fmt.Errorf("docker mounts differ from this workload's private directories")
	}
	for _, actual := range h.Mounts {
		wanted, ok := expected[actual.Target]
		if !ok || actual.Type != "bind" || actual.Source != wanted.Source || actual.ReadOnly != wanted.ReadOnly {
			return fmt.Errorf("docker mount differs from its requested scope")
		}
	}
	if st.Desired.Accelerator == domain.AcceleratorCPU {
		if h.Runtime != "runc" || len(h.DeviceRequests) != 0 {
			return fmt.Errorf("CPU container received unexpected GPU access")
		}
	} else {
		if h.Runtime != "nvidia" || len(h.DeviceRequests) != 1 || h.DeviceRequests[0].Driver != "nvidia" || !slices.Equal(h.DeviceRequests[0].DeviceIDs, st.Devices) {
			return fmt.Errorf("docker GPU UUIDs differ from the allocation")
		}
	}
	return nil
}
