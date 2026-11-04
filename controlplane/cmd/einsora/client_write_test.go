package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krishnanand20/einsora/controlplane/internal/api"
	"github.com/krishnanand20/einsora/controlplane/internal/auth"
	"github.com/krishnanand20/einsora/controlplane/internal/cluster/fake"
	"github.com/krishnanand20/einsora/controlplane/internal/controller"
	"github.com/krishnanand20/einsora/controlplane/internal/service"
	"github.com/krishnanand20/einsora/controlplane/internal/testutil"
)

// cliHarness is the real API, with a controller on the fake backend to move workloads along, and
// the CLI pointed at it as tenant acme.
type cliHarness struct {
	t    *testing.T
	ctrl *controller.Controller
}

func newCLIHarness(t *testing.T) *cliHarness {
	t.Helper()
	st := testutil.SeededStore(t)
	cat := testutil.Catalog(t)
	svc := service.New(st, cat, nil)
	srv := httptest.NewServer(api.New(svc, auth.Dev{}, slog.New(slog.DiscardHandler), st.Ping).Handler())
	t.Cleanup(srv.Close)
	t.Setenv("EINSORA_API_URL", srv.URL)
	t.Setenv("EINSORA_DEV_TENANT", "acme")
	ctrl := controller.New(st, fake.New(map[string]int{"l40s-48gb": 8}), cat,
		controller.Options{ProvisionTimeout: 10 * time.Minute}, slog.New(slog.DiscardHandler), nil)
	return &cliHarness{t: t, ctrl: ctrl}
}

// run runs a client command, answering any prompt with answer, and returns its output.
func (h *cliHarness) run(answer string, args ...string) (string, error) {
	h.t.Helper()
	var out, prompts bytes.Buffer
	term := cli{in: strings.NewReader(answer), out: &out, errOut: &prompts, interactive: answer != ""}
	err := runClient(context.Background(), args[0], args[1:], term)
	return out.String() + prompts.String(), err
}

func (h *cliHarness) must(args ...string) string {
	h.t.Helper()
	out, err := h.run("", args...)
	if err != nil {
		h.t.Fatalf("einsora %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func (h *cliHarness) reconcile() {
	h.t.Helper()
	for range 3 {
		if err := h.ctrl.ReconcileOnce(context.Background()); err != nil {
			h.t.Fatal(err)
		}
	}
}

// launch, stop, start, and delete take a workload through its life by name, as the API does it.
func TestCLILaunchStopStartDelete(t *testing.T) {
	h := newCLIHarness(t)
	out := h.must("launch", "notebook", "lab", "-image", "pytorch-notebook", "-class", "l40s-48gb")
	if !strings.Contains(out, "Launched lab (") || !strings.Contains(out, "pending, 1 × l40s-48gb in ") {
		t.Fatalf("launch output:\n%s", out)
	}
	h.reconcile()
	if out := h.must("workloads"); !strings.Contains(out, "running") {
		t.Fatalf("not running after reconciling:\n%s", out)
	}

	if out := h.must("stop", "lab"); !strings.Contains(out, "lab (") || !strings.Contains(out, ": stopping") {
		t.Errorf("stop output: %q", out)
	}
	h.reconcile()
	if out := h.must("start", "lab"); !strings.Contains(out, ": pending") {
		t.Errorf("start output: %q", out)
	}
	if _, err := h.run("", "start", "lab"); err == nil || !strings.Contains(err.Error(), "only stopped or failed workloads can be started") {
		t.Errorf("starting a pending workload: %v", err)
	}

	// Delete asks first; with no terminal to ask at, it needs -y.
	if _, err := h.run("", "delete", "lab"); err == nil || !strings.Contains(err.Error(), "add -y") {
		t.Errorf("delete with no terminal: %v", err)
	}
	out, err := h.run("n\n", "delete", "lab")
	if err == nil || err.Error() != "not deleted" || !strings.Contains(out, "Delete lab (") {
		t.Errorf("delete answered no: %v\n%s", err, out)
	}
	if out, err := h.run("y\n", "delete", "lab"); err != nil || !strings.Contains(out, ": deleting") {
		t.Errorf("delete answered yes: %v\n%s", err, out)
	}
	h.reconcile()

	// A deleted workload's name still finds its history.
	if out := h.must("events", "lab"); !strings.Contains(out, "delete requested by") {
		t.Errorf("events of a deleted workload by name:\n%s", out)
	}
	if _, err := h.run("", "stop", "lab"); err == nil || !strings.Contains(err.Error(), "no workload named") {
		t.Errorf("stop of a deleted workload by name: %v", err)
	}
}

// Names can repeat: a shared name is refused with the IDs to choose from, and an ID always works.
func TestCLIAmbiguousNameAndIDs(t *testing.T) {
	h := newCLIHarness(t)
	var ids []string
	for range 2 {
		out := h.must("launch", "notebook", "twin", "-image", "pytorch-notebook", "-class", "l40s-48gb", "-json")
		var w struct {
			ID     string `json:"id"`
			Region string `json:"region"`
		}
		if err := json.Unmarshal([]byte(out), &w); err != nil || w.ID == "" || w.Region == "" {
			t.Fatalf("launch -json: %v\n%s", err, out)
		}
		ids = append(ids, w.ID)
	}
	_, err := h.run("", "delete", "twin", "-y")
	if err == nil || !strings.Contains(err.Error(), "2 workloads are named") || !strings.Contains(err.Error(), ids[1]) {
		t.Fatalf("ambiguous name: %v", err)
	}
	if out := h.must("delete", ids[0], "-y", "-json"); !strings.Contains(out, `"state": "deleting"`) {
		t.Errorf("delete by ID -json:\n%s", out)
	}
	for _, bad := range []string{"Twin", "../twin", "-"} {
		if _, err := h.run("", "stop", bad); err == nil {
			t.Errorf("stop %q was accepted", bad)
		}
	}
}

// Another tenant's workload is never found, by name or by ID.
func TestCLIOtherTenantsWorkload(t *testing.T) {
	h := newCLIHarness(t)
	out := h.must("launch", "notebook", "mine", "-image", "pytorch-notebook", "-class", "l40s-48gb", "-json")
	var w struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &w); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EINSORA_DEV_TENANT", "globex")
	if _, err := h.run("", "delete", "mine", "-y"); err == nil || !strings.Contains(err.Error(), "no workload named") {
		t.Errorf("another tenant by name: %v", err)
	}
	if _, err := h.run("", "delete", w.ID, "-y"); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("another tenant by ID: %v", err)
	}
	if _, err := h.run("", "events", w.ID); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("another tenant's events by ID: %v", err)
	}
}

func TestCLILaunchArguments(t *testing.T) {
	h := newCLIHarness(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"launch", "notebook"}, "missing arguments"},
		{[]string{"launch", "notebook", "a", "b", "-image", "x", "-class", "y"}, `unexpected argument "b"`},
		{[]string{"launch", "notebook", "a"}, "-image and -class are required"},
		{[]string{"launch", "notebook", "a", "-image", "pytorch-notebook", "-class", "nope"}, "HTTP 400"},
		{[]string{"launch", "endpoint", "e", "-image", "openai-endpoint", "-class", "l40s-48gb"}, "model_uri must be"},
		{[]string{"stop", "lab", "-y"}, "-y is only for delete"},
	} {
		if _, err := h.run("", tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: error %v, want %q", tc.args, err, tc.want)
		}
	}
	// Flags can come before, between, and after the kind and name.
	out := h.must("launch", "-class", "l40s-48gb", "endpoint", "-model", "hf://org/model", "chat", "-image", "openai-endpoint", "-gpus", "2")
	if !strings.Contains(out, "Launched chat (") || !strings.Contains(out, "2 × l40s-48gb") {
		t.Errorf("launch with flags around the arguments:\n%s", out)
	}
}

func TestCLICatalog(t *testing.T) {
	h := newCLIHarness(t)
	out := h.must("catalog")
	for _, want := range []string{"REGION", "(default)", "l40s-48gb", "nvidia", "48 GB", "pytorch-notebook", "notebook", "openai-endpoint", "vllm (safetensors)"} {
		if !strings.Contains(out, want) {
			t.Errorf("catalog output is missing %q:\n%s", want, out)
		}
	}
	var cat struct {
		GPUClasses []json.RawMessage `json:"gpu_classes"`
		Images     []json.RawMessage `json:"images"`
	}
	if err := json.Unmarshal([]byte(h.must("catalog", "-json")), &cat); err != nil || len(cat.GPUClasses) == 0 || len(cat.Images) == 0 {
		t.Errorf("catalog -json: %v %+v", err, cat)
	}
}

// -json prints the API's objects whole, including fields the table leaves out.
func TestCLIListsAsJSON(t *testing.T) {
	h := newCLIHarness(t)
	h.must("launch", "notebook", "lab", "-image", "pytorch-notebook", "-class", "l40s-48gb")
	h.reconcile()
	var ws []map[string]any
	if err := json.Unmarshal([]byte(h.must("workloads", "-json")), &ws); err != nil || len(ws) != 1 {
		t.Fatalf("workloads -json: %v %v", err, ws)
	}
	if ws[0]["name"] != "lab" || ws[0]["placed_class"] != "l40s-48gb" {
		t.Errorf("workloads -json lost fields: %v", ws[0])
	}
	var ms []any
	if err := json.Unmarshal([]byte(h.must("machines", "-json")), &ms); err != nil || ms == nil {
		t.Errorf("machines -json with no machines should be []: %v %v", err, ms)
	}
}

func TestSSHPublicKey(t *testing.T) {
	dir := t.TempDir()
	const pub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl you@laptop"
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if got, err := sshPublicKey(pub); err != nil || got != pub {
		t.Errorf("a key line: %q, %v", got, err)
	}
	if got, err := sshPublicKey(write("id.pub", pub+"\n")); err != nil || got != pub {
		t.Errorf("a .pub file: %q, %v", got, err)
	}
	// A fake key, its header built at run time so secret scanners don't mistake the test for a key.
	header := "OPENSSH " + "PRIVATE KEY"
	private := "-----BEGIN " + header + "-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END " + header + "-----\n"
	_, err := sshPublicKey(write("id", private))
	if err == nil || !strings.Contains(err.Error(), "is a private key") || strings.Contains(err.Error(), "b3BlbnNzaC1") {
		t.Errorf("a private key file: %v", err)
	}
	if _, err := sshPublicKey(write("two", pub+"\n"+pub+"\n")); err == nil {
		t.Error("a file of two keys was accepted")
	}
	if _, err := sshPublicKey(pub + "\nssh-rsa AAAA"); err == nil {
		t.Error("a key line with a newline was accepted")
	}
	if _, err := sshPublicKey(filepath.Join(dir, "missing.pub")); err == nil {
		t.Error("a missing file was accepted")
	}
}
