// Package store defines persistence for business state. Implementations: memstore (dev and
// tests), sqlitestore (Solo and Lab), and pgstore (Postgres). All must pass the storetest
// conformance suite.
package store

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/domain"
)

type Store interface {
	// InTx runs fn in a transaction: committed if fn returns nil, rolled back otherwise.
	InTx(ctx context.Context, fn func(q Queries) error) error
	Ping(ctx context.Context) error
	Close()
}

// Queries is every operation available inside a transaction.
//
// Tenant-scoped methods take a tenantID and must never return another tenant's rows.
// System-wide methods are for the controller and metering only — never call them from
// request handlers (AGENTS.md rule 8).
type Queries interface {
	// Tenants and quotas
	CreateTenant(ctx context.Context, t domain.Tenant) error
	TenantBySlug(ctx context.Context, slug string) (domain.Tenant, error)
	TenantByID(ctx context.Context, id uuid.UUID) (domain.Tenant, error)
	SetQuota(ctx context.Context, q domain.Quota) error
	Quotas(ctx context.Context, tenantID uuid.UUID) ([]domain.Quota, error)
	// LockQuota returns the tenant's limit for a GPU class (0 if unset). Concurrent creates must
	// serialize: Postgres locks the quota row until the transaction ends, and SQLite and memstore
	// run one transaction at a time.
	LockQuota(ctx context.Context, tenantID uuid.UUID, gpuClass string) (int, error)
	// GPUsHeld sums gpu_count over the tenant's workloads whose state HoldsQuota.
	GPUsHeld(ctx context.Context, tenantID uuid.UUID, gpuClass string) (int, error)

	// Machines, tenant-scoped: the owner's view (ADR-0013).
	CreateMachine(ctx context.Context, m domain.Machine) error
	GetMachine(ctx context.Context, tenantID, id uuid.UUID) (domain.Machine, error)
	ListMachines(ctx context.Context, tenantID uuid.UUID) ([]domain.Machine, error)
	// Machines, system-wide (the controller and the agent backend only, ADR-0016).
	// MachinesInRegion lists every machine in region, by name.
	MachinesInRegion(ctx context.Context, region domain.Region) ([]domain.Machine, error)
	// MachineByID returns one machine, whoever owns it.
	MachineByID(ctx context.Context, id uuid.UUID) (domain.Machine, error)
	// UpdateMachineInventory records what m's agent reports (OS, Arch, CPUs, MemoryMiB,
	// Accelerators) and LastSeenAt, for the machine m.ID. It never changes the owner, name, region,
	// or sharing, which no agent decides.
	UpdateMachineInventory(ctx context.Context, m domain.Machine) error
	// WorkloadsOnMachine returns every workload placed on the machine that isn't deleted: what its
	// agent should be running, parking, or removing.
	WorkloadsOnMachine(ctx context.Context, machineID uuid.UUID) ([]domain.Workload, error)

	// Local accounts (ADR-0012). Sign-in looks a user up before any tenant is known, so these are
	// system-wide: request handlers reach them only through the service's account methods.
	CountUsers(ctx context.Context) (int, error)
	// LockUsers makes "is this the first account?" and creating it atomic: Postgres locks the
	// users table until the transaction ends; SQLite and memstore run one transaction at a time.
	LockUsers(ctx context.Context) error
	CreateUser(ctx context.Context, u domain.User) error
	UserByEmail(ctx context.Context, email string) (domain.User, error)
	UserByID(ctx context.Context, id uuid.UUID) (domain.User, error)
	// LockUser holds one user's row until the transaction ends, so a password check and what
	// depends on it (a new session, a password change) can't interleave with another change:
	// Postgres locks the row; SQLite and memstore run one transaction at a time. Not found if absent.
	LockUser(ctx context.Context, id uuid.UUID) error
	SetPasswordHash(ctx context.Context, userID uuid.UUID, hash string) error
	CreateSession(ctx context.Context, s domain.Session) error
	SessionByTokenHash(ctx context.Context, hash string) (domain.Session, error)
	DeleteSession(ctx context.Context, hash string) error
	// DeleteOtherSessions signs a user out of every session except keepHash ("" means all of them).
	DeleteOtherSessions(ctx context.Context, userID uuid.UUID, keepHash string) error
	DeleteExpiredSessions(ctx context.Context, now time.Time) error
	CreateAPIToken(ctx context.Context, t domain.APIToken) error
	APITokenByHash(ctx context.Context, hash string) (domain.APIToken, error)
	TouchAPIToken(ctx context.Context, id uuid.UUID, at time.Time) error
	// API tokens, user-scoped: a user lists and revokes only their own (rule 8). Revoking another
	// user's token, or one that doesn't exist, is not found; revoking a revoked one is a no-op.
	ListAPITokens(ctx context.Context, userID uuid.UUID) ([]domain.APIToken, error)
	RevokeAPIToken(ctx context.Context, userID, id uuid.UUID, at time.Time) error

	// Workloads, tenant-scoped. Deleted workloads are invisible.
	CreateWorkload(ctx context.Context, w domain.Workload) error
	GetWorkload(ctx context.Context, tenantID, id uuid.UUID) (domain.Workload, error)
	// GetWorkloadHistory is GetWorkload including deleted workloads, so users can still read and
	// export the full history of something they deleted (ADR-0015).
	GetWorkloadHistory(ctx context.Context, tenantID, id uuid.UUID) (domain.Workload, error)
	ListWorkloads(ctx context.Context, tenantID uuid.UUID) ([]domain.Workload, error)
	// ListWorkloadHistory is ListWorkloads including deleted workloads (ADR-0015).
	ListWorkloadHistory(ctx context.Context, tenantID uuid.UUID) ([]domain.Workload, error)
	// EventsForWorkload returns one workload's timeline, oldest first. Another tenant's workload
	// yields no events; callers still check the workload exists first, so they can return 404.
	EventsForWorkload(ctx context.Context, tenantID, workloadID uuid.UUID) ([]domain.Event, error)
	// UpdateWorkloadState is a compare-and-swap: it fails with CodeConflict unless the
	// workload is still in state from. Use service.Transition, not this, directly.
	UpdateWorkloadState(ctx context.Context, id uuid.UUID, from, to domain.State, at time.Time) error

	// Workloads, system-wide and per region (controller / metering only). A controller handles one
	// region (ADR-0009), so it never sees, counts, or admits another region's workloads.
	WorkloadsInStates(ctx context.Context, region domain.Region, states ...domain.State) ([]domain.Workload, error)
	// GPUsAllocated sums gpu_count by class over the region's workloads whose state HoldsCapacity.
	// It counts the placed class, or the requested class for a workload that holds capacity without
	// a recorded placement, so capacity is never under-counted.
	GPUsAllocated(ctx context.Context, region domain.Region) (map[string]int, error)
	// SetPlacement records what a workload was admitted on: its class and, on agents, its machine
	// and driver (uuid.Nil and "" for none), and counts a new attempt. The machine must be allowed to host it with that driver
	// (domain.Machine.CanHost and Permits). Call it in the transaction that moves the workload to
	// provisioning.
	SetPlacement(ctx context.Context, id uuid.UUID, placedClass string, machineID uuid.UUID, driver string) error
	// WorkloadsForMetering returns workloads that are metered now or had an event at or after since.
	WorkloadsForMetering(ctx context.Context, since time.Time) ([]domain.Workload, error)

	// Access tickets (ADR-0017). Creating one is tenant-scoped (its tenant must own the workload);
	// using one is system-wide, by its hash: the access proxy knows nothing else. UseAccessTicket
	// marks it used and returns it, or reports not found for an unknown, used, or expired ticket.
	CreateAccessTicket(ctx context.Context, t domain.AccessTicket) error
	UseAccessTicket(ctx context.Context, hash string, now time.Time) (domain.AccessTicket, error)
	DeleteExpiredAccessTickets(ctx context.Context, now time.Time) error

	// Leases, system-wide (controllers only). TakeLease takes or renews the lease name for holder
	// until now+ttl, and reports whether holder has it: it's free, expired, or already holder's.
	TakeLease(ctx context.Context, name, holder string, now time.Time, ttl time.Duration) (bool, error)
	// ReleaseLease gives up holder's lease name, if holder has it, so a standby needn't wait for it
	// to expire.
	ReleaseLease(ctx context.Context, name, holder string) error

	// Events (append-only) and usage.
	AppendEvent(ctx context.Context, e domain.Event) error
	// Events is system-wide (controller / metering only); request paths use EventsForWorkload.
	Events(ctx context.Context, workloadID uuid.UUID) ([]domain.Event, error)
	// InsertUsageIfAbsent writes r unless a record for (workload, period) exists; reports whether it wrote.
	InsertUsageIfAbsent(ctx context.Context, r domain.UsageRecord) (bool, error)
	Usage(ctx context.Context, tenantID uuid.UUID, from, to time.Time) ([]domain.UsageRecord, error)
	// UsageForWorkload returns one workload's usage records, oldest first (tenant-scoped).
	UsageForWorkload(ctx context.Context, tenantID, workloadID uuid.UUID) ([]domain.UsageRecord, error)

	// Workload output (ADR-0024), system-wide: for the agent backend, which stores what agents
	// ship, and the controller, which removes expired output. OutputState is where one of a
	// workload's streams stands, or not found before its first output; PutOutputState records it.
	OutputState(ctx context.Context, workloadID uuid.UUID, stream domain.OutputStream) (domain.OutputState, error)
	PutOutputState(ctx context.Context, s domain.OutputState) error
	// AppendOutput stores a chunk with the workload's next Seq, which is higher than every Seq the
	// workload had before, even after its output is removed, and returns that Seq.
	AppendOutput(ctx context.Context, c domain.OutputChunk) (int64, error)
	// OldestOutput returns up to limit of a workload's chunks, oldest first.
	OldestOutput(ctx context.Context, workloadID uuid.UUID, limit int) ([]domain.OutputChunk, error)
	// ExpiredOutput returns up to limit chunks received before before, of workloads in region,
	// oldest first.
	ExpiredOutput(ctx context.Context, region domain.Region, before time.Time, limit int) ([]domain.OutputChunk, error)
	DeleteOutput(ctx context.Context, ids []int64) error
	// Workload output, tenant-scoped: up to limit of a workload's chunks with Seq above after,
	// oldest first, and its streams' states. Another tenant's workload yields none; callers check
	// the workload exists first, so they can return 404.
	OutputForWorkload(ctx context.Context, tenantID, workloadID uuid.UUID, after int64, limit int) ([]domain.OutputChunk, error)
	OutputStatesForWorkload(ctx context.Context, tenantID, workloadID uuid.UUID) ([]domain.OutputState, error)
}

func NotFound(what string) error { return domain.Errorf(domain.CodeNotFound, "%s not found", what) }

// InvalidRegion refuses a record without a valid region: every workload and usage record belongs
// to exactly one (rule 9).
func InvalidRegion(what string, r domain.Region) error {
	return domain.Errorf(domain.CodeInvalid, "%s needs a valid region, got %q", what, r)
}

func StaleState(id uuid.UUID, from domain.State) error {
	return domain.Errorf(domain.CodeConflict, "workload %s is no longer %s", id, from)
}

// StatesAsStrings converts states for SQL array parameters.
func StatesAsStrings(states []domain.State) []string {
	out := make([]string, len(states))
	for i, s := range states {
		out[i] = string(s)
	}
	return out
}
