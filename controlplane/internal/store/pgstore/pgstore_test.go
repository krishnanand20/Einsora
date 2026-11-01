package pgstore

import (
	"context"
	"database/sql"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"

	"github.com/krishnanand20/tensorafi/controlplane/internal/store"
	"github.com/krishnanand20/tensorafi/controlplane/internal/store/storetest"
	"github.com/krishnanand20/tensorafi/controlplane/migrations"
)

func TestConformance(t *testing.T) {
	url := os.Getenv("TENSORAFI_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TENSORAFI_TEST_DATABASE_URL to run Postgres store tests (make test-cp-pg)")
	}
	ctx := context.Background()
	if _, err := Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	storetest.Run(t, func(t *testing.T) store.Store {
		st, err := Open(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(st.Close)
		// Every table in the schema list, so a new table can't be missed.
		tables := slices.Sorted(maps.Keys(storetest.SchemaColumns))
		if _, err := st.pool.Exec(ctx, `TRUNCATE `+strings.Join(tables, ", ")); err != nil {
			t.Fatal(err)
		}
		return st
	})
}

func TestSchemaMatchesSQLite(t *testing.T) {
	url := os.Getenv("TENSORAFI_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TENSORAFI_TEST_DATABASE_URL to run Postgres store tests (make test-cp-pg)")
	}
	ctx := context.Background()
	if _, err := Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	st, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	columns := map[string][]string{}
	rows, err := st.pool.Query(ctx, `SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name <> 'goose_db_version'`)
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// Indexes backing primary keys and unique constraints are left out, as in SQLite.
	rows, err = st.pool.Query(ctx, `SELECT i.indexname FROM pg_indexes i
		WHERE i.schemaname = current_schema() AND i.tablename <> 'goose_db_version'
		  AND NOT EXISTS (SELECT 1 FROM pg_constraint c WHERE c.conname = i.indexname)`)
	if err != nil {
		t.Fatal(err)
	}
	indexes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	storetest.CheckSchema(t, columns, indexes)
}

// Upgrading a database with data, from version 1 through the 00002 gap to the latest. It runs
// in a throwaway schema, so the test database's own tables are untouched.
func TestUpgradeRepairsPlacement(t *testing.T) {
	base := os.Getenv("TENSORAFI_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("set TENSORAFI_TEST_DATABASE_URL to run Postgres store tests (make test-cp-pg)")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })
	schema := "upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") })
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	url := base + sep + "search_path=" + schema

	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations.Postgres)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, storetest.UpgradeFixture("now()")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, storetest.BadUsageAfter00002("date_trunc('hour', now())")); err != nil {
		t.Fatal(err)
	}
	if n, err := Migrate(ctx, url); err != nil || n == 0 {
		t.Fatalf("upgrade: applied %d, %v", n, err)
	}
	st, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	storetest.CheckUpgraded(t, st)
}
