package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/catalog"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/service"
	"github.com/krishnanand20/einsora/controlplane/internal/store"
	"github.com/krishnanand20/einsora/controlplane/internal/testutil"
)

func setup(t *testing.T) (*service.Service, store.Store) {
	t.Helper()
	st := testutil.SeededStore(t)
	return service.New(st, testutil.Catalog(t), testutil.NewClock(testutil.T0).Now), st
}

func notebook(name string, gpus int) service.CreateWorkloadInput {
	return service.CreateWorkloadInput{Name: name, Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUClass: "l40s-48gb", GPUCount: gpus}
}

func wantCode(t *testing.T, err error, want domain.Code) {
	t.Helper()
	if got, _ := domain.CodeOf(err); got != want {
		t.Fatalf("err = %v, want code %s", err, want)
	}
}

func TestCreateRecordsIntentAndCreationEvent(t *testing.T) {
	svc, st := setup(t)
	ctx := context.Background()
	w, err := svc.CreateWorkload(ctx, testutil.Acme, notebook("nb", 1))
	if err != nil {
		t.Fatal(err)
	}
	if w.State != domain.StatePending || w.CreatedBy != testutil.Acme.Subject {
		t.Fatalf("workload = %+v", w)
	}
	_ = st.InTx(ctx, func(q store.Queries) error {
		evs, _ := q.Events(ctx, w.ID)
		if len(evs) != 1 || evs[0].To != domain.StatePending {
			t.Errorf("events = %+v", evs)
		}
		return nil
	})
}

func TestQuotaIsEnforced(t *testing.T) {
	svc, _ := setup(t)
	ctx := context.Background()
	if _, err := svc.CreateWorkload(ctx, testutil.Acme, notebook("a", 2)); err != nil {
		t.Fatal(err)
	}
	_, err := svc.CreateWorkload(ctx, testutil.Acme, notebook("b", 1)) // acme's l40s quota is 2
	wantCode(t, err, domain.CodeQuotaExceeded)
}

func TestOtherTenantsSeeNotFound(t *testing.T) {
	svc, _ := setup(t)
	ctx := context.Background()
	w, err := svc.CreateWorkload(ctx, testutil.Acme, notebook("secret", 1))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.GetWorkload(ctx, testutil.Globex, w.ID)
	wantCode(t, err, domain.CodeNotFound)
	_, err = svc.DeleteWorkload(ctx, testutil.Globex, w.ID)
	wantCode(t, err, domain.CodeNotFound)
	list, err := svc.ListWorkloads(ctx, testutil.Globex, true)
	if err != nil || len(list) != 0 {
		t.Fatalf("globex list = %v, %v", list, err)
	}
}

func TestUnknownTenantIsForbidden(t *testing.T) {
	svc, _ := setup(t)
	_, err := svc.ListWorkloads(context.Background(), domain.Principal{Subject: "x", TenantSlug: "nobody"}, false)
	wantCode(t, err, domain.CodeForbidden)
}

func TestInputValidation(t *testing.T) {
	vm := service.CreateWorkloadInput{Name: "vm", Kind: domain.KindVM, ImageID: "ubuntu-24.04-cuda", GPUClass: "l40s-48gb", GPUCount: 1, SSHPublicKey: testutil.SSHKey}
	ep := service.CreateWorkloadInput{Name: "ep", Kind: domain.KindEndpoint, ImageID: "openai-endpoint", GPUClass: "l40s-48gb", GPUCount: 1, ModelURI: "hf://Qwen/Qwen2.5-0.5B-Instruct"}
	tests := []struct {
		name   string
		mutate func(*service.CreateWorkloadInput)
		base   service.CreateWorkloadInput
		who    domain.Principal
		want   domain.Code // "" = success
	}{
		{"valid vm", func(*service.CreateWorkloadInput) {}, vm, testutil.Acme, ""},
		{"valid endpoint", func(*service.CreateWorkloadInput) {}, ep, testutil.Acme, ""},
		{"vm without key", func(in *service.CreateWorkloadInput) { in.SSHPublicKey = "" }, vm, testutil.Acme, domain.CodeInvalid},
		{"key with newline", func(in *service.CreateWorkloadInput) { in.SSHPublicKey += "\nruncmd: [evil]" }, vm, testutil.Acme, domain.CodeInvalid},
		{"vm with model", func(in *service.CreateWorkloadInput) { in.ModelURI = "hf://x/y" }, vm, testutil.Acme, domain.CodeInvalid},
		{"http model uri", func(in *service.CreateWorkloadInput) { in.ModelURI = "http://evil/model" }, ep, testutil.Acme, domain.CodeInvalid},
		{"credentials in model uri", func(in *service.CreateWorkloadInput) { in.ModelURI = "s3://AKIA:secret@models/m" }, ep, testutil.Acme, domain.CodeInvalid},
		{"unparseable model uri", func(in *service.CreateWorkloadInput) { in.ModelURI = "s3://AKIA:secret%40models/m" }, ep, testutil.Acme, domain.CodeInvalid},
		{"password read as a port", func(in *service.CreateWorkloadInput) { in.ModelURI = "s3://user:2024/Spring!@bucket/m" }, ep, testutil.Acme, domain.CodeInvalid},
		{"port in model uri", func(in *service.CreateWorkloadInput) { in.ModelURI = "s3://bucket:9000/m" }, ep, testutil.Acme, domain.CodeInvalid},
		{"@ in model uri", func(in *service.CreateWorkloadInput) { in.ModelURI = "hf://Qwen/Qwen2.5@main" }, ep, testutil.Acme, domain.CodeInvalid},
		{"model uri with revision", func(in *service.CreateWorkloadInput) { in.ModelURI = "hf://Qwen/Qwen2.5-0.5B-Instruct?revision=main" }, ep, testutil.Acme, ""},
		{"bad name", func(in *service.CreateWorkloadInput) { in.Name = "Bad_Name" }, ep, testutil.Acme, domain.CodeInvalid},
		{"zero gpus", func(in *service.CreateWorkloadInput) { in.GPUCount = 0 }, ep, testutil.Acme, domain.CodeInvalid},
		{"too many gpus", func(in *service.CreateWorkloadInput) { in.GPUCount = 9 }, ep, testutil.Acme, domain.CodeInvalid},
		{"image kind mismatch", func(in *service.CreateWorkloadInput) { in.ImageID = "pytorch-notebook" }, ep, testutil.Acme, domain.CodeInvalid},
		{"unknown gpu", func(in *service.CreateWorkloadInput) { in.GPUClass = "h200" }, ep, testutil.Acme, domain.CodeInvalid},
		{"internal-only gpu, untrusted tenant", func(in *service.CreateWorkloadInput) { in.GPUClass = "rtx4090-24gb" }, ep, testutil.Acme, domain.CodeInvalid},
		{"internal-only gpu, trusted tenant", func(in *service.CreateWorkloadInput) { in.GPUClass = "rtx4090-24gb" }, ep, testutil.Internal, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _ := setup(t)
			in := tt.base
			tt.mutate(&in)
			_, err := svc.CreateWorkload(context.Background(), tt.who, in)
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			wantCode(t, err, tt.want)
		})
	}
}

func TestStartRechecksQuota(t *testing.T) {
	svc, st := setup(t)
	ctx := context.Background()
	first, err := svc.CreateWorkload(ctx, testutil.Acme, notebook("first", 2))
	if err != nil {
		t.Fatal(err)
	}
	// Drive it to stopped the way the controller would: placed, then admitted.
	err = st.InTx(ctx, func(q store.Queries) error {
		if err := q.SetPlacement(ctx, first.ID, first.GPUClass, uuid.Nil, ""); err != nil {
			return err
		}
		first.PlacedClass = first.GPUClass
		for _, s := range []domain.State{domain.StateProvisioning, domain.StateStopping, domain.StateStopped} {
			if err := service.Transition(ctx, q, &first, s, "test", testutil.T0); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateWorkload(ctx, testutil.Acme, notebook("second", 2)); err != nil {
		t.Fatalf("stopped workloads should not hold quota: %v", err)
	}
	_, err = svc.StartWorkload(ctx, testutil.Acme, first.ID)
	wantCode(t, err, domain.CodeQuotaExceeded)
}

func TestStopRequiresRunningState(t *testing.T) {
	svc, _ := setup(t)
	ctx := context.Background()
	w, _ := svc.CreateWorkload(ctx, testutil.Acme, notebook("nb", 1))
	_, err := svc.StopWorkload(ctx, testutil.Acme, w.ID)
	wantCode(t, err, domain.CodeInvalidTransition)
}

func TestDeleteIsIdempotent(t *testing.T) {
	svc, _ := setup(t)
	ctx := context.Background()
	w, _ := svc.CreateWorkload(ctx, testutil.Acme, notebook("nb", 1))
	for range 2 {
		got, err := svc.DeleteWorkload(ctx, testutil.Acme, w.ID)
		if err != nil || got.State != domain.StateDeleting {
			t.Fatalf("delete = %+v, %v", got, err)
		}
	}
}

// Region and runtime checks at create time (ADR-0009, ADR-0013), against a two-region catalog.
func TestCreateChecksRegionAndRuntime(t *testing.T) {
	cat := &catalog.Catalog{
		Regions: []catalog.Region{{ID: "local"}, {ID: "eu-fra-1"}},
		GPUClasses: []catalog.GPUClass{
			{ID: "l40s", Accelerator: domain.AcceleratorNVIDIA, VRAMGB: 48, ContainerResource: "nvidia.com/gpu", VCPUsPerGPU: 8, MemoryGiBPerGPU: 64},
			{ID: "fra-only", Accelerator: domain.AcceleratorNVIDIA, VRAMGB: 80, ContainerResource: "nvidia.com/gpu", VCPUsPerGPU: 8, MemoryGiBPerGPU: 64, Regions: []domain.Region{"eu-fra-1"}},
			{ID: "cpu", Accelerator: domain.AcceleratorCPU, VCPUsPerGPU: 4, MemoryGiBPerGPU: 16},
		},
		Images: []catalog.Image{{ID: "nb", Kind: domain.KindNotebook, Runtimes: []catalog.Runtime{
			{Accelerators: []domain.AcceleratorKind{domain.AcceleratorNVIDIA}, Ref: "cuda-notebook"},
		}}},
	}
	if err := cat.Validate(); err != nil {
		t.Fatal(err)
	}
	st := testutil.SeededStore(t)
	ctx := context.Background()
	err := st.InTx(ctx, func(q store.Queries) error {
		tn, err := q.TenantBySlug(ctx, "internal")
		if err != nil {
			return err
		}
		for _, class := range []string{"l40s", "fra-only", "cpu"} {
			if err := q.SetQuota(ctx, domain.Quota{TenantID: tn.ID, GPUClass: class, MaxGPUs: 4}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, cat, testutil.NewClock(testutil.T0).Now)
	in := func(class string, region domain.Region) service.CreateWorkloadInput {
		return service.CreateWorkloadInput{Name: "nb", Kind: domain.KindNotebook, ImageID: "nb", GPUClass: class, GPUCount: 1, Region: region}
	}

	w, err := svc.CreateWorkload(ctx, testutil.Internal, in("l40s", ""))
	if err != nil || w.Region != "local" {
		t.Fatalf("default region: %+v, %v; want local, the catalog's first region", w, err)
	}
	if w, err := svc.CreateWorkload(ctx, testutil.Internal, in("fra-only", "eu-fra-1")); err != nil || w.Region != "eu-fra-1" {
		t.Fatalf("named region: %+v, %v", w, err)
	}
	for name, tt := range map[string]struct {
		input service.CreateWorkloadInput
		want  string
	}{
		"unknown region":                 {in("l40s", "us-east-1"), "unknown region"},
		"class not offered there":        {in("fra-only", "local"), "isn't offered in region"},
		"no runtime for the accelerator": {in("cpu", ""), "can't run on"},
	} {
		_, err := svc.CreateWorkload(ctx, testutil.Internal, tt.input)
		if code, _ := domain.CodeOf(err); code != domain.CodeInvalid || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want invalid_request mentioning %q", name, err, tt.want)
		}
	}
}

// The launch rules are checked again at start: an untrusted tenant can't restart a workload that
// runs natively on a Mac, even one that exists because the catalog or its tier changed (rule 4).
func TestStartRechecksLaunchRules(t *testing.T) {
	svc, st := setup(t)
	ctx := context.Background()
	var id uuid.UUID
	err := st.InTx(ctx, func(q store.Queries) error {
		tn, err := q.TenantBySlug(ctx, testutil.Acme.TenantSlug) // untrusted
		if err != nil {
			return err
		}
		if err := q.SetQuota(ctx, domain.Quota{TenantID: tn.ID, GPUClass: "apple-silicon-32gb", MaxGPUs: 1}); err != nil {
			return err
		}
		id = uuid.Must(uuid.NewV7())
		return q.CreateWorkload(ctx, domain.Workload{
			ID: id, TenantID: tn.ID, Name: "mac-nb", Kind: domain.KindNotebook, State: domain.StateFailed,
			ImageID: "pytorch-notebook", GPUClass: "apple-silicon-32gb", GPUCount: 1, Params: map[string]string{},
			CreatedBy: "test", CreatedAt: testutil.T0, UpdatedAt: testutil.T0, Region: domain.RegionLocal, PlacedClass: "apple-silicon-32gb",
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.StartWorkload(ctx, testutil.Acme, id)
	wantCode(t, err, domain.CodeInvalid)
}
