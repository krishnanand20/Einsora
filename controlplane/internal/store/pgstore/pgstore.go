// Package pgstore is the Postgres Store used outside local dev.
package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver, used only by goose
	"github.com/pressly/goose/v3"

	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
	"github.com/krishnanand20/tensorafi/controlplane/internal/store"
	"github.com/krishnanand20/tensorafi/controlplane/migrations"
)

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) InTx(ctx context.Context, fn func(store.Queries) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return fn(&queries{db: tx}) })
}

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
func (s *Store) Close()                         { s.pool.Close() }

// Migrate applies pending migrations and returns how many ran.
func Migrate(ctx context.Context, url string) (applied int, err error) {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return 0, err
	}
	defer func() {
		if cerr := db.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close migration connection: %w", cerr)
		}
	}()
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations.Postgres)
	if err != nil {
		return 0, fmt.Errorf("load migrations: %w", err)
	}
	results, err := p.Up(ctx)
	return len(results), err
}

type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type queries struct{ db dbtx }

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func mapErr(err error, what string) error {
	switch {
	case errors.Is(err, pgx.ErrNoRows), pgCode(err) == "23503": // foreign_key_violation
		return store.NotFound(what)
	case pgCode(err) == "23505": // unique_violation
		return domain.Errorf(domain.CodeConflict, "%s already exists", what)
	}
	return err
}

// ---- tenants and quotas

const tenantCols = `id, slug, name, tier, created_at`

func scanTenant(row pgx.Row) (domain.Tenant, error) {
	var t domain.Tenant
	var tier string
	err := row.Scan(&t.ID, &t.Slug, &t.Name, &tier, &t.CreatedAt)
	t.Tier = domain.IsolationTier(tier)
	return t, err
}

func (q *queries) CreateTenant(ctx context.Context, t domain.Tenant) error {
	_, err := q.db.Exec(ctx, `INSERT INTO tenants (`+tenantCols+`) VALUES ($1, $2, $3, $4, $5)`,
		t.ID, t.Slug, t.Name, string(t.Tier), t.CreatedAt)
	return mapErr(err, "tenant")
}

func (q *queries) TenantBySlug(ctx context.Context, slug string) (domain.Tenant, error) {
	t, err := scanTenant(q.db.QueryRow(ctx, `SELECT `+tenantCols+` FROM tenants WHERE slug = $1`, slug))
	return t, mapErr(err, "tenant")
}

func (q *queries) TenantByID(ctx context.Context, id uuid.UUID) (domain.Tenant, error) {
	t, err := scanTenant(q.db.QueryRow(ctx, `SELECT `+tenantCols+` FROM tenants WHERE id = $1`, id))
	return t, mapErr(err, "tenant")
}

func (q *queries) SetQuota(ctx context.Context, quota domain.Quota) error {
	_, err := q.db.Exec(ctx, `
		INSERT INTO quotas (tenant_id, gpu_class, max_gpus) VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, gpu_class) DO UPDATE SET max_gpus = EXCLUDED.max_gpus`,
		quota.TenantID, quota.GPUClass, quota.MaxGPUs)
	return mapErr(err, "tenant")
}

func (q *queries) Quotas(ctx context.Context, tenantID uuid.UUID) ([]domain.Quota, error) {
	rows, err := q.db.Query(ctx,
		`SELECT tenant_id, gpu_class, max_gpus FROM quotas WHERE tenant_id = $1 ORDER BY gpu_class`, tenantID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Quota, error) {
		var x domain.Quota
		err := r.Scan(&x.TenantID, &x.GPUClass, &x.MaxGPUs)
		return x, err
	})
}

func (q *queries) LockQuota(ctx context.Context, tenantID uuid.UUID, gpuClass string) (int, error) {
	var limit int
	err := q.db.QueryRow(ctx,
		`SELECT max_gpus FROM quotas WHERE tenant_id = $1 AND gpu_class = $2 FOR UPDATE`,
		tenantID, gpuClass).Scan(&limit)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return limit, err
}

func (q *queries) GPUsHeld(ctx context.Context, tenantID uuid.UUID, gpuClass string) (int, error) {
	var n int
	err := q.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(gpu_count), 0) FROM workloads
		WHERE tenant_id = $1 AND gpu_class = $2 AND state = ANY($3)`,
		tenantID, gpuClass, store.StatesAsStrings(domain.StatesWhere(domain.State.HoldsQuota))).Scan(&n)
	return n, err
}

// nullID stores uuid.Nil as NULL, for optional references such as a workload's machine.
func nullID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

// nilUUID reads a NULL reference back as uuid.Nil.
const nilUUID = `'00000000-0000-0000-0000-000000000000'`

// ---- local accounts

// nullTime stores a zero time as NULL; optionalTime reads NULL back as zero.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func optionalTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

const userCols = `id, tenant_id, email, password_hash, created_at, disabled_at`

func scanUser(row pgx.Row) (domain.User, error) {
	var u domain.User
	var disabled *time.Time
	err := row.Scan(&u.ID, &u.TenantID, &u.Email, &u.PasswordHash, &u.CreatedAt, &disabled)
	u.DisabledAt = optionalTime(disabled)
	return u, err
}

func (q *queries) LockUsers(ctx context.Context) error {
	_, err := q.db.Exec(ctx, `LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE`)
	return err
}

func (q *queries) LockUser(ctx context.Context, id uuid.UUID) error {
	var one int
	err := q.db.QueryRow(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, id).Scan(&one)
	return mapErr(err, "user")
}

func (q *queries) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := q.db.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

func (q *queries) CreateUser(ctx context.Context, u domain.User) error {
	_, err := q.db.Exec(ctx, `INSERT INTO users (`+userCols+`) VALUES ($1, $2, $3, $4, $5, $6)`,
		u.ID, u.TenantID, u.Email, u.PasswordHash, u.CreatedAt, nullTime(u.DisabledAt))
	return mapErr(err, "user")
}

func (q *queries) UserByEmail(ctx context.Context, email string) (domain.User, error) {
	u, err := scanUser(q.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE email = $1`, email))
	return u, mapErr(err, "user")
}

func (q *queries) UserByID(ctx context.Context, id uuid.UUID) (domain.User, error) {
	u, err := scanUser(q.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id))
	return u, mapErr(err, "user")
}

func (q *queries) SetPasswordHash(ctx context.Context, userID uuid.UUID, hash string) error {
	tag, err := q.db.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, userID, hash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.NotFound("user")
	}
	return nil
}

func (q *queries) CreateSession(ctx context.Context, s domain.Session) error {
	_, err := q.db.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES ($1, $2, $3, $4)`,
		s.TokenHash, s.UserID, s.CreatedAt, s.ExpiresAt)
	return mapErr(err, "session")
}

func (q *queries) SessionByTokenHash(ctx context.Context, hash string) (domain.Session, error) {
	var s domain.Session
	err := q.db.QueryRow(ctx, `SELECT token_hash, user_id, created_at, expires_at FROM sessions WHERE token_hash = $1`, hash).
		Scan(&s.TokenHash, &s.UserID, &s.CreatedAt, &s.ExpiresAt)
	return s, mapErr(err, "session")
}

func (q *queries) DeleteSession(ctx context.Context, hash string) error {
	_, err := q.db.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hash)
	return err
}

func (q *queries) DeleteOtherSessions(ctx context.Context, userID uuid.UUID, keepHash string) error {
	_, err := q.db.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1 AND token_hash <> $2`, userID, keepHash)
	return err
}

func (q *queries) DeleteExpiredSessions(ctx context.Context, now time.Time) error {
	_, err := q.db.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= $1`, now)
	return err
}

const tokenCols = `id, user_id, name, prefix, token_hash, created_at, expires_at, last_used_at, revoked_at` //nolint:gosec // column names, not a credential

func scanToken(row pgx.Row) (domain.APIToken, error) {
	var t domain.APIToken
	var expires, lastUsed, revoked *time.Time
	err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &t.TokenHash, &t.CreatedAt, &expires, &lastUsed, &revoked)
	t.ExpiresAt, t.LastUsedAt, t.RevokedAt = optionalTime(expires), optionalTime(lastUsed), optionalTime(revoked)
	return t, err
}

func (q *queries) CreateAPIToken(ctx context.Context, t domain.APIToken) error {
	_, err := q.db.Exec(ctx, `INSERT INTO api_tokens (`+tokenCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		t.ID, t.UserID, t.Name, t.Prefix, t.TokenHash, t.CreatedAt, nullTime(t.ExpiresAt), nullTime(t.LastUsedAt), nullTime(t.RevokedAt))
	return mapErr(err, "token")
}

func (q *queries) APITokenByHash(ctx context.Context, hash string) (domain.APIToken, error) {
	t, err := scanToken(q.db.QueryRow(ctx, `SELECT `+tokenCols+` FROM api_tokens WHERE token_hash = $1`, hash))
	return t, mapErr(err, "token")
}

func (q *queries) TouchAPIToken(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := q.db.Exec(ctx, `UPDATE api_tokens SET last_used_at = $2 WHERE id = $1`, id, at)
	return err
}

func (q *queries) ListAPITokens(ctx context.Context, userID uuid.UUID) ([]domain.APIToken, error) {
	rows, err := q.db.Query(ctx, `SELECT `+tokenCols+` FROM api_tokens WHERE user_id = $1 ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.APIToken, error) { return scanToken(r) })
}

func (q *queries) RevokeAPIToken(ctx context.Context, userID, id uuid.UUID, at time.Time) error {
	tag, err := q.db.Exec(ctx, `UPDATE api_tokens SET revoked_at = COALESCE(revoked_at, $3) WHERE id = $1 AND user_id = $2`, id, userID, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.NotFound("token")
	}
	return nil
}

// ---- machines

const machineCols = `id, tenant_id, name, region, os, arch, cpus, memory_mib, accelerators, sharing, last_seen_at, created_at`

func scanMachine(row pgx.Row) (domain.Machine, error) {
	var m domain.Machine
	var region, sharing string
	var lastSeen *time.Time
	err := row.Scan(&m.ID, &m.TenantID, &m.Name, &region, &m.OS, &m.Arch, &m.CPUs, &m.MemoryMiB, &m.Accelerators, &sharing, &lastSeen, &m.CreatedAt)
	m.Region, m.Sharing = domain.Region(region), domain.SharingLevel(sharing)
	if lastSeen != nil {
		m.LastSeenAt = *lastSeen
	}
	return m, err
}

func (q *queries) CreateMachine(ctx context.Context, m domain.Machine) error {
	accelerators := m.Accelerators
	if accelerators == nil {
		accelerators = []domain.Accelerator{}
	}
	var lastSeen any
	if !m.LastSeenAt.IsZero() {
		lastSeen = m.LastSeenAt
	}
	_, err := q.db.Exec(ctx, `INSERT INTO machines (`+machineCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		m.ID, m.TenantID, m.Name, string(m.Region), m.OS, m.Arch, m.CPUs, m.MemoryMiB, accelerators, string(m.Sharing), lastSeen, m.CreatedAt)
	return mapErr(err, "machine")
}

func (q *queries) MachinesInRegion(ctx context.Context, region domain.Region) ([]domain.Machine, error) {
	rows, err := q.db.Query(ctx, `SELECT `+machineCols+` FROM machines WHERE region = $1 ORDER BY name, id`, string(region))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Machine, error) { return scanMachine(r) })
}

func (q *queries) TakeLease(ctx context.Context, name, holder string, now time.Time, ttl time.Duration) (bool, error) {
	var got string
	err := q.db.QueryRow(ctx, `INSERT INTO leases (name, holder, expires_at) VALUES ($1, $2, $3)
		ON CONFLICT (name) DO UPDATE SET holder = EXCLUDED.holder, expires_at = EXCLUDED.expires_at
		WHERE leases.holder = EXCLUDED.holder OR leases.expires_at <= $4
		RETURNING holder`, name, holder, now.Add(ttl), now).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (q *queries) CreateAccessTicket(ctx context.Context, t domain.AccessTicket) error {
	tag, err := q.db.Exec(ctx, `INSERT INTO access_tickets (ticket_hash, tenant_id, workload_id, subject, expires_at)
		SELECT $1, $2, $3, $4, $5 WHERE EXISTS (SELECT 1 FROM workloads WHERE id = $3 AND tenant_id = $2)`,
		t.Hash, t.TenantID, t.WorkloadID, t.Subject, t.ExpiresAt)
	if err != nil {
		return mapErr(err, "access ticket")
	}
	if tag.RowsAffected() == 0 {
		return store.NotFound("workload")
	}
	return nil
}

func (q *queries) UseAccessTicket(ctx context.Context, hash string, now time.Time) (domain.AccessTicket, error) {
	t := domain.AccessTicket{Hash: hash, UsedAt: now}
	err := q.db.QueryRow(ctx, `UPDATE access_tickets SET used_at = $2
		WHERE ticket_hash = $1 AND used_at IS NULL AND expires_at > $2
		RETURNING tenant_id, workload_id, subject, expires_at`, hash, now).Scan(&t.TenantID, &t.WorkloadID, &t.Subject, &t.ExpiresAt)
	return t, mapErr(err, "access ticket")
}

func (q *queries) DeleteExpiredAccessTickets(ctx context.Context, now time.Time) error {
	_, err := q.db.Exec(ctx, `DELETE FROM access_tickets WHERE expires_at <= $1`, now)
	return err
}

func (q *queries) ReleaseLease(ctx context.Context, name, holder string) error {
	_, err := q.db.Exec(ctx, `DELETE FROM leases WHERE name = $1 AND holder = $2`, name, holder)
	return err
}

func (q *queries) MachineByID(ctx context.Context, id uuid.UUID) (domain.Machine, error) {
	m, err := scanMachine(q.db.QueryRow(ctx, `SELECT `+machineCols+` FROM machines WHERE id = $1`, id))
	return m, mapErr(err, "machine")
}

func (q *queries) UpdateMachineInventory(ctx context.Context, m domain.Machine) error {
	accelerators := m.Accelerators
	if accelerators == nil {
		accelerators = []domain.Accelerator{}
	}
	var lastSeen any
	if !m.LastSeenAt.IsZero() {
		lastSeen = m.LastSeenAt
	}
	tag, err := q.db.Exec(ctx, `UPDATE machines SET os = $2, arch = $3, cpus = $4, memory_mib = $5, accelerators = $6,
		last_seen_at = $7 WHERE id = $1`, m.ID, m.OS, m.Arch, m.CPUs, m.MemoryMiB, accelerators, lastSeen)
	if err != nil {
		return mapErr(err, "machine")
	}
	if tag.RowsAffected() == 0 {
		return store.NotFound("machine")
	}
	return nil
}

func (q *queries) WorkloadsOnMachine(ctx context.Context, machineID uuid.UUID) ([]domain.Workload, error) {
	return q.collectWorkloads(ctx, `SELECT `+workloadSelect+` FROM workloads
		WHERE machine_id = $1 AND state <> 'deleted' ORDER BY created_at, id`, machineID)
}

func (q *queries) GetMachine(ctx context.Context, tenantID, id uuid.UUID) (domain.Machine, error) {
	m, err := scanMachine(q.db.QueryRow(ctx, `SELECT `+machineCols+` FROM machines WHERE id = $1 AND tenant_id = $2`, id, tenantID))
	return m, mapErr(err, "machine")
}

func (q *queries) ListMachines(ctx context.Context, tenantID uuid.UUID) ([]domain.Machine, error) {
	rows, err := q.db.Query(ctx, `SELECT `+machineCols+` FROM machines WHERE tenant_id = $1 ORDER BY name`, tenantID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Machine, error) { return scanMachine(r) })
}

// ---- workloads

const workloadCols = `id, tenant_id, name, kind, state, image_id, gpu_class, gpu_count, params, created_by, created_at, updated_at,
	region, machine_id, placed_class, driver, attempt, fallback, fallback_after_s`

// workloadSelect reads workloadCols, with NULL placement read back as "not placed".
const workloadSelect = `id, tenant_id, name, kind, state, image_id, gpu_class, gpu_count, params, created_by, created_at, updated_at,
	region, COALESCE(machine_id, ` + nilUUID + `), COALESCE(placed_class, ''), COALESCE(driver, ''), attempt, fallback, fallback_after_s`

func scanWorkload(row pgx.Row) (domain.Workload, error) {
	var w domain.Workload
	var kind, state, region string
	var fallbackAfter int64
	err := row.Scan(&w.ID, &w.TenantID, &w.Name, &kind, &state, &w.ImageID, &w.GPUClass,
		&w.GPUCount, &w.Params, &w.CreatedBy, &w.CreatedAt, &w.UpdatedAt,
		&region, &w.MachineID, &w.PlacedClass, &w.Driver, &w.Attempt, &w.Fallback, &fallbackAfter)
	w.Kind, w.State, w.Region = domain.WorkloadKind(kind), domain.State(state), domain.Region(region)
	w.FallbackAfter = time.Duration(fallbackAfter) * time.Second
	return w, err
}

func (q *queries) collectWorkloads(ctx context.Context, sql string, args ...any) ([]domain.Workload, error) {
	rows, err := q.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Workload, error) { return scanWorkload(r) })
}

func (q *queries) CreateWorkload(ctx context.Context, w domain.Workload) error {
	if !w.Region.Valid() {
		return store.InvalidRegion("workload", w.Region)
	}
	params, fallback := w.Params, w.Fallback
	if params == nil {
		params = map[string]string{}
	}
	if fallback == nil {
		fallback = []string{}
	}
	_, err := q.db.Exec(ctx, `INSERT INTO workloads (`+workloadCols+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, NULLIF($15, ''), NULLIF($16, ''), $17, $18, $19)`,
		w.ID, w.TenantID, w.Name, string(w.Kind), string(w.State), w.ImageID, w.GPUClass,
		w.GPUCount, params, w.CreatedBy, w.CreatedAt, w.UpdatedAt,
		string(w.Region), nullID(w.MachineID), w.PlacedClass, w.Driver, w.Attempt, fallback, int64(w.FallbackAfter/time.Second))
	return mapErr(err, "workload")
}

func (q *queries) GetWorkload(ctx context.Context, tenantID, id uuid.UUID) (domain.Workload, error) {
	w, err := scanWorkload(q.db.QueryRow(ctx, `SELECT `+workloadSelect+` FROM workloads
		WHERE id = $1 AND tenant_id = $2 AND state <> 'deleted'`, id, tenantID))
	return w, mapErr(err, "workload")
}

func (q *queries) GetWorkloadHistory(ctx context.Context, tenantID, id uuid.UUID) (domain.Workload, error) {
	w, err := scanWorkload(q.db.QueryRow(ctx, `SELECT `+workloadSelect+` FROM workloads
		WHERE id = $1 AND tenant_id = $2`, id, tenantID))
	return w, mapErr(err, "workload")
}

func (q *queries) ListWorkloads(ctx context.Context, tenantID uuid.UUID) ([]domain.Workload, error) {
	return q.collectWorkloads(ctx, `SELECT `+workloadSelect+` FROM workloads
		WHERE tenant_id = $1 AND state <> 'deleted' ORDER BY created_at, id`, tenantID)
}

func (q *queries) ListWorkloadHistory(ctx context.Context, tenantID uuid.UUID) ([]domain.Workload, error) {
	return q.collectWorkloads(ctx, `SELECT `+workloadSelect+` FROM workloads
		WHERE tenant_id = $1 ORDER BY created_at, id`, tenantID)
}

func (q *queries) UpdateWorkloadState(ctx context.Context, id uuid.UUID, from, to domain.State, at time.Time) error {
	tag, err := q.db.Exec(ctx, `UPDATE workloads SET state = $3, updated_at = $4 WHERE id = $1 AND state = $2`,
		id, string(from), string(to), at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.StaleState(id, from)
	}
	return nil
}

func (q *queries) WorkloadsInStates(ctx context.Context, region domain.Region, states ...domain.State) ([]domain.Workload, error) {
	return q.collectWorkloads(ctx, `SELECT `+workloadSelect+` FROM workloads
		WHERE region = $1 AND state = ANY($2) ORDER BY created_at, id`, string(region), store.StatesAsStrings(states))
}

func (q *queries) SetPlacement(ctx context.Context, id uuid.UUID, placedClass string, machineID uuid.UUID, driver string) error {
	var w domain.Workload
	var region string
	err := q.db.QueryRow(ctx, `SELECT tenant_id, region FROM workloads WHERE id = $1`, id).Scan(&w.TenantID, &region)
	if err != nil {
		return mapErr(err, "workload")
	}
	w.Region = domain.Region(region)
	if machineID == uuid.Nil && driver != "" {
		return domain.ErrDriverWithoutMachine
	}
	if machineID != uuid.Nil {
		// Locked until the transaction ends, so its sharing level can't change between check and use.
		m, err := scanMachine(q.db.QueryRow(ctx, `SELECT `+machineCols+` FROM machines WHERE id = $1 FOR SHARE`, machineID))
		if err != nil {
			return mapErr(err, "machine")
		}
		if err := m.CheckPlacement(w, driver); err != nil {
			return err
		}
	}
	_, err = q.db.Exec(ctx, `UPDATE workloads SET placed_class = NULLIF($2, ''), machine_id = $3, driver = NULLIF($4, ''), attempt = attempt + 1 WHERE id = $1`,
		id, placedClass, nullID(machineID), driver)
	return mapErr(err, "machine")
}

func (q *queries) GPUsAllocated(ctx context.Context, region domain.Region) (map[string]int, error) {
	rows, err := q.db.Query(ctx, `SELECT COALESCE(placed_class, gpu_class), SUM(gpu_count) FROM workloads
		WHERE region = $1 AND state = ANY($2) GROUP BY COALESCE(placed_class, gpu_class)`,
		string(region), store.StatesAsStrings(domain.StatesWhere(domain.State.HoldsCapacity)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var class string
		var n int
		if err := rows.Scan(&class, &n); err != nil {
			return nil, err
		}
		out[class] = n
	}
	return out, rows.Err()
}

func (q *queries) WorkloadsForMetering(ctx context.Context, since time.Time) ([]domain.Workload, error) {
	return q.collectWorkloads(ctx, `SELECT `+workloadSelect+` FROM workloads w
		WHERE state = ANY($1)
		   OR EXISTS (SELECT 1 FROM workload_events e WHERE e.workload_id = w.id AND e.at >= $2)
		ORDER BY created_at, id`,
		store.StatesAsStrings(domain.StatesWhere(domain.State.Metered)), since)
}

// ---- events and usage

func (q *queries) AppendEvent(ctx context.Context, e domain.Event) error {
	_, err := q.db.Exec(ctx, `INSERT INTO workload_events (workload_id, from_state, to_state, reason, at)
		VALUES ($1, NULLIF($2, ''), $3, $4, $5)`,
		e.WorkloadID, string(e.From), string(e.To), e.Reason, e.At)
	return mapErr(err, "workload")
}

func (q *queries) Events(ctx context.Context, workloadID uuid.UUID) ([]domain.Event, error) {
	return q.collectEvents(ctx, `SELECT id, workload_id, COALESCE(from_state, ''), to_state, reason, at
		FROM workload_events WHERE workload_id = $1 ORDER BY at, id`, workloadID)
}

func (q *queries) EventsForWorkload(ctx context.Context, tenantID, workloadID uuid.UUID) ([]domain.Event, error) {
	return q.collectEvents(ctx, `SELECT e.id, e.workload_id, COALESCE(e.from_state, ''), e.to_state, e.reason, e.at
		FROM workload_events e JOIN workloads w ON w.id = e.workload_id AND w.tenant_id = $2
		WHERE e.workload_id = $1 ORDER BY e.at, e.id`, workloadID, tenantID)
}

func (q *queries) collectEvents(ctx context.Context, sql string, args ...any) ([]domain.Event, error) {
	rows, err := q.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Event, error) {
		var e domain.Event
		var from, to string
		err := r.Scan(&e.ID, &e.WorkloadID, &from, &to, &e.Reason, &e.At)
		e.From, e.To = domain.State(from), domain.State(to)
		return e, err
	})
}

func (q *queries) InsertUsageIfAbsent(ctx context.Context, r domain.UsageRecord) (bool, error) {
	if !r.Region.Valid() {
		return false, store.InvalidRegion("usage record", r.Region)
	}
	tag, err := q.db.Exec(ctx, `INSERT INTO usage_records (`+usageCols+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (workload_id, period_start) DO NOTHING`,
		r.WorkloadID, r.PeriodStart, r.TenantID, r.GPUClass, r.GPUCount, r.Seconds, string(r.Region), nullID(r.MachineID))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

const usageCols = `workload_id, period_start, tenant_id, gpu_class, gpu_count, seconds, region, machine_id`

const usageSelect = `workload_id, period_start, tenant_id, gpu_class, gpu_count, seconds, region, COALESCE(machine_id, ` + nilUUID + `)`

func (q *queries) collectUsage(ctx context.Context, sql string, args ...any) ([]domain.UsageRecord, error) {
	rows, err := q.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.UsageRecord, error) {
		var u domain.UsageRecord
		var region string
		err := r.Scan(&u.WorkloadID, &u.PeriodStart, &u.TenantID, &u.GPUClass, &u.GPUCount, &u.Seconds, &region, &u.MachineID)
		u.Region = domain.Region(region)
		return u, err
	})
}

func (q *queries) Usage(ctx context.Context, tenantID uuid.UUID, from, to time.Time) ([]domain.UsageRecord, error) {
	return q.collectUsage(ctx, `SELECT `+usageSelect+` FROM usage_records
		WHERE tenant_id = $1 AND period_start >= $2 AND period_start < $3
		ORDER BY period_start, workload_id`, tenantID, from, to)
}

func (q *queries) UsageForWorkload(ctx context.Context, tenantID, workloadID uuid.UUID) ([]domain.UsageRecord, error) {
	return q.collectUsage(ctx, `SELECT `+usageSelect+` FROM usage_records
		WHERE tenant_id = $1 AND workload_id = $2 ORDER BY period_start`, tenantID, workloadID)
}
