// Package client is a small Go client for the Einsora API, used by the `einsora` CLI's client
// commands (workloads, launch, stop, events, export, and the rest). It only speaks the public /v1 API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

type Client struct {
	BaseURL   string
	Token     string // bearer token (OIDC or, later, API tokens)
	DevTenant string // X-Dev-Tenant, for dev-mode servers only
	// InsecureHTTP allows sending Token over plain http:// to another machine. Off by default,
	// because anyone on the network path could read the token.
	InsecureHTTP bool
	HTTP         *http.Client
}

// FromEnv reads EINSORA_API_URL, EINSORA_TOKEN, EINSORA_DEV_TENANT, and EINSORA_INSECURE_HTTP.
func FromEnv(getenv func(string) string) *Client {
	base := getenv("EINSORA_API_URL")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	insecure, _ := strconv.ParseBool(getenv("EINSORA_INSECURE_HTTP"))
	return &Client{
		BaseURL:      strings.TrimRight(base, "/"),
		Token:        getenv("EINSORA_TOKEN"),
		DevTenant:    getenv("EINSORA_DEV_TENANT"),
		InsecureHTTP: insecure,
		HTTP: &http.Client{
			Timeout: 60 * time.Second,
			// The API never redirects. Following one could carry the token to plain HTTP or another host.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// tokenSafe reports whether the token may be sent to u: over https, to this machine, or when
// the user has opted in to plain HTTP.
func (c *Client) tokenSafe(u *url.URL) bool {
	if u.Scheme == "https" || c.InsecureHTTP || u.Hostname() == "localhost" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}

// workloadPath builds an API path for a workload, refusing anything that isn't a workload ID.
func workloadPath(id, suffix string) (string, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		return "", fmt.Errorf("%q is not a workload ID (run `einsora workloads` to list them)", id)
	}
	return "/v1/workloads/" + u.String() + suffix, nil
}

// APIError is an error response from the API, including the request ID to give an operator.
type APIError struct {
	Status    int
	Code      string
	Message   string
	RequestID string
}

// Error is printed to a terminal, so the server's text is made printable first.
func (e *APIError) Error() string {
	msg := fmt.Sprintf("%s (HTTP %d, %s)", Printable(e.Message), e.Status, Printable(e.Code))
	if e.RequestID != "" {
		msg += ", request ID " + Printable(e.RequestID)
	}
	return msg
}

// Printable makes server-supplied text safe for a terminal: control characters, which could
// carry escape sequences, are shown as spaces.
func Printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

type Workload struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	State       string    `json:"state"`
	ImageID     string    `json:"image_id"`
	GPUClass    string    `json:"gpu_class"`
	PlacedClass string    `json:"placed_class"`
	Region      string    `json:"region"`
	GPUCount    int       `json:"gpu_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// JSON is the workload exactly as the server sent it, with every field, for -json output.
	JSON json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps the server's JSON beside the fields this client knows.
func (w *Workload) UnmarshalJSON(b []byte) error {
	type fields Workload
	if err := json.Unmarshal(b, (*fields)(w)); err != nil {
		return err
	}
	w.JSON = append(json.RawMessage(nil), b...)
	return nil
}

type Event struct {
	ID     int64     `json:"id"`
	From   string    `json:"from"`
	To     string    `json:"to"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// do sends a request, with in as its JSON body if it isn't nil, and returns the response if it
// succeeded, or the API's error.
func (c *Client) do(ctx context.Context, method, path string, in any) (*http.Response, error) {
	var reqBody io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reqBody)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		if !c.tokenSafe(req.URL) {
			return nil, fmt.Errorf("refusing to send EINSORA_TOKEN over plain HTTP to %s: use https://, "+
				"or set EINSORA_INSECURE_HTTP=true if you trust every network in between", req.URL.Host)
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.DevTenant != "" {
		req.Header.Set("X-Dev-Tenant", c.DevTenant)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach %s: %w", c.BaseURL, err)
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body)
	apiErr := &APIError{Status: resp.StatusCode, Code: body.Error.Code, Message: body.Error.Message, RequestID: body.Error.RequestID}
	if apiErr.RequestID == "" {
		apiErr.RequestID = resp.Header.Get("X-Request-ID")
	}
	if apiErr.Message == "" {
		apiErr.Message = http.StatusText(resp.StatusCode)
	}
	return nil, apiErr
}

// doJSON sends a request (see do) and decodes the response into dst.
func (c *Client) doJSON(ctx context.Context, method, path string, in, dst any) error {
	resp, err := c.do(ctx, method, path, in)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return json.NewDecoder(resp.Body).Decode(dst)
}

func (c *Client) getJSON(ctx context.Context, path string, dst any) error {
	return c.doJSON(ctx, http.MethodGet, path, nil, dst)
}

// ListWorkloads lists the caller's workloads; includeDeleted adds deleted ones.
func (c *Client) ListWorkloads(ctx context.Context, includeDeleted bool) ([]Workload, error) {
	var out struct {
		Items []Workload `json:"items"`
	}
	path := "/v1/workloads"
	if includeDeleted {
		path += "?include_deleted=true"
	}
	err := c.getJSON(ctx, path, &out)
	return out.Items, err
}

// Machine is one of the caller's own machines.
type Machine struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Region       string        `json:"region"`
	OS           string        `json:"os"`
	Arch         string        `json:"arch"`
	CPUs         int           `json:"cpus"`
	MemoryMiB    int64         `json:"memory_mib"`
	Accelerators []Accelerator `json:"accelerators"`
	Sharing      string        `json:"sharing"`
	Status       string        `json:"status"`
	LastSeenAt   *time.Time    `json:"last_seen_at"`
	Workloads    int           `json:"workloads"`
	// JSON is the machine exactly as the server sent it, for -json output.
	JSON json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps the server's JSON beside the fields this client knows.
func (m *Machine) UnmarshalJSON(b []byte) error {
	type fields Machine
	if err := json.Unmarshal(b, (*fields)(m)); err != nil {
		return err
	}
	m.JSON = append(json.RawMessage(nil), b...)
	return nil
}

// Accelerator is a machine's GPU (or Apple Silicon GPU), as its agent reported it.
type Accelerator struct {
	Kind      string `json:"kind"`
	Model     string `json:"model"`
	Count     int    `json:"count"`
	MemoryMiB int64  `json:"memory_mib"`
}

// ListMachines lists the caller's machines.
func (c *Client) ListMachines(ctx context.Context) ([]Machine, error) {
	var out struct {
		Items []Machine `json:"items"`
	}
	err := c.getJSON(ctx, "/v1/machines", &out)
	return out.Items, err
}

// Events returns a workload's complete timeline, oldest first.
func (c *Client) Events(ctx context.Context, id string) ([]Event, error) {
	path, err := workloadPath(id, "/events")
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []Event `json:"items"`
	}
	err = c.getJSON(ctx, path, &out)
	return out.Items, err
}

// MaxExportBytes bounds an export download, so a misbehaving server can't exhaust memory.
const MaxExportBytes = 256 << 20

// exportName is the only file name a server may suggest: the workload's name and ID.
var exportName = regexp.MustCompile(`^einsora-[a-z0-9-]{1,40}-[0-9a-f-]{36}\.json$`)

// Export downloads the complete export of a workload. The body is returned byte-for-byte as the
// server sent it, so nothing is lost or reformatted. The suggested file name is used only if it
// has the expected shape; the server never chooses where a file goes or what it overwrites.
func (c *Client) Export(ctx context.Context, id string) (body []byte, filename string, err error) {
	path, err := workloadPath(id, "/export")
	if err != nil {
		return nil, "", err
	}
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if body, err = io.ReadAll(io.LimitReader(resp.Body, MaxExportBytes+1)); err != nil {
		return nil, "", err
	}
	if len(body) > MaxExportBytes {
		return nil, "", fmt.Errorf("export is larger than %d MiB; download it from the API directly", MaxExportBytes>>20)
	}
	if !json.Valid(body) {
		return nil, "", errors.New("the server's export isn't valid JSON, so nothing was saved")
	}
	if _, params, perr := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); perr == nil && exportName.MatchString(params["filename"]) {
		filename = params["filename"]
	} else {
		filename = "einsora-workload-" + uuid.MustParse(id).String() + ".json" // id was checked by workloadPath
	}
	return body, filename, nil
}

// GetWorkload returns one of the caller's workloads, deleted or not.
func (c *Client) GetWorkload(ctx context.Context, id string) (Workload, error) {
	path, err := workloadPath(id, "?include_deleted=true")
	if err != nil {
		return Workload{}, err
	}
	var w Workload
	err = c.getJSON(ctx, path, &w)
	return w, err
}

// LaunchInput is a new workload, as POST /v1/workloads takes it.
type LaunchInput struct {
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	ImageID      string `json:"image_id"`
	GPUClass     string `json:"gpu_class"`
	GPUCount     int    `json:"gpu_count"`
	Region       string `json:"region,omitempty"`         // empty: the catalog's default
	ModelURI     string `json:"model_uri,omitempty"`      // endpoints only
	SSHPublicKey string `json:"ssh_public_key,omitempty"` // VMs only
}

// Launch creates a workload. It starts pending: the controller places it and starts it.
func (c *Client) Launch(ctx context.Context, in LaunchInput) (Workload, error) {
	var w Workload
	err := c.doJSON(ctx, http.MethodPost, "/v1/workloads", in, &w)
	return w, err
}

// Stop releases a workload's GPUs; a VM or notebook keeps its disk.
func (c *Client) Stop(ctx context.Context, id string) (Workload, error) {
	return c.act(ctx, http.MethodPost, id, "/stop")
}

// Start queues a stopped or failed workload to run again.
func (c *Client) Start(ctx context.Context, id string) (Workload, error) {
	return c.act(ctx, http.MethodPost, id, "/start")
}

// Delete removes a workload and everything it stored; its timeline and export stay.
func (c *Client) Delete(ctx context.Context, id string) (Workload, error) {
	return c.act(ctx, http.MethodDelete, id, "")
}

// act asks for a change to a workload and returns its state once the request is accepted.
func (c *Client) act(ctx context.Context, method, id, suffix string) (Workload, error) {
	path, err := workloadPath(id, suffix)
	if err != nil {
		return Workload{}, err
	}
	var w Workload
	err = c.doJSON(ctx, method, path, nil, &w)
	return w, err
}

// workloadName is the shape of a workload's name, so only a name or an ID is ever looked up.
var workloadName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// WorkloadID returns the ID of the workload ref names: ref itself if it's an ID, or the ID of the
// caller's one workload with that name. Names can repeat, so a name shared by several is refused
// with their IDs. withDeleted also looks among deleted workloads, after the ones that aren't.
func (c *Client) WorkloadID(ctx context.Context, ref string, withDeleted bool) (string, error) {
	if id, err := uuid.Parse(ref); err == nil {
		return id.String(), nil
	}
	if !workloadName.MatchString(ref) {
		return "", fmt.Errorf("%q is neither a workload's name nor its ID (run `einsora workloads` to list them)", Printable(ref))
	}
	ws, err := c.ListWorkloads(ctx, withDeleted)
	if err != nil {
		return "", err
	}
	var live, deleted []string
	for _, w := range ws {
		switch {
		case w.Name != ref:
		case w.State == "deleted":
			deleted = append(deleted, w.ID)
		default:
			live = append(live, w.ID)
		}
	}
	found := live
	if len(found) == 0 {
		found = deleted
	}
	switch len(found) {
	case 0:
		if withDeleted {
			return "", fmt.Errorf("you have no workload named %q (run `einsora workloads -all` to list them)", ref)
		}
		return "", fmt.Errorf("you have no workload named %q (run `einsora workloads` to list them; a deleted one needs its ID)", ref)
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("%d workloads are named %q; give one of their IDs: %s", len(found), ref, strings.Join(found, ", "))
}

// Catalog is what this installation offers the caller: regions (the first is the default), the
// GPU classes they may use, and images.
type Catalog struct {
	Regions    []Region   `json:"regions"`
	GPUClasses []GPUClass `json:"gpu_classes"`
	Images     []Image    `json:"images"`
	// JSON is the catalog exactly as the server sent it, for -json output.
	JSON json.RawMessage `json:"-"`
}

// Region is where workloads run and their data stays.
type Region struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// GPUClass is a kind of accelerator a workload asks for, or a CPU class (VRAMGB 0).
type GPUClass struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"display_name"`
	Accelerator string   `json:"accelerator"`
	VRAMGB      int      `json:"vram_gb"`
	SupportsVM  bool     `json:"supports_vm"`
	Regions     []string `json:"regions"`
}

// Image is something to launch, with what runs it on which accelerators.
type Image struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	Kind        string    `json:"kind"`
	Description string    `json:"description"`
	Runtimes    []Runtime `json:"runtimes"`
}

// Runtime is how an image runs on some accelerators: for an endpoint, its server and the model
// format it loads.
type Runtime struct {
	Accelerators []string `json:"accelerators"`
	Server       string   `json:"server"`
	ModelFormat  string   `json:"model_format"`
}

// Catalog returns what the caller can launch.
func (c *Client) Catalog(ctx context.Context) (Catalog, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/catalog", nil)
	if err != nil {
		return Catalog{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return Catalog{}, err
	}
	var cat Catalog
	if err := json.Unmarshal(raw, &cat); err != nil {
		return Catalog{}, fmt.Errorf("the server's catalog isn't valid JSON: %w", err)
	}
	cat.JSON = raw
	return cat, nil
}

// OutputChunk is a piece of a workload's output: from its stdout or stderr, or a note from
// Einsora (stream "einsora"), such as output that was lost.
type OutputChunk struct {
	ID         int64     `json:"id"`
	Stream     string    `json:"stream"`
	At         time.Time `json:"at"`
	Text       string    `json:"text"`
	Redactions int       `json:"redactions"`
	// JSON is the chunk exactly as the server sent it, for -json output.
	JSON json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps the server's JSON beside the fields this client knows.
func (o *OutputChunk) UnmarshalJSON(b []byte) error {
	type fields OutputChunk
	if err := json.Unmarshal(b, (*fields)(o)); err != nil {
		return err
	}
	o.JSON = append(json.RawMessage(nil), b...)
	return nil
}

// OutputPage is a page of a workload's output. Next is where to read on from; Notes say what
// isn't collected, or was lost or removed.
type OutputPage struct {
	Items     []OutputChunk `json:"items"`
	Next      int64         `json:"next"`
	Collected bool          `json:"collected"`
	Notes     []string      `json:"notes"`
}

// Output returns a page of a workload's output after the chunk ID after. With follow, the server
// waits a while for new output when there's none yet.
func (c *Client) Output(ctx context.Context, id string, after int64, follow bool) (OutputPage, error) {
	path, err := workloadPath(id, fmt.Sprintf("/logs?after=%d&follow=%t", after, follow))
	if err != nil {
		return OutputPage{}, err
	}
	var page OutputPage
	err = c.getJSON(ctx, path, &page)
	return page, err
}
