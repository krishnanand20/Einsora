package service_test

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
	"github.com/krishnanand20/tensorafi/controlplane/internal/service"
	"github.com/krishnanand20/tensorafi/controlplane/internal/store"
	"github.com/krishnanand20/tensorafi/controlplane/internal/testutil"
)

func endpoint(modelURI string) service.CreateWorkloadInput {
	return service.CreateWorkloadInput{Name: "llm", Kind: domain.KindEndpoint, ImageID: "openai-endpoint", GPUClass: "l40s-48gb", GPUCount: 1, ModelURI: modelURI}
}

func TestEventsAndExportAreTenantScoped(t *testing.T) {
	svc, _ := setup(t)
	ctx := context.Background()
	w, err := svc.CreateWorkload(ctx, testutil.Acme, notebook("nb", 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.WorkloadEvents(ctx, testutil.Globex, w.ID); !domain.IsNotFound(err) {
		t.Errorf("other tenant read events: %v", err)
	}
	if _, err := svc.ExportWorkload(ctx, testutil.Globex, w.ID); !domain.IsNotFound(err) {
		t.Errorf("other tenant exported workload: %v", err)
	}
	events, err := svc.WorkloadEvents(ctx, testutil.Acme, w.ID)
	if err != nil || len(events) != 1 || events[0].To != domain.StatePending || !strings.Contains(events[0].Reason, testutil.Acme.Subject) {
		t.Fatalf("owner events = %+v, %v", events, err)
	}
}

func TestDeletedWorkloadsStayExportable(t *testing.T) {
	svc, st := setup(t)
	ctx := context.Background()
	w, err := svc.CreateWorkload(ctx, testutil.Acme, notebook("gone", 1))
	if err != nil {
		t.Fatal(err)
	}
	err = st.InTx(ctx, func(q store.Queries) error {
		for _, s := range []domain.State{domain.StateDeleting, domain.StateDeleted} {
			if err := service.Transition(ctx, q, &w, s, "test", testutil.T0); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetWorkload(ctx, testutil.Acme, w.ID); !domain.IsNotFound(err) {
		t.Fatalf("deleted workload should be gone from normal reads: %v", err)
	}
	exp, err := svc.ExportWorkload(ctx, testutil.Acme, w.ID)
	if err != nil || exp.Workload.State != domain.StateDeleted || len(exp.Events) != 3 {
		t.Fatalf("export of deleted workload = %+v, %v", exp, err)
	}
}

// storedEndpoint writes an endpoint straight to the store, the way data from before create-time
// checks refused credentials would look, so the export's redaction is tested on its own.
func storedEndpoint(t *testing.T, st store.Store, modelURI string) domain.Workload {
	t.Helper()
	ctx := context.Background()
	var w domain.Workload
	err := st.InTx(ctx, func(q store.Queries) error {
		tn, err := q.TenantBySlug(ctx, testutil.Acme.TenantSlug)
		if err != nil {
			return err
		}
		w = domain.Workload{
			ID: uuid.Must(uuid.NewV7()), TenantID: tn.ID, Name: "llm", Kind: domain.KindEndpoint,
			State: domain.StatePending, ImageID: "openai-endpoint", GPUClass: "l40s-48gb", GPUCount: 1,
			Params: map[string]string{domain.ParamModelURI: modelURI}, CreatedBy: testutil.Acme.Subject,
			CreatedAt: testutil.T0, UpdatedAt: testutil.T0, Region: domain.RegionLocal,
		}
		return q.CreateWorkload(ctx, w)
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestExportKeepsNonSecretValuesUnchanged(t *testing.T) {
	svc, _ := setup(t)
	ctx := context.Background()
	w, err := svc.CreateWorkload(ctx, testutil.Internal, endpoint("hf://Qwen/Qwen2.5-0.5B-Instruct"))
	if err != nil {
		t.Fatal(err)
	}
	exp, err := svc.ExportWorkload(ctx, testutil.Internal, w.ID)
	if err != nil || exp.Workload.Params[domain.ParamModelURI] != "hf://Qwen/Qwen2.5-0.5B-Instruct" || len(exp.Redactions) != 0 {
		t.Errorf("non-secret values must be exported unchanged: %+v, %v", exp, err)
	}
}

func TestExportRedactsCredentialsInURIs(t *testing.T) {
	tests := []struct {
		name, uri, want string
		secrets         []string
		redactions      int
	}{
		{"userinfo", "s3://AKIAKEY:supersecret@models/acme/llm", "s3://REDACTED@models/acme/llm", []string{"AKIAKEY", "supersecret"}, 1},
		{"presigned query", "s3://models/acme/llm?X-Amz-Signature=sig123&region=eu", "s3://models/acme/llm?X-Amz-Signature=REDACTED&region=eu", []string{"sig123"}, 1},
		{"token and userinfo", "s3://user:pw@models/llm?access_token=tok999", "s3://REDACTED@models/llm?access_token=REDACTED", []string{":pw@", "tok999"}, 2},
		{"harmless query kept byte for byte", "hf://Qwen/Qwen2.5-0.5B-Instruct?revision=main", "hf://Qwen/Qwen2.5-0.5B-Instruct?revision=main", nil, 0},
		// Values url.Parse rejects or misreads must still be redacted (fail closed).
		{"bad escape in userinfo", "s3://AKIA:se%cret@bucket/m", "s3://REDACTED@bucket/m", []string{"AKIA", "se%cret"}, 1},
		{"bad port", "s3://AKIA:secret@bucket:abc/m", "s3://REDACTED@REDACTED/m", []string{"AKIA", "secret"}, 2},
		{"escaped @", "s3://AKIA:secret%40bucket/m", "s3://REDACTED/m", []string{"AKIA", "secret"}, 1},
		{"unescaped slash in password", "s3://user:pa/ss@bucket/m", "s3://REDACTED@bucket/m", []string{"user:", "pa/ss"}, 1},
		{"semicolon separator", "s3://bucket/m?X-Amz-Signature=sig123;x=1", "s3://bucket/m?X-Amz-Signature=REDACTED;x=1", []string{"sig123"}, 1},
		{"fragment", "s3://bucket/m#X-Amz-Signature=sig123", "s3://bucket/m#X-Amz-Signature=REDACTED", []string{"sig123"}, 1},
		{"bad escape in value", "s3://bucket/m?X-Amz-Signature=sig123%zz", "s3://bucket/m?X-Amz-Signature=REDACTED", []string{"sig123"}, 1},
		{"escaped name", "s3://bucket/m?X%2DAmz-Signature=sig123", "s3://bucket/m?X%2DAmz-Signature=REDACTED", []string{"sig123"}, 1},
		{"rest kept byte for byte", "s3://bucket/m?X-Amz-Credential=cred1&a=1;b=%20x", "s3://bucket/m?X-Amz-Credential=REDACTED&a=1;b=%20x", []string{"cred1"}, 1},
		// Unescaped passwords that url.Parse reads as a port, a path, a query, or a fragment.
		{"password read as port and path", "s3://user:2024/Spring!@bucket/m", "s3://REDACTED@bucket/m", []string{"user:", "2024", "Spring"}, 1},
		{"password with digits and slash", "s3://AKIAXXXX:1234/abc+def@bucket/m", "s3://REDACTED@bucket/m", []string{"AKIAXXXX", "abc+def"}, 1},
		{"password read as query", "s3://user:12?ssword@bucket/m", "s3://REDACTED@bucket/m", []string{"user:", "ssword"}, 1},
		{"password read as fragment", "s3://user:pa#ssword@bucket/m", "s3://REDACTED@bucket/m", []string{"user:", "ssword"}, 1},
		{"@ inside the password", "s3://AKIA:ab@cd/efgh@bucket/m", "s3://REDACTED@bucket/m", []string{"AKIA", "ab@cd", "efgh"}, 1},
		{"azure sas signature", "s3://bucket/m?sv=2022-11-02&sig=abc123", "s3://bucket/m?sv=2022-11-02&sig=REDACTED", []string{"abc123"}, 1},
		{"@ in a harmless query kept", "s3://bucket/m?owner=a@b.example", "s3://bucket/m?owner=a@b.example", nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, st := setup(t)
			w := storedEndpoint(t, st, tt.uri)
			exp, err := svc.ExportWorkload(context.Background(), testutil.Acme, w.ID)
			if err != nil {
				t.Fatal(err)
			}
			got := exp.Workload.Params[domain.ParamModelURI]
			for _, secret := range tt.secrets {
				if strings.Contains(got, secret) {
					t.Fatalf("secret %q leaked into export: %q", secret, got)
				}
			}
			if got != tt.want {
				t.Errorf("model_uri = %q, want %q", got, tt.want)
			}
			if len(exp.Redactions) != tt.redactions {
				t.Errorf("redactions = %+v, want %d; every removal must be reported", exp.Redactions, tt.redactions)
			}
			for _, r := range exp.Redactions {
				if r.Field != "params.model_uri" || r.Reason == "" {
					t.Errorf("redaction %+v must name the field and say why", r)
				}
			}
		})
	}
}

func TestCleanReason(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain text kept", "admitted: 2 × l40s-48gb", "admitted: 2 × l40s-48gb"},
		{"terminal escapes neutralized", "ready\x1b]52;c;ZXZpbA==\x07\x1b[2J", "ready ]52;c;ZXZpbA==  [2J"},
		{"credentials in a URI redacted", `pull "s3://AKIA:hunter2@models/m" failed`, `pull "s3://REDACTED@models/m" failed`},
		{"quote inside the credentials", `pull 's3://AKIA'x:hunter2@models/m' failed`, `pull 's3://REDACTED@models/m' failed`},
	}
	for _, tt := range tests {
		if got := service.CleanReason(tt.in); got != tt.want {
			t.Errorf("%s: CleanReason(%q) = %q, want %q", tt.name, tt.in, got, tt.want)
		}
	}
	long := service.CleanReason(strings.Repeat("é", 3000))
	if !strings.HasSuffix(long, "[cut at 2000 bytes]") || len(long) > 2100 || !utf8.ValidString(long) {
		t.Errorf("long reason not cut cleanly: %d bytes, suffix %q", len(long), long[len(long)-30:])
	}
}
