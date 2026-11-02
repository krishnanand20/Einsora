// Package protocol is what node agents and the control plane say to each other (ADR-0016): one
// call, Sync. The agent sends what it has; the control plane answers with everything the machine
// should run, as soon as that changes or after a timeout (a long poll). In Solo it's a function
// call to the in-process agent; over the network (Phase 2) it's POST /agent/v1/sync over HTTPS with
// mutual TLS. The types are plain data with JSON tags, so both carry the same messages.
package protocol

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/domain"
)

// Version is the protocol version. The control plane refuses an agent speaking another.
const Version = 1

// Conn is an agent's connection to the control plane.
type Conn interface {
	Sync(ctx context.Context, req SyncRequest) (SyncResponse, error)
}

// SyncRequest is an agent's report.
type SyncRequest struct {
	Version int `json:"version"`
	// Generation is the desired state the agent last received; 0 when it starts. The control plane
	// answers at once when it has a newer one, and otherwise waits for one.
	Generation uint64 `json:"generation"`
	// Applied is the desired state the agent last finished applying. A workload's status from
	// before the desired state that last changed it is stale: the control plane reads it as still
	// in progress, never as the answer (such as an old failure after a restart).
	Applied uint64 `json:"applied"`
	// Inventory is sent when the agent starts, when it changes, and when the control plane asks.
	Inventory *Inventory `json:"inventory,omitempty"`
	// Statuses lists every workload the agent has, and those it refused: complete, every time.
	Statuses []Status `json:"statuses"`
}

// SyncResponse is the machine's complete desired state. A workload listed with ModeRemove is removed
// with its data; one the agent has that isn't listed is stopped and kept, never removed, because a
// list can leave one out only by mistake.
type SyncResponse struct {
	Generation    uint64    `json:"generation"`
	Workloads     []Desired `json:"workloads"`
	SendInventory bool      `json:"send_inventory,omitempty"` // the control plane doesn't have it
}

// Inventory is what an agent reports about its machine. It's bounded and validated by the control
// plane, and never decides the machine's owner, region, sharing, or a driver's isolation.
type Inventory struct {
	AgentVersion string               `json:"agent_version"`
	OS           string               `json:"os"`   // linux or darwin
	Arch         string               `json:"arch"` // amd64 or arm64
	CPUs         int                  `json:"cpus"`
	MemoryMiB    int64                `json:"memory_mib"`
	Accelerators []domain.Accelerator `json:"accelerators"`
	// UsableCPUs and UsableMemoryMiB are what Einsora may allocate; the agent keeps the rest for
	// the machine's owner.
	UsableCPUs      int   `json:"usable_cpus"`
	UsableMemoryMiB int64 `json:"usable_memory_mib"`
	// Drivers are the drivers the agent has, in its order of preference.
	Drivers []Driver `json:"drivers"`
}

// Driver is one of an agent's drivers, and which accelerators it can use on this machine.
type Driver struct {
	Name         string                   `json:"name"`
	Accelerators []domain.AcceleratorKind `json:"accelerators"`
}

// Mode is what a workload should be doing on the machine.
type Mode string

const (
	ModeRun    Mode = "run"
	ModePark   Mode = "park"   // stopped, keeping its disk (VMs and notebooks)
	ModeRemove Mode = "remove" // gone, with everything it stored
)

// Desired is one workload the machine should run, park, or remove.
type Desired struct {
	ID   uuid.UUID `json:"id"`
	Mode Mode      `json:"mode"`
	// Attempt counts the workload's admissions. An agent runs each attempt at most once: one that
	// ended (failed, or stopped by itself) isn't started again until a new attempt.
	Attempt      int64                  `json:"attempt"`
	Kind         domain.WorkloadKind    `json:"kind"`
	Driver       string                 `json:"driver"`
	Accelerator  domain.AcceleratorKind `json:"accelerator"`
	Devices      int                    `json:"devices"`                 // whole GPUs; 0 for CPU classes
	DeviceModels []string               `json:"device_models,omitempty"` // eligible physical GPU models, from the catalog
	VCPUs        int                    `json:"vcpus"`
	MemoryMiB    int64                  `json:"memory_mib"`
	ImageID      string                 `json:"image_id"`
	Runtime      Runtime                `json:"runtime"`
	// ModelURI is an endpoint's model (hf:// or s3://, never with credentials); SSHPublicKey is a
	// VM's login key.
	ModelURI     string `json:"model_uri,omitempty"`
	SSHPublicKey string `json:"ssh_public_key,omitempty"`
	// StrongIsolation is set when the workload isn't the machine owner's: the agent must refuse to
	// run it with a driver whose isolation is weak (the trust matrix).
	StrongIsolation bool `json:"strong_isolation,omitempty"`
}

// Runtime is how the image runs on this machine's accelerator (catalog.Runtime).
type Runtime struct {
	Ref         string `json:"ref"`
	Server      string `json:"server,omitempty"`
	ModelFormat string `json:"model_format,omitempty"`
}

// State is a workload's condition on the machine.
type State string

const (
	StateProgressing State = "progressing"
	StateReady       State = "ready"
	StateStopped     State = "stopped"
	StateFailed      State = "failed"
)

// Status is one workload's state, with a code from a fixed vocabulary. The control plane turns
// codes into the reasons owners read; an agent's own words never reach a timeline (rule 14).
type Status struct {
	ID      uuid.UUID `json:"id"`
	Attempt int64     `json:"attempt"` // the attempt the driver last ran it for
	State   State     `json:"state"`
	Code    Code      `json:"code,omitempty"`
	// Access is where a ready workload's web server listens on its machine, and the token it
	// expects (ADR-0017). The control plane keeps it in memory only, for its access proxy.
	Access *Access `json:"access,omitempty"`
}

// Access is how the access proxy reaches a workload on its machine: its server's Unix socket, never a
// TCP port (ADR-0017), and the token the server expects.
type Access struct {
	Socket string `json:"socket"`
	Token  string `json:"token"`
}

// MaxSocketPath is the longest Unix socket path every OS Einsora runs on accepts: macOS allows 104
// bytes, with the terminating NUL.
const MaxSocketPath = 103

// String and LogValue keep the token out of logs if an Access is ever printed or logged.
func (a Access) String() string { return fmt.Sprintf("Access{Socket:%s}", a.Socket) }

func (a Access) LogValue() slog.Value { return slog.GroupValue(slog.String("socket", a.Socket)) }

// Code says why a workload is in its state.
type Code string

const (
	CodeStarting        Code = "starting"
	CodePreparing       Code = "preparing"         // getting its software ready, which can take minutes the first time
	CodeDevicesBusy     Code = "devices-busy"      // waiting for another workload to free its GPU
	CodeExited          Code = "exited"            // it stopped by itself
	CodeOutOfMemory     Code = "out-of-memory"     //
	CodeNoDriver        Code = "no-driver"         // the agent doesn't have the driver it was given
	CodeIsolation       Code = "isolation"         // the driver isolates less strongly than required
	CodeImage           Code = "image-unavailable" // the image or model couldn't be fetched
	CodeInvalid         Code = "invalid"           // the agent can't run what it was given
	CodeDriverProblem   Code = "driver-problem"    // the driver failed; the agent's log has details
	CodeRemovingFailed  Code = "removing-failed"   // removing it failed; retried
	CodeDiskMissing     Code = "disk-missing"      // a workload that ran here before has no data here now
	CodeSandboxChanged  Code = "sandbox-changed"   // an old native server must stop before using the updated sandbox
	CodeFetchingModel   Code = "fetching-model"
	CodeValidatingModel Code = "validating-model"
	CodeLoadingModel    Code = "loading-model"
	CodeModelRejected   Code = "model-rejected"
	CodeModelNotFound   Code = "model-not-found" // its source has no model, or a GGUF tag matches none or several
	CodePullingImage    Code = "pulling-image"
	CodeRuntimeChanged  Code = "runtime-changed"
)

// Reason is the text an owner reads for code. Unknown codes, which a newer or hostile agent could
// send, get a fixed reason.
func Reason(code Code) string {
	switch code {
	case CodeStarting, "":
		return "starting on its machine"
	case CodePreparing:
		return "preparing its software on its machine; the first time, this downloads a few hundred MB"
	case CodeDevicesBusy:
		return "waiting for its machine's GPU to be free"
	case CodeExited:
		return "it exited unexpectedly"
	case CodeOutOfMemory:
		return "it ran out of memory"
	case CodeNoDriver:
		return "its machine doesn't have the driver it was placed with"
	case CodeIsolation:
		return "its machine refused to run it without strong isolation"
	case CodeImage:
		return "its machine couldn't get the image or model"
	case CodeInvalid:
		return "its machine couldn't run it as specified"
	case CodeDriverProblem:
		return "its machine's driver failed; the machine's log has the details"
	case CodeRemovingFailed:
		return "its machine couldn't remove it yet; retrying"
	case CodeDiskMissing:
		return "its disk isn't on its machine any more"
	case CodeSandboxChanged:
		return "its native sandbox changed; restart the workload to use the updated protections"
	case CodeFetchingModel:
		return "downloading its model to its own machine"
	case CodeValidatingModel:
		return "checking its model against the code-free model policy"
	case CodeLoadingModel:
		return "loading its validated model on its machine"
	case CodeModelRejected:
		return "its model failed the code-free model policy; use safetensors weights without custom code or, for a GGUF endpoint, one GGUF model"
	case CodeModelNotFound:
		return "its model source doesn't name exactly one model; check the repository or S3 prefix and, for a GGUF endpoint, the quantization tag (as in :Q4_K_M)"
	case CodePullingImage:
		return "preparing its container image on its machine; the first time, this downloads the image"
	case CodeRuntimeChanged:
		return "its container runtime policy changed; restart the workload to use the updated protections"
	}
	return "its machine reported a problem"
}

// ProgressReason is the text an owner reads for a workload still in progress with code, or empty
// when there's nothing worth saying (it's starting, stopping, or being removed as usual).
func ProgressReason(code Code) string {
	switch code {
	case "", CodeStarting:
		return ""
	}
	return Reason(code)
}

// FailureReason is the text an owner reads for a workload that failed with code. A failure without
// a known code, which a hostile or buggy agent could send, reads as a problem, never as "starting".
func FailureReason(code Code) string {
	switch code {
	case "", CodeStarting, CodePreparing, CodeDevicesBusy, CodeRemovingFailed, CodeFetchingModel, CodeValidatingModel, CodeLoadingModel, CodePullingImage:
		return Reason("unknown")
	}
	return Reason(code)
}
