// Package sqlitestore is the SQLite Store for the Solo and Lab profiles (ADR-0012). It uses a
// pure-Go driver (modernc.org/sqlite), so the einsora binary stays CGO-free and cross-compiles
// for every platform (rule 11).
//
// All access goes through one connection, so a process runs one transaction at a time: the same
// guarantee memstore's mutex gives, and what Postgres's row lock in LockQuota provides.
// Transactions begin IMMEDIATE, so processes sharing a file also take turns: one waits up to the
// busy timeout (15 seconds) for another's transaction, then fails. Opening and migrating are
// retried, so processes starting together on a new or older file all succeed. A long transaction
// also delays /readyz, which waits for the connection.
package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/store"
	"github.com/krishnanand20/einsora/controlplane/migrations"
)

type Store struct{ db *sql.DB }

// Open opens the database file at path, creating it if needed, and applies pending migrations.
// One machine owns a SQLite file, so there's no separate migration step to forget.
func Open(ctx context.Context, path string) (*Store, error) {
	path, checked, err := prepare(path)
	if err != nil {
		return nil, err
	}
	var db *sql.DB
	err = retry(ctx, func() error {
		var err error
		if db, err = connect(path, checked); err != nil {
			return err
		}
		if _, err = migrate(ctx, db); err != nil {
			_ = db.Close()
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Migrate applies pending migrations to the file at path and returns how many ran.
func Migrate(ctx context.Context, path string) (applied int, err error) {
	path, checked, err := prepare(path)
	if err != nil {
		return 0, err
	}
	err = retry(ctx, func() error {
		db, err := connect(path, checked)
		if err != nil {
			return err
		}
		applied, err = migrate(ctx, db)
		return errors.Join(err, db.Close())
	})
	return applied, err
}

// prepare checks the path and the files at it before SQLite opens anything, and returns the
// absolute path. An absolute path never starts with "file:", so SQLite reads it as a plain file
// name, never as a URI that could point somewhere other than the file checked here.
func prepare(path string) (string, os.FileInfo, error) {
	switch {
	case path == "" || strings.ContainsRune(path, '?'):
		return "", nil, fmt.Errorf("sqlite database path %q must be non-empty and contain no '?'", path)
	case path == ":memory:" || strings.HasPrefix(path, "file:"):
		return "", nil, fmt.Errorf("sqlite database path %q must be a file path, not :memory: or a file: URI", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, err
	}
	checked, err := ensurePrivate(abs)
	return abs, checked, err
}

// sidecars are the files SQLite keeps next to a database. They hold the same data.
var sidecars = []string{"-wal", "-shm", "-journal"}

// ensurePrivate makes sure the database and its sidecar files are readable by their owner only,
// like the log file and exports: they hold everything recorded about every workload. A new
// database is created 0600, and SQLite's sidecars copy its mode. Refused, each with the fix:
// a directory others can write to (where they could plant a file), anything that isn't a regular
// file (a symlink or device), and files others can read. It returns what it checked, so connect
// can confirm SQLite opened that same file.
func ensurePrivate(path string) (os.FileInfo, error) {
	dir := filepath.Dir(path)
	di, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("sqlite database directory: %w", err)
	}
	if di.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("sqlite database directory %s can be written by other users (mode %v); "+
			"use a private directory, or run: chmod go-w %s", dir, di.Mode().Perm(), dir)
	}
	f, err := os.OpenFile(filepath.Clean(path), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) // already absolute and clean
	switch {
	case err == nil:
		if err := f.Close(); err != nil { // SQLite treats an empty file as an empty database
			return nil, err
		}
	case !errors.Is(err, os.ErrExist):
		return nil, fmt.Errorf("create sqlite database: %w", err)
	}
	var main os.FileInfo
	var readable []string
	for i, name := range append([]string{path}, sidecarNames(path)...) {
		info, err := os.Lstat(name)
		if i > 0 && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s must be a regular file, not a symlink or device", name)
		}
		if info.Mode().Perm()&0o077 != 0 {
			readable = append(readable, name)
		}
		if i == 0 {
			main = info
		}
	}
	if len(readable) > 0 {
		return nil, fmt.Errorf("sqlite files can be read by other users: %s; run: chmod 600 %s",
			strings.Join(readable, ", "), strings.Join(readable, " "))
	}
	return main, nil
}

func sidecarNames(path string) []string {
	out := make([]string, len(sidecars))
	for i, suffix := range sidecars {
		out[i] = path + suffix
	}
	return out
}

func connect(path string, checked os.FileInfo) (*sql.DB, error) {
	dsn := path + "?" + url.Values{
		// recursive_triggers makes INSERT OR REPLACE fire the append-only DELETE trigger too.
		"_pragma": {"foreign_keys(1)", "recursive_triggers(1)", "busy_timeout(15000)", "journal_mode(WAL)"},
		"_txlock": {"immediate"},
	}.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	if now, err := os.Lstat(path); err != nil || !os.SameFile(checked, now) {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite database %s changed while it was being opened", path)
	}
	return db, nil
}

func migrate(ctx context.Context, db *sql.DB) (int, error) {
	p, err := goose.NewProvider(goose.DialectSQLite3, db, migrations.SQLite)
	if err != nil {
		return 0, fmt.Errorf("load migrations: %w", err)
	}
	results, err := p.Up(ctx)
	return len(results), err
}

// retry runs fn a few times with backoff. Processes that start together on a new or older file
// collide while switching it to WAL mode or migrating it: one wins, and the others then find the
// file ready. A real error fails every attempt and is returned.
func retry(ctx context.Context, fn func() error) error {
	delay := 50 * time.Millisecond
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(delay):
		}
		delay = min(2*delay, time.Second)
	}
	return err
}

func (s *Store) InTx(ctx context.Context, fn func(store.Queries) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit; also runs if fn panics
	if err := fn(&queries{db: tx}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
func (s *Store) Close()                         { _ = s.db.Close() }

type queries struct{ db *sql.Tx }

func mapErr(err error, what string) error {
	var se *sqlite.Error
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return store.NotFound(what)
	case errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
		return store.NotFound(what)
	case errors.As(err, &se) && (se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || se.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY):
		return domain.Errorf(domain.CodeConflict, "%s already exists", what)
	}
	return err
}

// Times are stored as fixed-width UTC text, so comparing and sorting the text compares the times.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

func ts(t time.Time) string { return t.UTC().Format(timeLayout) }

// timeCol scans a stored time.
type timeCol struct{ t *time.Time }

func (c timeCol) Scan(v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("time column holds %T, want text", v)
	}
	t, err := time.Parse(timeLayout, s)
	*c.t = t
	return err
}

// statesJSON passes a set of states as one parameter, read with json_each.
func statesJSON(states []domain.State) string {
	b, _ := json.Marshal(store.StatesAsStrings(states)) // []string always marshals
	return string(b)
}

type scanner interface{ Scan(dest ...any) error }

// collect runs a query and scans every row with scan.
func collect[T any](ctx context.Context, q *queries, scan func(scanner) (T, error), query string, args ...any) ([]T, error) {
	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---- tenants and quotas

const tenantCols = `id, slug, name, tier, created_at`

func scanTenant(row scanner) (domain.Tenant, error) {
	var t domain.Tenant
	var tier string
	err := row.Scan(&t.ID, &t.Slug, &t.Name, &tier, timeCol{&t.CreatedAt})
	t.Tier = domain.IsolationTier(tier)
	return t, err
}

func (q *queries) CreateTenant(ctx context.Context, t domain.Tenant) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO tenants (`+tenantCols+`) VALUES (?, ?, ?, ?, ?)`,
		t.ID, t.Slug, t.Name, string(t.Tier), ts(t.CreatedAt))
	return mapErr(err, "tenant")
}

func (q *queries) TenantBySlug(ctx context.Context, slug string) (domain.Tenant, error) {
	t, err := scanTenant(q.db.QueryRowContext(ctx, `SELECT `+tenantCols+` FROM tenants WHERE slug = ?`, slug))
	return t, mapErr(err, "tenant")
}

func (q *queries) TenantByID(ctx context.Context, id uuid.UUID) (domain.Tenant, error) {
	t, err := scanTenant(q.db.QueryRowContext(ctx, `SELECT `+tenantCols+` FROM tenants WHERE id = ?`, id))
	return t, mapErr(err, "tenant")
}

func (q *queries) SetQuota(ctx context.Context, quota domain.Quota) error {
	_, err := q.db.ExecContext(ctx, `
		INSERT INTO quotas (tenant_id, gpu_class, max_gpus) VALUES (?, ?, ?)
		ON CONFLICT (tenant_id, gpu_class) DO UPDATE SET max_gpus = excluded.max_gpus`,
		quota.TenantID, quota.GPUClass, quota.MaxGPUs)
	return mapErr(err, "tenant")
}

func (q *queries) Quotas(ctx context.Context, tenantID uuid.UUID) ([]domain.Quota, error) {
	return collect(ctx, q, func(r scanner) (domain.Quota, error) {
		var x domain.Quota
		err := r.Scan(&x.TenantID, &x.GPUClass, &x.MaxGPUs)
		return x, err
	}, `SELECT tenant_id, gpu_class, max_gpus FROM quotas WHERE tenant_id = ? ORDER BY gpu_class`, tenantID)
}

// LockQuota needs no row lock: transactions already run one at a time (see the package comment).
func (q *queries) LockQuota(ctx context.Context, tenantID uuid.UUID, gpuClass string) (int, error) {
	var limit int
	err := q.db.QueryRowContext(ctx,
		`SELECT max_gpus FROM quotas WHERE tenant_id = ? AND gpu_class = ?`, tenantID, gpuClass).Scan(&limit)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return limit, err
}

func (q *queries) GPUsHeld(ctx context.Context, tenantID uuid.UUID, gpuClass string) (int, error) {
	var n int
	err := q.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(gpu_count), 0) FROM workloads
		WHERE tenant_id = ? AND gpu_class = ? AND state IN (SELECT value FROM json_each(?))`,
		tenantID, gpuClass, statesJSON(domain.StatesWhere(domain.State.HoldsQuota))).Scan(&n)
	return n, err
}

// nullID stores uuid.Nil as NULL, for optional references such as a workload's machine.
// A NULL is read back as an empty string (COALESCE), which uuid.UUID scans as uuid.Nil.
func nullID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

// ---- local accounts

// nullTS stores a zero time as NULL; optionalTimeCol reads a NULL back as the zero time.
func nullTS(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return ts(t)
}

// optionalTimeCol scans a nullable time column, read back as an empty string (COALESCE) when NULL.
type optionalTimeCol struct{ t *time.Time }

func (c optionalTimeCol) Scan(v any) error {
	if s, ok := v.(string); ok && s == "" {
		*c.t = time.Time{}
		return nil
	}
	return timeCol(c).Scan(v)
}

const userCols = `id, tenant_id, email, password_hash, created_at, disabled_at`

const userSelect = `id, tenant_id, email, password_hash, created_at, COALESCE(disabled_at, '')`

func scanUser(row scanner) (domain.User, error) {
	var u domain.User
	err := row.Scan(&u.ID, &u.TenantID, &u.Email, &u.PasswordHash, timeCol{&u.CreatedAt}, optionalTimeCol{&u.DisabledAt})
	return u, err
}

// LockUsers needs nothing here: transactions already run one at a time.
func (q *queries) LockUsers(context.Context) error { return nil }

// LockUser only checks the user exists: SQLite runs one transaction at a time.
func (q *queries) LockUser(ctx context.Context, id uuid.UUID) error {
	var one int
	err := q.db.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ?`, id).Scan(&one)
	return mapErr(err, "user")
}

func (q *queries) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := q.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

func (q *queries) CreateUser(ctx context.Context, u domain.User) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO users (`+userCols+`) VALUES (?, ?, ?, ?, ?, ?)`,
		u.ID, u.TenantID, u.Email, u.PasswordHash, ts(u.CreatedAt), nullTS(u.DisabledAt))
	return mapErr(err, "user")
}

func (q *queries) UserByEmail(ctx context.Context, email string) (domain.User, error) {
	u, err := scanUser(q.db.QueryRowContext(ctx, `SELECT `+userSelect+` FROM users WHERE email = ?`, email))
	return u, mapErr(err, "user")
}

func (q *queries) UserByID(ctx context.Context, id uuid.UUID) (domain.User, error) {
	u, err := scanUser(q.db.QueryRowContext(ctx, `SELECT `+userSelect+` FROM users WHERE id = ?`, id))
	return u, mapErr(err, "user")
}

func (q *queries) SetPasswordHash(ctx context.Context, userID uuid.UUID, hash string) error {
	res, err := q.db.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, hash, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.NotFound("user")
	}
	return nil
}

func (q *queries) CreateSession(ctx context.Context, s domain.Session) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		s.TokenHash, s.UserID, ts(s.CreatedAt), ts(s.ExpiresAt))
	return mapErr(err, "session")
}

func (q *queries) SessionByTokenHash(ctx context.Context, hash string) (domain.Session, error) {
	var s domain.Session
	err := q.db.QueryRowContext(ctx, `SELECT token_hash, user_id, created_at, expires_at FROM sessions WHERE token_hash = ?`, hash).
		Scan(&s.TokenHash, &s.UserID, timeCol{&s.CreatedAt}, timeCol{&s.ExpiresAt})
	return s, mapErr(err, "session")
}

func (q *queries) DeleteSession(ctx context.Context, hash string) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hash)
	return err
}

func (q *queries) DeleteOtherSessions(ctx context.Context, userID uuid.UUID, keepHash string) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?`, userID, keepHash)
	return err
}

func (q *queries) DeleteExpiredSessions(ctx context.Context, now time.Time) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, ts(now))
	return err
}

const tokenCols = `id, user_id, name, prefix, token_hash, created_at, expires_at, last_used_at, revoked_at` //nolint:gosec // column names, not a credential

//nolint:gosec // column names, not a credential
const tokenSelect = `id, user_id, name, prefix, token_hash, created_at,
	COALESCE(expires_at, ''), COALESCE(last_used_at, ''), COALESCE(revoked_at, '')`

func scanToken(row scanner) (domain.APIToken, error) {
	var t domain.APIToken
	err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &t.TokenHash, timeCol{&t.CreatedAt},
		optionalTimeCol{&t.ExpiresAt}, optionalTimeCol{&t.LastUsedAt}, optionalTimeCol{&t.RevokedAt})
	return t, err
}

func (q *queries) CreateAPIToken(ctx context.Context, t domain.APIToken) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO api_tokens (`+tokenCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.UserID, t.Name, t.Prefix, t.TokenHash, ts(t.CreatedAt), nullTS(t.ExpiresAt), nullTS(t.LastUsedAt), nullTS(t.RevokedAt))
	return mapErr(err, "token")
}

func (q *queries) APITokenByHash(ctx context.Context, hash string) (domain.APIToken, error) {
	t, err := scanToken(q.db.QueryRowContext(ctx, `SELECT `+tokenSelect+` FROM api_tokens WHERE token_hash = ?`, hash))
	return t, mapErr(err, "token")
}

func (q *queries) TouchAPIToken(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := q.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, ts(at), id)
	return err
}

func (q *queries) ListAPITokens(ctx context.Context, userID uuid.UUID) ([]domain.APIToken, error) {
	return collect(ctx, q, scanToken, `SELECT `+tokenSelect+` FROM api_tokens WHERE user_id = ? ORDER BY created_at, id`, userID)
}

func (q *queries) RevokeAPIToken(ctx context.Context, userID, id uuid.UUID, at time.Time) error {
	res, err := q.db.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ? AND user_id = ?`,
		ts(at), id, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.NotFound("token")
	}
	return nil
}

// ---- machines

const machineCols = `id, tenant_id, name, region, os, arch, cpus, memory_mib, accelerators, sharing, last_seen_at, created_at`

const machineSelect = `id, tenant_id, name, region, os, arch, cpus, memory_mib, accelerators, sharing, COALESCE(last_seen_at, ''), created_at`

func scanMachine(row scanner) (domain.Machine, error) {
	var m domain.Machine
	var region, sharing, accelerators, lastSeen string
	err := row.Scan(&m.ID, &m.TenantID, &m.Name, &region, &m.OS, &m.Arch, &m.CPUs, &m.MemoryMiB, &accelerators, &sharing, &lastSeen, timeCol{&m.CreatedAt})
	if err != nil {
		return m, err
	}
	m.Region, m.Sharing = domain.Region(region), domain.SharingLevel(sharing)
	if lastSeen != "" {
		if err := (timeCol{&m.LastSeenAt}).Scan(lastSeen); err != nil {
			return m, err
		}
	}
	if err := json.Unmarshal([]byte(accelerators), &m.Accelerators); err != nil {
		return m, fmt.Errorf("machine %s accelerators: %w", m.ID, err)
	}
	return m, nil
}

func (q *queries) CreateMachine(ctx context.Context, m domain.Machine) error {
	accelerators := m.Accelerators
	if accelerators == nil {
		accelerators = []domain.Accelerator{}
	}
	raw, err := json.Marshal(accelerators)
	if err != nil {
		return err
	}
	var lastSeen any
	if !m.LastSeenAt.IsZero() {
		lastSeen = ts(m.LastSeenAt)
	}
	_, err = q.db.ExecContext(ctx, `INSERT INTO machines (`+machineCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.TenantID, m.Name, string(m.Region), m.OS, m.Arch, m.CPUs, m.MemoryMiB, string(raw), string(m.Sharing), lastSeen, ts(m.CreatedAt))
	return mapErr(err, "machine")
}

func (q *queries) MachinesInRegion(ctx context.Context, region domain.Region) ([]domain.Machine, error) {
	return collect(ctx, q, scanMachine, `SELECT `+machineSelect+` FROM machines WHERE region = ? ORDER BY name, id`, string(region))
}

func (q *queries) TakeLease(ctx context.Context, name, holder string, now time.Time, ttl time.Duration) (bool, error) {
	var got string
	err := q.db.QueryRowContext(ctx, `INSERT INTO leases (name, holder, expires_at) VALUES (?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET holder = excluded.holder, expires_at = excluded.expires_at
		WHERE leases.holder = excluded.holder OR leases.expires_at <= ?
		RETURNING holder`, name, holder, ts(now.Add(ttl)), ts(now)).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (q *queries) CreateAccessTicket(ctx context.Context, t domain.AccessTicket) error {
	res, err := q.db.ExecContext(ctx, `INSERT INTO access_tickets (ticket_hash, tenant_id, workload_id, subject, expires_at)
		SELECT ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM workloads WHERE id = ? AND tenant_id = ?)`,
		t.Hash, t.TenantID, t.WorkloadID, t.Subject, ts(t.ExpiresAt), t.WorkloadID, t.TenantID)
	if err != nil {
		return mapErr(err, "access ticket")
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return err
		}
		return store.NotFound("workload")
	}
	return nil
}

func (q *queries) UseAccessTicket(ctx context.Context, hash string, now time.Time) (domain.AccessTicket, error) {
	t := domain.AccessTicket{Hash: hash, UsedAt: now}
	err := q.db.QueryRowContext(ctx, `UPDATE access_tickets SET used_at = ?
		WHERE ticket_hash = ? AND used_at IS NULL AND expires_at > ?
		RETURNING tenant_id, workload_id, subject, expires_at`, ts(now), hash, ts(now)).Scan(&t.TenantID, &t.WorkloadID, &t.Subject, timeCol{&t.ExpiresAt})
	return t, mapErr(err, "access ticket")
}

func (q *queries) DeleteExpiredAccessTickets(ctx context.Context, now time.Time) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM access_tickets WHERE expires_at <= ?`, ts(now))
	return err
}

func (q *queries) ReleaseLease(ctx context.Context, name, holder string) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM leases WHERE name = ? AND holder = ?`, name, holder)
	return err
}

func (q *queries) MachineByID(ctx context.Context, id uuid.UUID) (domain.Machine, error) {
	m, err := scanMachine(q.db.QueryRowContext(ctx, `SELECT `+machineSelect+` FROM machines WHERE id = ?`, id))
	return m, mapErr(err, "machine")
}

func (q *queries) UpdateMachineInventory(ctx context.Context, m domain.Machine) error {
	accelerators := m.Accelerators
	if accelerators == nil {
		accelerators = []domain.Accelerator{}
	}
	raw, err := json.Marshal(accelerators)
	if err != nil {
		return err
	}
	var lastSeen any
	if !m.LastSeenAt.IsZero() {
		lastSeen = ts(m.LastSeenAt)
	}
	res, err := q.db.ExecContext(ctx, `UPDATE machines SET os = ?, arch = ?, cpus = ?, memory_mib = ?, accelerators = ?,
		last_seen_at = ? WHERE id = ?`, m.OS, m.Arch, m.CPUs, m.MemoryMiB, string(raw), lastSeen, m.ID)
	if err != nil {
		return mapErr(err, "machine")
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.NotFound("machine")
	}
	return nil
}

func (q *queries) WorkloadsOnMachine(ctx context.Context, machineID uuid.UUID) ([]domain.Workload, error) {
	return q.workloads(ctx, `SELECT `+workloadSelect+` FROM workloads
		WHERE machine_id = ? AND state <> 'deleted' ORDER BY created_at, id`, machineID)
}

func (q *queries) GetMachine(ctx context.Context, tenantID, id uuid.UUID) (domain.Machine, error) {
	m, err := scanMachine(q.db.QueryRowContext(ctx, `SELECT `+machineSelect+` FROM machines WHERE id = ? AND tenant_id = ?`, id, tenantID))
	return m, mapErr(err, "machine")
}

func (q *queries) ListMachines(ctx context.Context, tenantID uuid.UUID) ([]domain.Machine, error) {
	return collect(ctx, q, scanMachine, `SELECT `+machineSelect+` FROM machines WHERE tenant_id = ? ORDER BY name`, tenantID)
}

// ---- workloads

const workloadCols = `id, tenant_id, name, kind, state, image_id, gpu_class, gpu_count, params, created_by, created_at, updated_at,
	region, machine_id, placed_class, driver, attempt, fallback, fallback_after_s`

// workloadSelect reads workloadCols, with NULL placement read back as "not placed".
const workloadSelect = `id, tenant_id, name, kind, state, image_id, gpu_class, gpu_count, params, created_by, created_at, updated_at,
	region, COALESCE(machine_id, ''), COALESCE(placed_class, ''), COALESCE(driver, ''), attempt, fallback, fallback_after_s`

func scanWorkload(row scanner) (domain.Workload, error) {
	var w domain.Workload
	var kind, state, params, region, fallback string
	var fallbackAfter int64
	err := row.Scan(&w.ID, &w.TenantID, &w.Name, &kind, &state, &w.ImageID, &w.GPUClass,
		&w.GPUCount, &params, &w.CreatedBy, timeCol{&w.CreatedAt}, timeCol{&w.UpdatedAt},
		&region, &w.MachineID, &w.PlacedClass, &w.Driver, &w.Attempt, &fallback, &fallbackAfter)
	if err != nil {
		return w, err
	}
	w.Kind, w.State, w.Region = domain.WorkloadKind(kind), domain.State(state), domain.Region(region)
	w.FallbackAfter = time.Duration(fallbackAfter) * time.Second
	if err := json.Unmarshal([]byte(params), &w.Params); err != nil {
		return w, fmt.Errorf("workload %s params: %w", w.ID, err)
	}
	if err := json.Unmarshal([]byte(fallback), &w.Fallback); err != nil {
		return w, fmt.Errorf("workload %s fallback: %w", w.ID, err)
	}
	return w, nil
}

func (q *queries) workloads(ctx context.Context, query string, args ...any) ([]domain.Workload, error) {
	out, err := collect(ctx, q, scanWorkload, query, args...)
	if out == nil && err == nil {
		out = []domain.Workload{}
	}
	return out, err
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
	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}
	rawFallback, err := json.Marshal(fallback)
	if err != nil {
		return err
	}
	_, err = q.db.ExecContext(ctx, `INSERT INTO workloads (`+workloadCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?)`,
		w.ID, w.TenantID, w.Name, string(w.Kind), string(w.State), w.ImageID, w.GPUClass,
		w.GPUCount, string(rawParams), w.CreatedBy, ts(w.CreatedAt), ts(w.UpdatedAt),
		string(w.Region), nullID(w.MachineID), w.PlacedClass, w.Driver, w.Attempt, string(rawFallback), int64(w.FallbackAfter/time.Second))
	return mapErr(err, "workload")
}

func (q *queries) GetWorkload(ctx context.Context, tenantID, id uuid.UUID) (domain.Workload, error) {
	w, err := scanWorkload(q.db.QueryRowContext(ctx, `SELECT `+workloadSelect+` FROM workloads
		WHERE id = ? AND tenant_id = ? AND state <> 'deleted'`, id, tenantID))
	return w, mapErr(err, "workload")
}

func (q *queries) GetWorkloadHistory(ctx context.Context, tenantID, id uuid.UUID) (domain.Workload, error) {
	w, err := scanWorkload(q.db.QueryRowContext(ctx, `SELECT `+workloadSelect+` FROM workloads
		WHERE id = ? AND tenant_id = ?`, id, tenantID))
	return w, mapErr(err, "workload")
}

func (q *queries) ListWorkloads(ctx context.Context, tenantID uuid.UUID) ([]domain.Workload, error) {
	return q.workloads(ctx, `SELECT `+workloadSelect+` FROM workloads
		WHERE tenant_id = ? AND state <> 'deleted' ORDER BY created_at, id`, tenantID)
}

func (q *queries) ListWorkloadHistory(ctx context.Context, tenantID uuid.UUID) ([]domain.Workload, error) {
	return q.workloads(ctx, `SELECT `+workloadSelect+` FROM workloads
		WHERE tenant_id = ? ORDER BY created_at, id`, tenantID)
}

func (q *queries) UpdateWorkloadState(ctx context.Context, id uuid.UUID, from, to domain.State, at time.Time) error {
	res, err := q.db.ExecContext(ctx, `UPDATE workloads SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
		string(to), ts(at), id, string(from))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.StaleState(id, from)
	}
	return nil
}

func (q *queries) WorkloadsInStates(ctx context.Context, region domain.Region, states ...domain.State) ([]domain.Workload, error) {
	return q.workloads(ctx, `SELECT `+workloadSelect+` FROM workloads
		WHERE region = ? AND state IN (SELECT value FROM json_each(?)) ORDER BY created_at, id`, string(region), statesJSON(states))
}

func (q *queries) SetPlacement(ctx context.Context, id uuid.UUID, placedClass string, machineID uuid.UUID, driver string) error {
	var w domain.Workload
	var region string
	err := q.db.QueryRowContext(ctx, `SELECT tenant_id, region FROM workloads WHERE id = ?`, id).Scan(&w.TenantID, &region)
	if err != nil {
		return mapErr(err, "workload")
	}
	w.Region = domain.Region(region)
	if machineID == uuid.Nil && driver != "" {
		return domain.ErrDriverWithoutMachine
	}
	if machineID != uuid.Nil {
		m, err := scanMachine(q.db.QueryRowContext(ctx, `SELECT `+machineSelect+` FROM machines WHERE id = ?`, machineID))
		if err != nil {
			return mapErr(err, "machine")
		}
		if err := m.CheckPlacement(w, driver); err != nil {
			return err
		}
	}
	_, err = q.db.ExecContext(ctx, `UPDATE workloads SET placed_class = NULLIF(?, ''), machine_id = ?, driver = NULLIF(?, ''), attempt = attempt + 1 WHERE id = ?`,
		placedClass, nullID(machineID), driver, id)
	return mapErr(err, "machine")
}

func (q *queries) GPUsAllocated(ctx context.Context, region domain.Region) (map[string]int, error) {
	type row struct {
		class string
		n     int
	}
	rows, err := collect(ctx, q, func(r scanner) (row, error) {
		var x row
		err := r.Scan(&x.class, &x.n)
		return x, err
	}, `SELECT COALESCE(placed_class, gpu_class), SUM(gpu_count) FROM workloads
		WHERE region = ? AND state IN (SELECT value FROM json_each(?)) GROUP BY COALESCE(placed_class, gpu_class)`,
		string(region), statesJSON(domain.StatesWhere(domain.State.HoldsCapacity)))
	out := map[string]int{}
	for _, r := range rows {
		out[r.class] = r.n
	}
	return out, err
}

func (q *queries) WorkloadsForMetering(ctx context.Context, since time.Time) ([]domain.Workload, error) {
	return q.workloads(ctx, `SELECT `+workloadSelect+` FROM workloads w
		WHERE state IN (SELECT value FROM json_each(?))
		   OR EXISTS (SELECT 1 FROM workload_events e WHERE e.workload_id = w.id AND e.at >= ?)
		ORDER BY created_at, id`,
		statesJSON(domain.StatesWhere(domain.State.Metered)), ts(since))
}

// ---- events and usage

func (q *queries) AppendEvent(ctx context.Context, e domain.Event) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO workload_events (workload_id, from_state, to_state, reason, at)
		VALUES (?, NULLIF(?, ''), ?, ?, ?)`,
		e.WorkloadID, string(e.From), string(e.To), e.Reason, ts(e.At))
	return mapErr(err, "workload")
}

const eventCols = `e.id, e.workload_id, COALESCE(e.from_state, ''), e.to_state, e.reason, e.at`

func scanEvent(r scanner) (domain.Event, error) {
	var e domain.Event
	var from, to string
	err := r.Scan(&e.ID, &e.WorkloadID, &from, &to, &e.Reason, timeCol{&e.At})
	e.From, e.To = domain.State(from), domain.State(to)
	return e, err
}

func (q *queries) Events(ctx context.Context, workloadID uuid.UUID) ([]domain.Event, error) {
	return collect(ctx, q, scanEvent, `SELECT `+eventCols+` FROM workload_events e
		WHERE e.workload_id = ? ORDER BY e.at, e.id`, workloadID)
}

func (q *queries) EventsForWorkload(ctx context.Context, tenantID, workloadID uuid.UUID) ([]domain.Event, error) {
	return collect(ctx, q, scanEvent, `SELECT `+eventCols+` FROM workload_events e
		JOIN workloads w ON w.id = e.workload_id AND w.tenant_id = ?
		WHERE e.workload_id = ? ORDER BY e.at, e.id`, tenantID, workloadID)
}

func (q *queries) InsertUsageIfAbsent(ctx context.Context, r domain.UsageRecord) (bool, error) {
	if !r.Region.Valid() {
		return false, store.InvalidRegion("usage record", r.Region)
	}
	res, err := q.db.ExecContext(ctx, `INSERT INTO usage_records (`+usageCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (workload_id, period_start) DO NOTHING`,
		r.WorkloadID, ts(r.PeriodStart), r.TenantID, r.GPUClass, r.GPUCount, r.Seconds, string(r.Region), nullID(r.MachineID))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

const usageCols = `workload_id, period_start, tenant_id, gpu_class, gpu_count, seconds, region, machine_id`

const usageSelect = `workload_id, period_start, tenant_id, gpu_class, gpu_count, seconds, region, COALESCE(machine_id, '')`

func scanUsage(r scanner) (domain.UsageRecord, error) {
	var u domain.UsageRecord
	var region string
	err := r.Scan(&u.WorkloadID, timeCol{&u.PeriodStart}, &u.TenantID, &u.GPUClass, &u.GPUCount, &u.Seconds, &region, &u.MachineID)
	u.Region = domain.Region(region)
	return u, err
}

func (q *queries) Usage(ctx context.Context, tenantID uuid.UUID, from, to time.Time) ([]domain.UsageRecord, error) {
	return collect(ctx, q, scanUsage, `SELECT `+usageSelect+` FROM usage_records
		WHERE tenant_id = ? AND period_start >= ? AND period_start < ?
		ORDER BY period_start, workload_id`, tenantID, ts(from), ts(to))
}

func (q *queries) UsageForWorkload(ctx context.Context, tenantID, workloadID uuid.UUID) ([]domain.UsageRecord, error) {
	return collect(ctx, q, scanUsage, `SELECT `+usageSelect+` FROM usage_records
		WHERE tenant_id = ? AND workload_id = ? ORDER BY period_start`, tenantID, workloadID)
}
