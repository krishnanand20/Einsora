// Package memstore is an in-memory Store for `tensorafi dev` and tests. Transactions are
// serialized by one mutex and rolled back by restoring a snapshot.
package memstore

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
	"github.com/krishnanand20/tensorafi/controlplane/internal/store"
)

type Store struct {
	mu sync.Mutex
	d  data
}

type quotaKey struct {
	tenant uuid.UUID
	class  string
}

type usageKey struct {
	workload uuid.UUID
	period   time.Time
}

type data struct {
	tenants     map[uuid.UUID]domain.Tenant
	users       map[uuid.UUID]domain.User
	sessions    map[string]domain.Session
	tokens      map[uuid.UUID]domain.APIToken
	machines    map[uuid.UUID]domain.Machine
	leases      map[string]lease
	tickets     map[string]domain.AccessTicket
	quotas      map[quotaKey]domain.Quota
	workloads   map[uuid.UUID]domain.Workload
	events      []domain.Event
	usage       map[usageKey]domain.UsageRecord
	nextEventID int64
}

func New() *Store {
	return &Store{d: data{
		tenants:   map[uuid.UUID]domain.Tenant{},
		users:     map[uuid.UUID]domain.User{},
		sessions:  map[string]domain.Session{},
		tokens:    map[uuid.UUID]domain.APIToken{},
		machines:  map[uuid.UUID]domain.Machine{},
		leases:    map[string]lease{},
		tickets:   map[string]domain.AccessTicket{},
		quotas:    map[quotaKey]domain.Quota{},
		workloads: map[uuid.UUID]domain.Workload{},
		usage:     map[usageKey]domain.UsageRecord{},
	}}
}

func (s *Store) InTx(_ context.Context, fn func(store.Queries) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := s.d.clone()
	if err := fn(&queries{d: &s.d}); err != nil {
		s.d = snapshot
		return err
	}
	return nil
}

func (s *Store) Ping(context.Context) error { return nil }
func (s *Store) Close()                     {}

func (d *data) clone() data {
	c := *d
	c.tenants = maps.Clone(d.tenants)
	c.users = maps.Clone(d.users)
	c.sessions = maps.Clone(d.sessions)
	c.tokens = maps.Clone(d.tokens)
	c.machines = make(map[uuid.UUID]domain.Machine, len(d.machines))
	for id, m := range d.machines {
		c.machines[id] = cloneMachine(m)
	}
	c.leases = maps.Clone(d.leases)
	c.tickets = maps.Clone(d.tickets)
	c.quotas = maps.Clone(d.quotas)
	c.workloads = make(map[uuid.UUID]domain.Workload, len(d.workloads))
	for id, w := range d.workloads {
		c.workloads[id] = cloneWorkload(w)
	}
	c.events = slices.Clone(d.events)
	c.usage = maps.Clone(d.usage)
	return c
}

func (q *queries) CreateAccessTicket(_ context.Context, t domain.AccessTicket) error {
	w, ok := q.d.workloads[t.WorkloadID]
	if !ok || w.TenantID != t.TenantID {
		return store.NotFound("workload")
	}
	if _, dup := q.d.tickets[t.Hash]; dup {
		return domain.Errorf(domain.CodeConflict, "access ticket already exists")
	}
	q.d.tickets[t.Hash] = t
	return nil
}

func (q *queries) UseAccessTicket(_ context.Context, hash string, now time.Time) (domain.AccessTicket, error) {
	t, ok := q.d.tickets[hash]
	if !ok || !t.UsedAt.IsZero() || !now.Before(t.ExpiresAt) {
		return domain.AccessTicket{}, store.NotFound("access ticket")
	}
	t.UsedAt = now
	q.d.tickets[hash] = t
	return t, nil
}

func (q *queries) DeleteExpiredAccessTickets(_ context.Context, now time.Time) error {
	for hash, t := range q.d.tickets {
		if !now.Before(t.ExpiresAt) {
			delete(q.d.tickets, hash)
		}
	}
	return nil
}

type lease struct {
	holder  string
	expires time.Time
}

func (q *queries) TakeLease(_ context.Context, name, holder string, now time.Time, ttl time.Duration) (bool, error) {
	if l, ok := q.d.leases[name]; ok && l.holder != holder && now.Before(l.expires) {
		return false, nil
	}
	q.d.leases[name] = lease{holder: holder, expires: now.Add(ttl)}
	return true, nil
}

func (q *queries) ReleaseLease(_ context.Context, name, holder string) error {
	if l, ok := q.d.leases[name]; ok && l.holder == holder {
		delete(q.d.leases, name)
	}
	return nil
}

func cloneWorkload(w domain.Workload) domain.Workload {
	w.Params = maps.Clone(w.Params)
	w.Fallback = slices.Clone(w.Fallback)
	return w
}

func cloneMachine(m domain.Machine) domain.Machine {
	m.Accelerators = slices.Clone(m.Accelerators)
	return m
}

func sortWorkloads(ws []domain.Workload) {
	slices.SortFunc(ws, func(a, b domain.Workload) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID.String(), b.ID.String()))
	})
}

type queries struct{ d *data }

func (q *queries) CreateTenant(_ context.Context, t domain.Tenant) error {
	for _, existing := range q.d.tenants {
		if existing.Slug == t.Slug {
			return domain.Errorf(domain.CodeConflict, "tenant %q already exists", t.Slug)
		}
	}
	q.d.tenants[t.ID] = t
	return nil
}

func (q *queries) TenantBySlug(_ context.Context, slug string) (domain.Tenant, error) {
	for _, t := range q.d.tenants {
		if t.Slug == slug {
			return t, nil
		}
	}
	return domain.Tenant{}, store.NotFound("tenant")
}

func (q *queries) TenantByID(_ context.Context, id uuid.UUID) (domain.Tenant, error) {
	if t, ok := q.d.tenants[id]; ok {
		return t, nil
	}
	return domain.Tenant{}, store.NotFound("tenant")
}

func (q *queries) SetQuota(_ context.Context, quota domain.Quota) error {
	if _, ok := q.d.tenants[quota.TenantID]; !ok {
		return store.NotFound("tenant")
	}
	q.d.quotas[quotaKey{quota.TenantID, quota.GPUClass}] = quota
	return nil
}

func (q *queries) Quotas(_ context.Context, tenantID uuid.UUID) ([]domain.Quota, error) {
	var out []domain.Quota
	for k, v := range q.d.quotas {
		if k.tenant == tenantID {
			out = append(out, v)
		}
	}
	slices.SortFunc(out, func(a, b domain.Quota) int { return cmp.Compare(a.GPUClass, b.GPUClass) })
	return out, nil
}

func (q *queries) LockQuota(_ context.Context, tenantID uuid.UUID, gpuClass string) (int, error) {
	return q.d.quotas[quotaKey{tenantID, gpuClass}].MaxGPUs, nil
}

func (q *queries) GPUsHeld(_ context.Context, tenantID uuid.UUID, gpuClass string) (int, error) {
	n := 0
	for _, w := range q.d.workloads {
		if w.TenantID == tenantID && w.GPUClass == gpuClass && w.State.HoldsQuota() {
			n += w.GPUCount
		}
	}
	return n, nil
}

func (q *queries) CountUsers(context.Context) (int, error) { return len(q.d.users), nil }

// LockUsers needs nothing here: transactions already run one at a time.
func (q *queries) LockUsers(context.Context) error { return nil }

// LockUser only checks the user exists: memstore runs one transaction at a time.
func (q *queries) LockUser(_ context.Context, id uuid.UUID) error {
	if _, ok := q.d.users[id]; !ok {
		return store.NotFound("user")
	}
	return nil
}

func (q *queries) CreateUser(_ context.Context, u domain.User) error {
	if _, ok := q.d.tenants[u.TenantID]; !ok {
		return store.NotFound("user")
	}
	for id, existing := range q.d.users {
		if id == u.ID || existing.Email == u.Email {
			return domain.Errorf(domain.CodeConflict, "user already exists")
		}
	}
	q.d.users[u.ID] = u
	return nil
}

func (q *queries) UserByEmail(_ context.Context, email string) (domain.User, error) {
	for _, u := range q.d.users {
		if u.Email == email {
			return u, nil
		}
	}
	return domain.User{}, store.NotFound("user")
}

func (q *queries) UserByID(_ context.Context, id uuid.UUID) (domain.User, error) {
	if u, ok := q.d.users[id]; ok {
		return u, nil
	}
	return domain.User{}, store.NotFound("user")
}

func (q *queries) SetPasswordHash(_ context.Context, userID uuid.UUID, hash string) error {
	u, ok := q.d.users[userID]
	if !ok {
		return store.NotFound("user")
	}
	u.PasswordHash = hash
	q.d.users[userID] = u
	return nil
}

func (q *queries) CreateSession(_ context.Context, s domain.Session) error {
	if _, ok := q.d.users[s.UserID]; !ok {
		return store.NotFound("session")
	}
	if _, ok := q.d.sessions[s.TokenHash]; ok {
		return domain.Errorf(domain.CodeConflict, "session already exists")
	}
	q.d.sessions[s.TokenHash] = s
	return nil
}

func (q *queries) SessionByTokenHash(_ context.Context, hash string) (domain.Session, error) {
	if s, ok := q.d.sessions[hash]; ok {
		return s, nil
	}
	return domain.Session{}, store.NotFound("session")
}

func (q *queries) DeleteSession(_ context.Context, hash string) error {
	delete(q.d.sessions, hash)
	return nil
}

func (q *queries) DeleteOtherSessions(_ context.Context, userID uuid.UUID, keepHash string) error {
	for hash, s := range q.d.sessions {
		if s.UserID == userID && hash != keepHash {
			delete(q.d.sessions, hash)
		}
	}
	return nil
}

func (q *queries) DeleteExpiredSessions(_ context.Context, now time.Time) error {
	for hash, s := range q.d.sessions {
		if !s.ExpiresAt.After(now) {
			delete(q.d.sessions, hash)
		}
	}
	return nil
}

func (q *queries) CreateAPIToken(_ context.Context, t domain.APIToken) error {
	if _, ok := q.d.users[t.UserID]; !ok {
		return store.NotFound("token")
	}
	for id, existing := range q.d.tokens {
		if id == t.ID || existing.TokenHash == t.TokenHash ||
			(existing.UserID == t.UserID && existing.Name == t.Name && existing.RevokedAt.IsZero()) {
			return domain.Errorf(domain.CodeConflict, "token already exists")
		}
	}
	q.d.tokens[t.ID] = t
	return nil
}

func (q *queries) APITokenByHash(_ context.Context, hash string) (domain.APIToken, error) {
	for _, t := range q.d.tokens {
		if t.TokenHash == hash {
			return t, nil
		}
	}
	return domain.APIToken{}, store.NotFound("token")
}

func (q *queries) TouchAPIToken(_ context.Context, id uuid.UUID, at time.Time) error {
	if t, ok := q.d.tokens[id]; ok {
		t.LastUsedAt = at
		q.d.tokens[id] = t
	}
	return nil
}

func (q *queries) ListAPITokens(_ context.Context, userID uuid.UUID) ([]domain.APIToken, error) {
	var out []domain.APIToken
	for _, t := range q.d.tokens {
		if t.UserID == userID {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b domain.APIToken) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID.String(), b.ID.String()))
	})
	return out, nil
}

func (q *queries) RevokeAPIToken(_ context.Context, userID, id uuid.UUID, at time.Time) error {
	t, ok := q.d.tokens[id]
	if !ok || t.UserID != userID {
		return store.NotFound("token")
	}
	if t.RevokedAt.IsZero() {
		t.RevokedAt = at
		q.d.tokens[id] = t
	}
	return nil
}

func (q *queries) CreateMachine(_ context.Context, m domain.Machine) error {
	if _, ok := q.d.tenants[m.TenantID]; !ok {
		return store.NotFound("machine")
	}
	for id, existing := range q.d.machines {
		if id == m.ID || (existing.TenantID == m.TenantID && existing.Name == m.Name) {
			return domain.Errorf(domain.CodeConflict, "machine already exists")
		}
	}
	q.d.machines[m.ID] = cloneMachine(m)
	return nil
}

func (q *queries) GetMachine(_ context.Context, tenantID, id uuid.UUID) (domain.Machine, error) {
	m, ok := q.d.machines[id]
	if !ok || m.TenantID != tenantID {
		return domain.Machine{}, store.NotFound("machine")
	}
	return cloneMachine(m), nil
}

func (q *queries) ListMachines(_ context.Context, tenantID uuid.UUID) ([]domain.Machine, error) {
	var out []domain.Machine
	for _, m := range q.d.machines {
		if m.TenantID == tenantID {
			out = append(out, cloneMachine(m))
		}
	}
	slices.SortFunc(out, func(a, b domain.Machine) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

func (q *queries) MachinesInRegion(_ context.Context, region domain.Region) ([]domain.Machine, error) {
	var out []domain.Machine
	for _, m := range q.d.machines {
		if m.Region == region {
			out = append(out, cloneMachine(m))
		}
	}
	slices.SortFunc(out, func(a, b domain.Machine) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

func (q *queries) MachineByID(_ context.Context, id uuid.UUID) (domain.Machine, error) {
	m, ok := q.d.machines[id]
	if !ok {
		return domain.Machine{}, store.NotFound("machine")
	}
	return cloneMachine(m), nil
}

func (q *queries) UpdateMachineInventory(_ context.Context, m domain.Machine) error {
	cur, ok := q.d.machines[m.ID]
	if !ok {
		return store.NotFound("machine")
	}
	cur.OS, cur.Arch, cur.CPUs, cur.MemoryMiB, cur.LastSeenAt = m.OS, m.Arch, m.CPUs, m.MemoryMiB, m.LastSeenAt
	cur.Accelerators = slices.Clone(m.Accelerators)
	q.d.machines[m.ID] = cur
	return nil
}

func (q *queries) WorkloadsOnMachine(_ context.Context, machineID uuid.UUID) ([]domain.Workload, error) {
	out := []domain.Workload{}
	for _, w := range q.d.workloads {
		if machineID != uuid.Nil && w.MachineID == machineID && w.State != domain.StateDeleted {
			out = append(out, cloneWorkload(w))
		}
	}
	sortWorkloads(out)
	return out, nil
}

func (q *queries) CreateWorkload(_ context.Context, w domain.Workload) error {
	if !w.Region.Valid() {
		return store.InvalidRegion("workload", w.Region)
	}
	if _, ok := q.d.tenants[w.TenantID]; !ok {
		return store.NotFound("tenant")
	}
	if _, ok := q.d.workloads[w.ID]; ok {
		return domain.Errorf(domain.CodeConflict, "workload %s already exists", w.ID)
	}
	if _, ok := q.d.machines[w.MachineID]; w.MachineID != uuid.Nil && !ok {
		return store.NotFound("machine")
	}
	q.d.workloads[w.ID] = cloneWorkload(w)
	return nil
}

func (q *queries) GetWorkload(_ context.Context, tenantID, id uuid.UUID) (domain.Workload, error) {
	w, ok := q.d.workloads[id]
	if !ok || w.TenantID != tenantID || w.State == domain.StateDeleted {
		return domain.Workload{}, store.NotFound("workload")
	}
	return cloneWorkload(w), nil
}

func (q *queries) GetWorkloadHistory(_ context.Context, tenantID, id uuid.UUID) (domain.Workload, error) {
	w, ok := q.d.workloads[id]
	if !ok || w.TenantID != tenantID {
		return domain.Workload{}, store.NotFound("workload")
	}
	return cloneWorkload(w), nil
}

func (q *queries) ListWorkloads(_ context.Context, tenantID uuid.UUID) ([]domain.Workload, error) {
	return q.listWorkloads(tenantID, false), nil
}

func (q *queries) ListWorkloadHistory(_ context.Context, tenantID uuid.UUID) ([]domain.Workload, error) {
	return q.listWorkloads(tenantID, true), nil
}

func (q *queries) listWorkloads(tenantID uuid.UUID, includeDeleted bool) []domain.Workload {
	out := []domain.Workload{}
	for _, w := range q.d.workloads {
		if w.TenantID == tenantID && (includeDeleted || w.State != domain.StateDeleted) {
			out = append(out, cloneWorkload(w))
		}
	}
	sortWorkloads(out)
	return out
}

func (q *queries) UpdateWorkloadState(_ context.Context, id uuid.UUID, from, to domain.State, at time.Time) error {
	w, ok := q.d.workloads[id]
	if !ok {
		return store.NotFound("workload")
	}
	if w.State != from {
		return store.StaleState(id, from)
	}
	w.State, w.UpdatedAt = to, at
	q.d.workloads[id] = w
	return nil
}

func (q *queries) WorkloadsInStates(_ context.Context, region domain.Region, states ...domain.State) ([]domain.Workload, error) {
	var out []domain.Workload
	for _, w := range q.d.workloads {
		if w.Region == region && slices.Contains(states, w.State) {
			out = append(out, cloneWorkload(w))
		}
	}
	sortWorkloads(out)
	return out, nil
}

func (q *queries) GPUsAllocated(_ context.Context, region domain.Region) (map[string]int, error) {
	out := map[string]int{}
	for _, w := range q.d.workloads {
		if w.Region != region || !w.State.HoldsCapacity() {
			continue
		}
		class := w.PlacedClass
		if class == "" {
			class = w.GPUClass
		}
		out[class] += w.GPUCount
	}
	return out, nil
}

func (q *queries) SetPlacement(_ context.Context, id uuid.UUID, placedClass string, machineID uuid.UUID, driver string) error {
	w, ok := q.d.workloads[id]
	if !ok {
		return store.NotFound("workload")
	}
	if machineID == uuid.Nil && driver != "" {
		return domain.ErrDriverWithoutMachine
	}
	if machineID != uuid.Nil {
		m, ok := q.d.machines[machineID]
		if !ok {
			return store.NotFound("machine")
		}
		if err := m.CheckPlacement(w, driver); err != nil {
			return err
		}
	}
	w.PlacedClass, w.MachineID, w.Driver = placedClass, machineID, driver
	w.Attempt++
	q.d.workloads[id] = w
	return nil
}

func (q *queries) WorkloadsForMetering(_ context.Context, since time.Time) ([]domain.Workload, error) {
	recent := map[uuid.UUID]bool{}
	for _, e := range q.d.events {
		if !e.At.Before(since) {
			recent[e.WorkloadID] = true
		}
	}
	var out []domain.Workload
	for _, w := range q.d.workloads {
		if w.State.Metered() || recent[w.ID] {
			out = append(out, cloneWorkload(w))
		}
	}
	sortWorkloads(out)
	return out, nil
}

func (q *queries) AppendEvent(_ context.Context, e domain.Event) error {
	if _, ok := q.d.workloads[e.WorkloadID]; !ok {
		return store.NotFound("workload")
	}
	q.d.nextEventID++
	e.ID = q.d.nextEventID
	q.d.events = append(q.d.events, e)
	return nil
}

func (q *queries) Events(_ context.Context, workloadID uuid.UUID) ([]domain.Event, error) {
	var out []domain.Event
	for _, e := range q.d.events {
		if e.WorkloadID == workloadID {
			out = append(out, e)
		}
	}
	slices.SortStableFunc(out, func(a, b domain.Event) int {
		return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

func (q *queries) EventsForWorkload(ctx context.Context, tenantID, workloadID uuid.UUID) ([]domain.Event, error) {
	if w, ok := q.d.workloads[workloadID]; !ok || w.TenantID != tenantID {
		return nil, nil
	}
	return q.Events(ctx, workloadID)
}

func (q *queries) InsertUsageIfAbsent(_ context.Context, r domain.UsageRecord) (bool, error) {
	if !r.Region.Valid() {
		return false, store.InvalidRegion("usage record", r.Region)
	}
	k := usageKey{r.WorkloadID, r.PeriodStart.UTC()}
	if _, ok := q.d.usage[k]; ok {
		return false, nil
	}
	q.d.usage[k] = r
	return true, nil
}

func (q *queries) UsageForWorkload(_ context.Context, tenantID, workloadID uuid.UUID) ([]domain.UsageRecord, error) {
	var out []domain.UsageRecord
	for _, r := range q.d.usage {
		if r.TenantID == tenantID && r.WorkloadID == workloadID {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b domain.UsageRecord) int { return a.PeriodStart.Compare(b.PeriodStart) })
	return out, nil
}

func (q *queries) Usage(_ context.Context, tenantID uuid.UUID, from, to time.Time) ([]domain.UsageRecord, error) {
	var out []domain.UsageRecord
	for _, r := range q.d.usage {
		if r.TenantID == tenantID && !r.PeriodStart.Before(from) && r.PeriodStart.Before(to) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b domain.UsageRecord) int {
		return cmp.Or(a.PeriodStart.Compare(b.PeriodStart), cmp.Compare(a.WorkloadID.String(), b.WorkloadID.String()))
	})
	return out, nil
}
