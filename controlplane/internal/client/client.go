// Package client is a small Go client for the Einsora API, used by the `einsora` CLI commands
// (workloads, events, export). It only speaks the public /v1 API.
package client

import (
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
}

type Event struct {
	ID     int64     `json:"id"`
	From   string    `json:"from"`
	To     string    `json:"to"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

func (c *Client) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
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

func (c *Client) getJSON(ctx context.Context, path string, dst any) error {
	resp, err := c.get(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return json.NewDecoder(resp.Body).Decode(dst)
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
	resp, err := c.get(ctx, path)
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
