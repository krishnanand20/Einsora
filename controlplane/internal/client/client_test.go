package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/krishnanand20/tensorafi/controlplane/internal/api"
	"github.com/krishnanand20/tensorafi/controlplane/internal/auth"
	"github.com/krishnanand20/tensorafi/controlplane/internal/client"
	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
	"github.com/krishnanand20/tensorafi/controlplane/internal/service"
	"github.com/krishnanand20/tensorafi/controlplane/internal/testutil"
)

func TestClientAgainstRealAPI(t *testing.T) {
	st := testutil.SeededStore(t)
	svc := service.New(st, testutil.Catalog(t), nil)
	srv := httptest.NewServer(api.New(svc, auth.Dev{}, slog.New(slog.DiscardHandler), st.Ping).Handler())
	t.Cleanup(srv.Close)

	ctx := context.Background()
	w, err := svc.CreateWorkload(ctx, testutil.Acme, service.CreateWorkloadInput{
		Name: "nb", Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUClass: "l40s-48gb", GPUCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	c := client.FromEnv(func(k string) string {
		return map[string]string{"TENSORAFI_API_URL": srv.URL + "/", "TENSORAFI_DEV_TENANT": "acme"}[k]
	})

	list, err := c.ListWorkloads(ctx, false)
	if err != nil || len(list) != 1 || list[0].ID != w.ID.String() {
		t.Fatalf("ListWorkloads = %+v, %v", list, err)
	}
	events, err := c.Events(ctx, w.ID.String())
	if err != nil || len(events) != 1 || events[0].To != "pending" {
		t.Fatalf("Events = %+v, %v", events, err)
	}
	body, filename, err := c.Export(ctx, w.ID.String())
	if err != nil || filename != "tensorafi-nb-"+w.ID.String()+".json" {
		t.Fatalf("Export filename %q, %v", filename, err)
	}
	var exp map[string]any
	if err := json.Unmarshal(body, &exp); err != nil || exp["format"] != "tensorafi.workload-export/v1" {
		t.Fatalf("export body: %v %s", err, body)
	}

	other := *c
	other.DevTenant = "globex"
	_, err = other.Events(ctx, w.ID.String())
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 || apiErr.RequestID == "" {
		t.Fatalf("cross-tenant read: %v; want a 404 APIError carrying a request ID", err)
	}
}

// A server, or anyone between it and the user, must not choose where an export is written or what
// it contains beyond JSON.
func TestExportDistrustsTheServer(t *testing.T) {
	const id = "01a16a2a-d622-7d69-94ec-23b0f3327338"
	tests := []struct {
		name, disposition, body, wantName, wantErr string
	}{
		{"expected name kept", "attachment; filename=tensorafi-llm-" + id + ".json", `{}`, "tensorafi-llm-" + id + ".json", ""},
		{"dotfile refused", "attachment; filename=.bashrc", `{}`, "tensorafi-workload-" + id + ".json", ""},
		{"path refused", `attachment; filename="../../.zshrc"`, `{}`, "tensorafi-workload-" + id + ".json", ""},
		{"not JSON", "", "echo pwned\n", "", "isn't valid JSON"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.disposition != "" {
					w.Header().Set("Content-Disposition", tt.disposition)
				}
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)
			c := client.FromEnv(func(k string) string { return map[string]string{"TENSORAFI_API_URL": srv.URL}[k] })
			_, name, err := c.Export(context.Background(), id)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || name != tt.wantName {
				t.Fatalf("name = %q, err = %v; want %q", name, err, tt.wantName)
			}
		})
	}
}

func TestTokenIsNeverSentOverPlainHTTPToAnotherMachine(t *testing.T) {
	ctx := context.Background()
	env := func(vars map[string]string) func(string) string { return func(k string) string { return vars[k] } }

	remote := client.FromEnv(env(map[string]string{"TENSORAFI_API_URL": "http://lab-box.invalid:8080", "TENSORAFI_TOKEN": "tkn"}))
	if _, err := remote.ListWorkloads(ctx, false); err == nil || !strings.Contains(err.Error(), "refusing to send TENSORAFI_TOKEN") {
		t.Fatalf("token over plain HTTP: err = %v", err)
	}

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(srv.Close)
	local := client.FromEnv(env(map[string]string{"TENSORAFI_API_URL": srv.URL, "TENSORAFI_TOKEN": "tkn"}))
	if _, err := local.ListWorkloads(ctx, false); err != nil || got != "Bearer tkn" {
		t.Fatalf("loopback: err = %v, Authorization = %q", err, got)
	}
}

func TestRejectsMalformedWorkloadIDs(t *testing.T) {
	c := client.FromEnv(func(string) string { return "" })
	for _, id := range []string{"../../v1/catalog", "abc", ""} {
		if _, err := c.Events(context.Background(), id); err == nil || !strings.Contains(err.Error(), "not a workload ID") {
			t.Errorf("Events(%q): err = %v", id, err)
		}
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var followed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	c := client.FromEnv(func(k string) string {
		return map[string]string{"TENSORAFI_API_URL": srv.URL, "TENSORAFI_TOKEN": "tkn"}[k]
	})
	if _, err := c.ListWorkloads(context.Background(), false); err == nil || followed {
		t.Fatalf("redirect followed=%v, err=%v; the token must not travel to a redirect target", followed, err)
	}
}

func TestServerErrorTextIsMadePrintable(t *testing.T) {
	err := &client.APIError{Status: 400, Code: "bad\x1b[2J", Message: "evil\x1b]52;c;ZXZpbA==\x07", RequestID: "id\x1b"}
	if strings.ContainsAny(err.Error(), "\x1b\x07") {
		t.Errorf("APIError kept control characters: %q", err.Error())
	}
}
