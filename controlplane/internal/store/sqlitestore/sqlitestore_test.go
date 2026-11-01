package sqlitestore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/store"
	"github.com/krishnanand20/einsora/controlplane/internal/store/storetest"
	"github.com/krishnanand20/einsora/controlplane/migrations"
)

// SQLite needs no server, so the conformance suite runs on every `go test`.
func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		st, err := Open(context.Background(), filepath.Join(t.TempDir(), "einsora.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(st.Close)
		return st
	})
}

func TestMigrateIsIdempotentAndPathsMayHaveSpaces(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "my data", "einsora.db")
	if _, err := Migrate(ctx, path); err == nil {
		t.Fatal("a missing directory must be an error, not a silent new location")
	}
	path = filepath.Join(t.TempDir(), "einsora data.db")
	if n, err := Migrate(ctx, path); err != nil || n == 0 {
		t.Fatalf("first migrate: %d, %v", n, err)
	}
	if n, err := Migrate(ctx, path); err != nil || n != 0 {
		t.Fatalf("second migrate: %d, %v; want nothing to do", n, err)
	}
	if _, err := Open(ctx, path+"?mode=memory"); err == nil {
		t.Error("a path with '?' must be refused")
	}
}

// The schema enforces what Postgres enforces: slug format, append-only events, foreign keys.
func TestSchemaConstraints(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "einsora.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	exec := func(query string, args ...any) error {
		return st.InTx(ctx, func(q store.Queries) error {
			_, err := q.(*queries).db.ExecContext(ctx, query, args...)
			return err
		})
	}
	slugs := map[string]bool{
		"acme": true, "a-1": true, "-acme": false, "acme-": false, "Acme": false, "a_b": false, "": false,
		"acme\x00EVIL": false, "\x00": false, strings.Repeat("a", 40): true, strings.Repeat("a", 41): false,
	}
	for slug, ok := range slugs {
		err := exec(`INSERT INTO tenants (id, slug, name, tier, created_at) VALUES (?, ?, 'n', 'trusted', 'x')`, "id-"+slug, slug)
		if (err == nil) != ok {
			t.Errorf("slug %q: err = %v, want ok=%v", slug, err, ok)
		}
	}
	if err := exec(`INSERT INTO workloads (id, tenant_id, name, kind, state, image_id, gpu_class, gpu_count, created_by, created_at, updated_at)
		VALUES ('w1', 'no-such-tenant', 'n', 'vm', 'pending', 'i', 'c', 1, 's', 'x', 'x')`); err == nil {
		t.Error("foreign keys must be enforced")
	}
	if err := exec(`INSERT INTO workloads (id, tenant_id, name, kind, state, image_id, gpu_class, gpu_count, created_by, created_at, updated_at)
		VALUES ('w1', 'id-acme', 'n', 'vm', 'pending', 'i', 'c', 1, 's', 'x', 'x')`); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO workload_events (workload_id, to_state, reason, at) VALUES ('w1', 'pending', 'r', 'x')`); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []string{
		`UPDATE workload_events SET reason = 'rewritten'`,
		`DELETE FROM workload_events`,
		`INSERT OR REPLACE INTO workload_events (id, workload_id, to_state, reason, at) SELECT id, workload_id, to_state, 'rewritten', at FROM workload_events`,
	} {
		if err := exec(mutate); err == nil {
			t.Errorf("%s: workload_events must be append-only", mutate)
		}
	}
	orphan := domain.Workload{ID: uuid.New(), TenantID: uuid.New(), Name: "w", Kind: domain.KindVM, State: domain.StatePending,
		ImageID: "i", GPUClass: "c", GPUCount: 1, CreatedBy: "s", Region: domain.RegionLocal}
	err = st.InTx(ctx, func(q store.Queries) error { return q.CreateWorkload(ctx, orphan) })
	if !domain.IsNotFound(err) {
		t.Errorf("workload for a missing tenant: err = %v, want not found", err)
	}
}

// Quota checks rely on transactions running one at a time (there's no SELECT … FOR UPDATE). Many
// concurrent creates, from two handles on the same file as two processes would have, must never
// exceed the quota and must never fail with "database is locked".
func TestConcurrentCreatesNeverExceedQuota(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "einsora.db")
	a, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	b, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)

	tn := domain.Tenant{ID: uuid.New(), Slug: "acme", Name: "Acme", Tier: domain.TierUntrusted, CreatedAt: time.Now()}
	err = a.InTx(ctx, func(q store.Queries) error {
		if err := q.CreateTenant(ctx, tn); err != nil {
			return err
		}
		return q.SetQuota(ctx, domain.Quota{TenantID: tn.ID, GPUClass: "l40s-48gb", MaxGPUs: 5})
	})
	if err != nil {
		t.Fatal(err)
	}

	const attempts = 40
	var wg sync.WaitGroup
	errs := make(chan error, attempts)
	for i := range attempts {
		st := a
		if i%2 == 1 {
			st = b
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- st.InTx(ctx, func(q store.Queries) error {
				limit, err := q.LockQuota(ctx, tn.ID, "l40s-48gb")
				if err != nil {
					return err
				}
				held, err := q.GPUsHeld(ctx, tn.ID, "l40s-48gb")
				if err != nil || held+1 > limit {
					return err
				}
				now := time.Now()
				return q.CreateWorkload(ctx, domain.Workload{ID: uuid.New(), TenantID: tn.ID, Name: "w", Kind: domain.KindNotebook,
					State: domain.StatePending, ImageID: "i", GPUClass: "l40s-48gb", GPUCount: 1, CreatedBy: "s", CreatedAt: now, UpdatedAt: now,
					Region: domain.RegionLocal})
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent transaction failed: %v", err)
		}
	}
	var held int
	err = a.InTx(ctx, func(q store.Queries) error {
		held, err = q.GPUsHeld(ctx, tn.ID, "l40s-48gb")
		return err
	})
	if err != nil || held != 5 {
		t.Fatalf("GPUs held = %d, %v; want exactly the quota (5)", held, err)
	}
}

// The database holds everything about every workload, so it's readable by its owner only.
func TestDatabaseFileIsPrivate(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "einsora.db")
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{path, path + "-wal", path + "-shm"} { // all three exist while it's open
		info, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", filepath.Base(f), info.Mode().Perm())
		}
	}
	st.Close()

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("a database others can read must be refused with the fix: %v", err)
	}
}

func TestSchemaMatchesPostgres(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "einsora.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	columns := map[string][]string{}
	rows, err := st.db.QueryContext(ctx, `SELECT m.name, p.name FROM sqlite_schema m, pragma_table_info(m.name) p
		WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' AND m.name <> 'goose_db_version'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatal(err)
		}
		columns[table] = append(columns[table], column)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	var indexes []string
	rows, err = st.db.QueryContext(ctx, `SELECT name FROM sqlite_schema
		WHERE type = 'index' AND name NOT LIKE 'sqlite_autoindex_%' AND tbl_name <> 'goose_db_version'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		indexes = append(indexes, name)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	storetest.CheckSchema(t, columns, indexes)
}

// Paths SQLite would read as something other than the file checked here are refused.
func TestOnlyPlainFilePathsAreAccepted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Chdir(dir)
	for _, path := range []string{"file:x.db", "file:" + filepath.Join(dir, "x.db"), ":memory:", "x.db?mode=memory", ""} {
		if st, err := Open(ctx, path); err == nil {
			st.Close()
			t.Errorf("Open(%q) succeeded; want it refused", path)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("refused paths left files behind: %v", entries)
	}

	// A relative path is resolved once, so the check and SQLite use the same file.
	st, err := Open(ctx, "einsora.db")
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if info, err := os.Stat(filepath.Join(dir, "einsora.db")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("relative path: %v, %v", info, err)
	}
}

func TestUnsafeFilesAndDirectoriesAreRefused(t *testing.T) {
	ctx := context.Background()

	shared := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777); err != nil { //nolint:gosec // the test needs a directory others can write
		t.Fatal(err)
	}
	if _, err := Open(ctx, filepath.Join(shared, "einsora.db")); err == nil || !strings.Contains(err.Error(), "chmod go-w") {
		t.Errorf("database in a directory others can write: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "einsora.db")
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := os.WriteFile(path+"-wal", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path+"-wal", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, path); err == nil || !strings.Contains(err.Error(), "einsora.db-wal") {
		t.Errorf("a sidecar others can read must be refused and named: %v", err)
	}
	if err := os.Remove(path + "-wal"); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, link); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Errorf("a symlinked database must be refused: %v", err)
	}
}

// Processes that start together on a new file (api, controller, and metering as separate
// services) must all open it, not fail with "database is locked".
func TestConcurrentFirstOpens(t *testing.T) {
	ctx := context.Background()
	for range 3 {
		path := filepath.Join(t.TempDir(), "einsora.db")
		var wg sync.WaitGroup
		errs := make(chan error, 4)
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				st, err := Open(ctx, path)
				if err == nil {
					st.Close()
				}
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent first open: %v", err)
			}
		}
	}
}

// Upgrading a database with data, from version 1 through the 00002 gap to the latest.
func TestUpgradeRepairsPlacement(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "einsora.db")
	abs, checked, err := prepare(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := connect(abs, checked)
	if err != nil {
		t.Fatal(err)
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, db, migrations.SQLite)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, storetest.UpgradeFixture("'2026-01-01T10:00:00.000000000Z'")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, storetest.BadUsageAfter00002("'2026-01-01T10:00:00.000000000Z'")); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(ctx, path) // migrates to the latest version
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	storetest.CheckUpgraded(t, st)
}
