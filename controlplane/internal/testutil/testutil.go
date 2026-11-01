// Package testutil holds shared test fixtures. Import only from _test.go files.
package testutil

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/krishnanand20/tensorafi/controlplane/internal/catalog"
	"github.com/krishnanand20/tensorafi/controlplane/internal/devseed"
	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
	"github.com/krishnanand20/tensorafi/controlplane/internal/store"
	"github.com/krishnanand20/tensorafi/controlplane/internal/store/memstore"
)

// Principals for the tenants created by devseed.
var (
	Acme     = domain.Principal{Subject: "alice@acme.test", TenantSlug: "acme"}
	Globex   = domain.Principal{Subject: "bob@globex.test", TenantSlug: "globex"}
	Internal = domain.Principal{Subject: "ops@tensorafi.test", TenantSlug: "internal"}
)

const SSHKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleExampleExampleExampleExampleExampl test@example"

// T0 is a fixed start time for deterministic tests.
var T0 = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

// Catalog loads the real images/catalog.yaml, so tests break if the catalog does.
func Catalog(t testing.TB) *catalog.Catalog {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		path := filepath.Join(dir, "images", "catalog.yaml")
		if _, err := os.Stat(path); err == nil {
			c, err := catalog.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("images/catalog.yaml not found above working directory")
		}
		dir = parent
	}
}

// SeededStore returns a memstore with the devseed tenants and quotas.
func SeededStore(t testing.TB) store.Store {
	t.Helper()
	st := memstore.New()
	if err := devseed.Seed(context.Background(), st, T0); err != nil {
		t.Fatal(err)
	}
	return st
}

// Clock is a manually advanced clock.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

func NewClock(t time.Time) *Clock { return &Clock{now: t} }

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
