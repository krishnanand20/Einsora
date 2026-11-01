package access_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/tensorafi/controlplane/internal/access"
	"github.com/krishnanand20/tensorafi/controlplane/internal/credential"
	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
	"github.com/krishnanand20/tensorafi/controlplane/internal/service"
	"github.com/krishnanand20/tensorafi/controlplane/internal/store"
	"github.com/krishnanand20/tensorafi/controlplane/internal/testutil"
)

func endpointFixture(t *testing.T) (fixture, *service.Service) {
	t.Helper()
	f := newFixture(t)
	notebook := f.workload
	f.workload = uuid.New()
	machine := uuid.New()
	err := f.st.InTx(context.Background(), func(q store.Queries) error {
		if err := q.CreateMachine(context.Background(), domain.Machine{ID: machine, TenantID: f.tenant,
			Name: "mac", Region: domain.RegionLocal, OS: "darwin", Arch: "arm64", CPUs: 8, MemoryMiB: 32768,
			Sharing: domain.SharingPrivate, CreatedAt: testutil.T0}); err != nil {
			return err
		}
		return q.CreateWorkload(context.Background(), domain.Workload{ID: f.workload, TenantID: f.tenant,
			Name: "model", Kind: domain.KindEndpoint, State: domain.StateRunning, ImageID: "openai-endpoint",
			GPUClass: "apple-silicon-32gb", PlacedClass: "apple-silicon-32gb", GPUCount: 1, MachineID: machine,
			Driver: domain.DriverMacOSNative, Region: domain.RegionLocal, CreatedAt: testutil.T0, UpdatedAt: testutil.T0,
			Params: map[string]string{domain.ParamModelURI: "hf://org/model"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	f.targets[f.workload] = f.targets[notebook]
	svc := service.New(f.st, testutil.Catalog(t), func() time.Time { return testutil.T0 })
	f.proxy = access.New(f.st, f.targets, slog.New(slog.DiscardHandler), nil).WithAPITokens(svc)
	return f, svc
}

func apiToken(t *testing.T, f fixture, tenant string) (string, uuid.UUID, uuid.UUID) {
	t.Helper()
	value, hash, err := credential.NewToken(service.TokenPrefix)
	if err != nil {
		t.Fatal(err)
	}
	user, token := uuid.New(), uuid.New()
	err = f.st.InTx(context.Background(), func(q store.Queries) error {
		tenant, err := q.TenantBySlug(context.Background(), tenant)
		if err != nil {
			return err
		}
		if err := q.CreateUser(context.Background(), domain.User{ID: user, TenantID: tenant.ID,
			Email: user.String() + "@example.test", PasswordHash: "unused", CreatedAt: testutil.T0}); err != nil {
			return err
		}
		return q.CreateAPIToken(context.Background(), domain.APIToken{ID: token, UserID: user, Name: "test",
			Prefix: value[:8], TokenHash: hash, CreatedAt: testutil.T0})
	})
	if err != nil {
		t.Fatal(err)
	}
	return value, user, token
}

func TestEndpointUsesTenantScopedAPITokens(t *testing.T) {
	f, _ := endpointFixture(t)
	value, user, token := apiToken(t, f, "internal")
	header := map[string]string{"Authorization": "Bearer " + value}
	response := f.do("GET", f.host(f.workload), "/v1/models", &http.Cookie{Name: "private", Value: "browser-only"}, header)
	if response.Code != http.StatusOK || response.Header().Get("X-Request-ID") == "" ||
		response.Header().Get("X-TensoraFi-Region") != "local" || response.Header().Get("X-TensoraFi-Fallback") != "false" ||
		response.Header().Get("X-TensoraFi-Compute-Class") != "apple-silicon-32gb" || len(response.Result().Cookies()) != 0 {
		t.Fatalf("endpoint response = %d, %v", response.Code, response.Header())
	}
	seen := <-f.seen
	if seen.Header.Get("Authorization") != "Bearer jupyter-token" || seen.Header.Get("Cookie") != "" ||
		seen.Header.Get("X-Request-ID") != response.Header().Get("X-Request-ID") {
		t.Error("the endpoint received browser credentials or lost its request ID")
	}
	other, _, _ := apiToken(t, f, "globex")
	for _, id := range []uuid.UUID{f.workload, uuid.New()} {
		if response := f.do("GET", f.host(id), "/v1/models", nil, map[string]string{"Authorization": "Bearer " + other}); response.Code != http.StatusNotFound {
			t.Errorf("another tenant's request: %d; want 404", response.Code)
		}
	}
	if err := f.st.InTx(context.Background(), func(q store.Queries) error {
		return q.RevokeAPIToken(context.Background(), user, token, testutil.T0)
	}); err != nil {
		t.Fatal(err)
	}
	if response := f.do("GET", f.host(f.workload), "/v1/models", nil, header); response.Code != http.StatusUnauthorized {
		t.Errorf("revoked token: %d; want 401", response.Code)
	}
}

func TestEndpointRejectsCookiesWrongRoutesAndStoppedWorkloads(t *testing.T) {
	f, svc := endpointFixture(t)
	value, _, _ := apiToken(t, f, "internal")
	header := map[string]string{"Authorization": "Bearer " + value}
	for _, path := range []string{"/v1/models", "/v1/chat/completions"} {
		if response := f.do("GET", f.host(f.workload), path, &http.Cookie{Name: "tensorafi_access", Value: "planted"}, nil); response.Code != http.StatusUnauthorized {
			t.Errorf("cookie request for %s: %d; want 401", path, response.Code)
		}
	}
	for _, path := range []string{"/api", "/v1/load", "/v1/models/../../api"} {
		if response := f.do("POST", f.host(f.workload), path, nil, header); response.Code != http.StatusNotFound {
			t.Errorf("unsupported endpoint path %s: %d; want 404", path, response.Code)
		}
	}
	req := httptest.NewRequest("GET", "http://"+f.host(f.workload)+"/v1/models", nil)
	req.Header.Add("Authorization", "Bearer "+value)
	req.Header.Add("Authorization", "Bearer "+value)
	rec := httptest.NewRecorder()
	f.proxy.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("ambiguous credentials: %d; want 401", rec.Code)
	}
	if _, err := svc.StopWorkload(context.Background(), domain.Principal{TenantSlug: "internal", Subject: "owner"}, f.workload); err != nil {
		t.Fatal(err)
	}
	if response := f.do("GET", f.host(f.workload), "/v1/models", nil, header); response.Code != http.StatusServiceUnavailable {
		t.Errorf("stopped endpoint: %d; want 503", response.Code)
	}
}
