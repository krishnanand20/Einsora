package api_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/krishnanand20/tensorafi/controlplane/internal/api"
	"github.com/krishnanand20/tensorafi/controlplane/internal/auth"
	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
	"github.com/krishnanand20/tensorafi/controlplane/internal/service"
	"github.com/krishnanand20/tensorafi/controlplane/internal/store"
	"github.com/krishnanand20/tensorafi/controlplane/internal/testutil"
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
	if err != nil || u.Scheme != "http" || u.Host != "w-"+id.String()+".localhost:8090" || u.Path != "/_tensorafi/open" ||
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
