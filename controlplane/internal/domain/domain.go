// Package domain holds Einsora's core types and rules. It depends on nothing else in the
// module, so storage, HTTP, Kubernetes, and metering code can all depend on it.
package domain

import (
	"regexp"
	"time"

	"github.com/google/uuid"
)

// WorkloadKind is what a user launches.
type WorkloadKind string

const (
	KindVM       WorkloadKind = "vm"       // a full VM: KubeVirt, libvirt, or the macOS Virtualization framework
	KindNotebook WorkloadKind = "notebook" // JupyterLab: in a container, a Kata sandbox, or natively on a Mac
	KindEndpoint WorkloadKind = "endpoint" // OpenAI-compatible model server: vLLM, llama.cpp, or MLX
)

func (k WorkloadKind) Valid() bool {
	switch k {
	case KindVM, KindNotebook, KindEndpoint:
		return true
	}
	return false
}

// IsolationTier decides which runtimes a tenant's code may use.
type IsolationTier string

const (
	// TierTrusted is a machine's owner or the operator's own team: weak isolation (runc, native
	// macOS) is allowed.
	TierTrusted IsolationTier = "trusted"
	// TierUntrusted is every invited or community user: strong isolation only (VMs, Kata, or
	// Linux containers in their own VM), per the trust matrix in tenancy-and-isolation.md.
	TierUntrusted IsolationTier = "untrusted"
)

func (t IsolationTier) Valid() bool { return t == TierTrusted || t == TierUntrusted }

// slugPattern keeps tenant slugs and workload names safe to use as DNS labels.
var slugPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$`)

// ValidSlug reports whether s is 1-40 lowercase alphanumerics or '-', not starting or ending with '-'.
func ValidSlug(s string) bool { return slugPattern.MatchString(s) }

type Tenant struct {
	ID        uuid.UUID
	Slug      string
	Name      string
	Tier      IsolationTier
	CreatedAt time.Time
}

// Quota caps how many GPUs of one class a tenant may hold at once.
type Quota struct {
	TenantID uuid.UUID
	GPUClass string
	MaxGPUs  int
}

type Workload struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Name      string
	Kind      WorkloadKind
	State     State
	ImageID   string
	GPUClass  string // the class the user requested; PlacedClass is what it was admitted on
	GPUCount  int
	Params    map[string]string // kind-specific inputs, keys below
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time // also when the current state was entered

	// Placement (ADR-0009, ADR-0010).
	Region        Region        // fixed at creation; the workload's data never leaves it (rule 9)
	PlacedClass   string        // the class it was admitted on; empty until first admitted
	MachineID     uuid.UUID     // the agent machine it runs on; uuid.Nil until placed on one
	Driver        string        // the agent driver that runs it (ADR-0016); empty on Kubernetes
	Attempt       int64         // how many times it's been admitted; each admission is a new attempt (ADR-0016)
	Fallback      []string      // other classes, or "cpu", the user accepts, in order; empty means wait
	FallbackAfter time.Duration // how long to wait for GPUClass before trying Fallback
}

// Keys used in Workload.Params.
const (
	ParamSSHPublicKey = "ssh_public_key" // vm
	ParamModelURI     = "model_uri"      // endpoint
)

// Event is one append-only row in workload_events. Metering is computed from these.
type Event struct {
	ID         int64
	WorkloadID uuid.UUID
	From       State // empty for the creation event
	To         State
	Reason     string
	At         time.Time
}

// OutputStream is where a piece of a workload's output came from (ADR-0024): its stdout or stderr,
// or Einsora itself, for a note in the output such as output that was lost.
type OutputStream string

const (
	StreamStdout  OutputStream = "stdout"
	StreamStderr  OutputStream = "stderr"
	StreamEinsora OutputStream = "einsora"
)

func (s OutputStream) Valid() bool {
	return s == StreamStdout || s == StreamStderr || s == StreamEinsora
}

// OutputChunk is a piece of a workload's output as the control plane stored it.
type OutputChunk struct {
	ID int64 // the store's own, across all output: never shown to readers
	// Seq counts the workload's own output from 1, and is never used twice: readers keep their
	// place by it. A count across all workloads would show how much output other people's produce.
	Seq        int64
	WorkloadID uuid.UUID
	Stream     OutputStream
	At         time.Time // when the control plane received it
	Text       string    // valid UTF-8, with credentials removed
	Redactions int       // how many credentials were removed from it
}

// OutputState is where one of a workload's streams stands: how far it's stored, and how much of it
// was never received or has been removed, so readers can be told (ADR-0024).
type OutputState struct {
	WorkloadID uuid.UUID
	Stream     OutputStream
	// Source is the agent's identity for the stream; the stream's offsets start over when it changes.
	Source string
	// Next is the offset in the stream up to which output has been received.
	Next         int64
	StoredBytes  int64 // kept now
	LostBytes    int64 // never received: gone from the machine before it was sent
	TrimmedBytes int64 // removed to keep the workload within its size limit
	ExpiredBytes int64 // removed after the retention period
}

// UsageRecord is the metered time of one workload within one UTC clock hour (usage accounting, never money).
type UsageRecord struct {
	WorkloadID  uuid.UUID
	PeriodStart time.Time
	TenantID    uuid.UUID
	GPUClass    string // the class that actually ran (the workload's placed class, ADR-0010)
	GPUCount    int
	Seconds     int64
	Region      Region
	MachineID   uuid.UUID // uuid.Nil when it didn't run on an agent machine
}

// GPUSeconds is the quantity used for quotas, fair share, and reports.
func (u UsageRecord) GPUSeconds() int64 { return u.Seconds * int64(u.GPUCount) }

// Principal is an authenticated caller.
type Principal struct {
	Subject    string
	Email      string
	TenantSlug string
	UserID     uuid.UUID // the local account (local auth mode); uuid.Nil for dev and OIDC callers
	// Via says how the caller authenticated (AuthSession, AuthToken, …), and Credential names the
	// credential, such as `API token "laptop" (tsk_ab12)`. Timelines record both, so after a leak
	// the owner can tell which actions the leaked credential took.
	Via        AuthMethod
	Credential string
}

// AuthMethod is how a caller authenticated.
type AuthMethod string

const (
	AuthSession AuthMethod = "session" // a signed-in browser (local accounts)
	AuthToken   AuthMethod = "token"   // an API token (local accounts)
	AuthOIDC    AuthMethod = "oidc"
	AuthDev     AuthMethod = "dev"
)

// Actor is who did something, for timeline reasons: the subject, and the credential when known.
func (p Principal) Actor() string {
	if p.Credential == "" {
		return p.Subject
	}
	return p.Subject + " (" + p.Credential + ")"
}
