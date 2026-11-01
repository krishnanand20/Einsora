package docker

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/krishnanand20/tensorafi/controlplane/internal/redact"
)

// The narrow Engine API client uses only net/http. Remote daemons are deliberately unsupported:
// bind paths, GPU inventory, and Unix sockets must name this machine, never a different host.
type engine struct {
	http    *http.Client
	version string
}

type engineError struct {
	status  int
	message string
}

func (e *engineError) Error() string {
	return fmt.Sprintf("Docker API HTTP %d: %s", e.status, e.message)
}
func isHTTP(err error, code int) bool {
	var e *engineError
	return errors.As(err, &e) && e.status == code
}

func localEngine(socket string) *engine {
	return &engine{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		ResponseHeaderTimeout: 15 * time.Second,
	}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (e *engine) request(ctx context.Context, method, path string, in any) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("encode Docker request: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+e.version+path, body)
	if err != nil {
		return nil, fmt.Errorf("docker request: %w", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker request: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	var failure struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&failure)
	return nil, &engineError{status: resp.StatusCode, message: redact.Secrets(failure.Message)}
}

func (e *engine) do(ctx context.Context, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := e.request(ctx, method, path, in)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return err
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode Docker response: %w", err)
	}
	return nil
}

type engineInfo struct {
	OSType          string
	Runtimes        map[string]json.RawMessage
	SecurityOptions []string
	MemoryLimit     bool
	SwapLimit       bool
	PidsLimit       bool
	CPUCfsQuota     bool
}

func (e *engine) detect(ctx context.Context) (engineInfo, error) {
	var version struct {
		APIVersion    string `json:"ApiVersion"`
		MinAPIVersion string `json:"MinAPIVersion"`
		OS            string `json:"Os"`
	}
	if err := e.do(ctx, http.MethodGet, "/version", nil, &version); err != nil {
		return engineInfo{}, err
	}
	// All fields used here are supported by Engine API 1.41 (Docker 20.10) or later.
	if version.OS != "linux" || !apiAtLeast(version.APIVersion, 41) || apiAtLeast(version.MinAPIVersion, 48) {
		return engineInfo{}, fmt.Errorf("docker needs a local Linux daemon accepting Engine API 1.41 through 1.47")
	}
	e.version = "/v1.47"
	if !apiAtLeast(version.APIVersion, 47) {
		e.version = "/v" + version.APIVersion
	}
	var info engineInfo
	if err := e.do(ctx, http.MethodGet, "/info", nil, &info); err != nil {
		return engineInfo{}, err
	}
	if info.OSType != "linux" || info.Runtimes["runc"] == nil {
		return engineInfo{}, fmt.Errorf("docker needs the Linux runc runtime")
	}
	for _, option := range info.SecurityOptions {
		if strings.HasPrefix(option, "name=rootless") || strings.HasPrefix(option, "name=userns") {
			return engineInfo{}, fmt.Errorf("docker driver needs a rootful daemon without user-namespace remapping; remapped bind-mount ownership is not supported yet")
		}
	}
	if !info.MemoryLimit || !info.SwapLimit || !info.PidsLimit || !info.CPUCfsQuota {
		return engineInfo{}, fmt.Errorf("docker needs working memory, swap, PID, and CPU cgroup limits")
	}
	return info, nil
}

func apiAtLeast(version string, minor int) bool {
	var major, got int
	if _, err := fmt.Sscanf(version, "%d.%d", &major, &got); err != nil {
		return false
	}
	return major > 1 || major == 1 && got >= minor
}

func (e *engine) image(ctx context.Context, ref string) (string, error) {
	var result struct {
		ID string `json:"Id"`
	}
	err := e.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, &result)
	return result.ID, err
}

func (e *engine) ensureImage(ctx context.Context, ref string) (string, error) {
	id, err := e.image(ctx, ref)
	if err == nil {
		return id, nil
	}
	if !isHTTP(err, http.StatusNotFound) {
		return "", err
	}
	resp, err := e.request(ctx, http.MethodPost, "/images/create?"+url.Values{"fromImage": {ref}}.Encode(), nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 16<<20))
	for {
		var progress struct {
			Error string `json:"error"`
		}
		err := decoder.Decode(&progress)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("pull image: %w", err)
		}
		if progress.Error != "" {
			return "", fmt.Errorf("pull image: %s", redact.Secrets(progress.Error))
		}
	}
	return e.image(ctx, ref)
}

type containerInfo struct {
	ID     string `json:"Id"`
	Config struct {
		Labels map[string]string
		Image  string
		User   string
	}
	HostConfig hostConfig
	State      struct {
		Running    bool
		Restarting bool
		Paused     bool
		OOMKilled  bool
		ExitCode   int
	}
}

func (e *engine) inspect(ctx context.Context, name string) (containerInfo, error) {
	var info containerInfo
	err := e.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, &info)
	return info, err
}

func (e *engine) containerAction(ctx context.Context, method, name, suffix string) error {
	return e.do(ctx, method, "/containers/"+url.PathEscape(name)+suffix, nil, nil)
}

func (e *engine) create(ctx context.Context, name string, config containerConfig) error {
	var result struct {
		ID       string `json:"Id"`
		Warnings []string
	}
	if err := e.do(ctx, http.MethodPost, "/containers/create?"+url.Values{"name": {name}}.Encode(), config, &result); err != nil {
		return err
	}
	if len(result.Warnings) > 0 {
		return fmt.Errorf("docker did not fully apply the requested container settings: %s", redact.Secrets(strings.Join(result.Warnings, "; ")))
	}
	return nil
}

func (e *engine) network(ctx context.Context, name string, want map[string]string, create bool) error {
	var network struct{ Labels map[string]string }
	err := e.do(ctx, http.MethodGet, "/networks/"+url.PathEscape(name), nil, &network)
	if isHTTP(err, http.StatusNotFound) && create {
		var result struct {
			ID string `json:"Id"`
		}
		err = e.do(ctx, http.MethodPost, "/networks/create", map[string]any{"Name": name, "CheckDuplicate": true, "Driver": "bridge",
			"Labels": want, "Options": map[string]string{"com.docker.network.bridge.enable_icc": "false"}}, &result)
		if err != nil && !isHTTP(err, http.StatusConflict) {
			return err
		}
		err = e.do(ctx, http.MethodGet, "/networks/"+url.PathEscape(name), nil, &network)
	}
	if err != nil {
		return err
	}
	if network.Labels[instanceLabel] != want[instanceLabel] || network.Labels[workloadLabel] != want[workloadLabel] {
		return fmt.Errorf("refusing a Docker network owned by a different workload")
	}
	return nil
}

func owned(info containerInfo, instance string, st state) bool {
	return info.Config.Labels[instanceLabel] == instance && info.Config.Labels[workloadLabel] == st.Desired.ID.String() &&
		info.Config.Labels[attemptLabel] == fmt.Sprint(st.Desired.Attempt) && st.Spec != "" && info.Config.Labels[specLabel] == st.Spec
}

func (e *engine) close() {
	if t, ok := e.http.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}

// Docker image names may contain a registry port, but none of its API object paths are hostnames.
func cleanID(id string) bool {
	_, err := hex.DecodeString(strings.TrimPrefix(id, "sha256:"))
	return strings.HasPrefix(id, "sha256:") && len(id) == 71 && err == nil
}
