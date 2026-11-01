package api_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/api"
	"github.com/krishnanand20/einsora/controlplane/internal/auth"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/service"
	"github.com/krishnanand20/einsora/controlplane/internal/store"
	"github.com/krishnanand20/einsora/controlplane/internal/testutil"
)

func TestOpeningAWorkloadGivesAOneTimeURL(t *testing.T) {
	st := testutil.SeededStore(t)
	ctx := context.Background()
	id, machine, endpoint := uuid.New(), uuid.New(), uuid.New()
	err := st.InTx(ctx, func(q store.Queries) error {
		tn, err := q.TenantBySlug(ctx, "acme")
		if err != nil {
			return err
		}
		if err := q.CreateMachine(ctx, domain.Machine{ID: machine, TenantID: tn.ID, Name: "studio", Region: domain.RegionLocal,
			OS: "darwin", Arch: "arm64", CPUs: 8, MemoryMiB: 16384, Sharing: domain.SharingPrivate, CreatedAt: testutil.T0}); err != nil {
			return err
		}
		if err := q.CreateWorkload(ctx, domain.Workload{ID: id, TenantID: tn.ID, Name: "nb", Kind: domain.KindNotebook,
			State: domain.StateRunning, ImageID: "pytorch-notebook", GPUClass: "l40s-48gb", GPUCount: 1, CreatedBy: "t",
			MachineID: machine, CreatedAt: testutil.T0, UpdatedAt: testutil.T0, Region: domain.RegionLocal}); err != nil {
			return err
		}
		return q.CreateWorkload(ctx, domain.Workload{ID: endpoint, TenantID: tn.ID, Name: "ep", Kind: domain.KindEndpoint,
			State: domain.StateRunning, ImageID: "openai-endpoint", GPUClass: "l40s-48gb", GPUCount: 1, CreatedBy: "t",
			MachineID: machine, CreatedAt: testutil.T0, UpdatedAt: testutil.T0, Region: domain.RegionLocal})
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, testutil.Catalog(t), nil)
	log := slog.New(slog.DiscardHandler)
	path := "/v1/workloads/" + id.String() + "/access"

	h := api.New(svc, auth.Dev{}, log, st.Ping).WithAccessPort(8090).Handler()
	code, body := call(t, h, "POST", path, "acme", "")
	if code != http.StatusCreated {
		t.Fatalf("open: %d %v", code, body)
	}
	u, err := url.Parse(body["url"].(string))
	if err != nil || u.Scheme != "http" || u.Host != "w-"+id.String()+".localhost:8090" || u.Path != "/_einsora/open" ||
		!strings.HasPrefix(u.Query().Get("ticket"), "tat_") || body["expires_at"] == nil {
		t.Errorf("url = %v (%v); want a one-time link at the workload's own host", body["url"], err)
	}
	if code, body := call(t, h, "POST", path, "globex", ""); code != http.StatusNotFound {
		t.Errorf("another tenant: %d %v; want 404", code, body)
	}
	endpointPath := "/v1/workloads/" + endpoint.String() + "/access"
	code, body = call(t, h, "POST", endpointPath, "acme", "")
	if code != http.StatusCreated || body["url"] != "http://w-"+endpoint.String()+".localhost:8090/v1" || body["expires_at"] != nil {
		t.Errorf("endpoint API access = %d %v; want a base URL without a ticket or expiry", code, body)
	}
	if code, _ := call(t, h, "POST", endpointPath, "globex", ""); code != http.StatusNotFound {
		t.Errorf("another tenant's endpoint access: %d; want 404", code)
	}

	// Without the access proxy (any backend but agent), there's nothing to open it through.
	h = api.New(svc, auth.Dev{}, log, st.Ping).Handler()
	if code, body := call(t, h, "POST", path, "acme", ""); code != http.StatusBadRequest || errCode(body) != "invalid_request" {
		t.Errorf("without the proxy: %d %v; want 400", code, body)
	}
}

func TestMachinesAPI(t *testing.T) {
	st := testutil.SeededStore(t)
	ctx := context.Background()
	id := uuid.New()
	err := st.InTx(ctx, func(q store.Queries) error {
		tn, err := q.TenantBySlug(ctx, "acme")
		if err != nil {
			return err
		}
		return q.CreateMachine(ctx, domain.Machine{ID: id, TenantID: tn.ID, Name: "studio", Region: domain.RegionLocal,
			OS: "darwin", Arch: "arm64", CPUs: 12, MemoryMiB: 36 * 1024, Sharing: domain.SharingPrivate,
			Accelerators: []domain.Accelerator{{Kind: domain.AcceleratorApple, Model: "Apple M3 Pro", Count: 1, MemoryMiB: 36 * 1024}},
			LastSeenAt:   time.Now(), CreatedAt: testutil.T0})
	})
	if err != nil {
		t.Fatal(err)
	}
	h := api.New(service.New(st, testutil.Catalog(t), nil), auth.Dev{}, slog.New(slog.DiscardHandler), st.Ping).Handler()

	code, list := call(t, h, "GET", "/v1/machines", "acme", "")
	items, _ := list["items"].([]any)
	if code != http.StatusOK || len(items) != 1 {
		t.Fatalf("list: %d %v", code, list)
	}
	m := items[0].(map[string]any)
	if m["name"] != "studio" || m["status"] != "online" || m["region"] != "local" || m["last_seen_at"] == nil ||
		len(m["accelerators"].([]any)) != 1 {
		t.Errorf("machine = %v", m)
	}
	for _, leaked := range []string{"tenant_id", "TenantID"} {
		if _, ok := m[leaked]; ok {
			t.Errorf("response leaks %q", leaked)
		}
	}
	if code, body := call(t, h, "GET", "/v1/machines/"+id.String(), "acme", ""); code != http.StatusOK || body["id"] != id.String() {
		t.Errorf("get: %d %v", code, body)
	}
	if code, list := call(t, h, "GET", "/v1/machines", "globex", ""); code != http.StatusOK || len(list["items"].([]any)) != 0 {
		t.Errorf("another tenant's list: %d %v; want empty", code, list)
	}
	if code, body := call(t, h, "GET", "/v1/machines/"+id.String(), "globex", ""); code != http.StatusNotFound {
		t.Errorf("another tenant's machine: %d %v; want 404", code, body)
	}
	if code, body := call(t, h, "GET", "/v1/machines/not-a-uuid", "acme", ""); code != http.StatusNotFound || !strings.Contains(fmt.Sprint(body), "machine not found") {
		t.Errorf("a malformed id: %d %v; want 404, machine not found", code, body)
	}
}
