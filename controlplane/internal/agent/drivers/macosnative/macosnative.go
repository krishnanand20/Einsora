//go:build darwin

// Package macosnative runs workloads natively on an Apple Silicon Mac, with the GPU through Metal
// (ADR-0013, ADR-0017): today, JupyterLab with PyTorch (MPS) and MLX. Each workload runs as a
// supervised process under a sandbox profile, from a uv environment built once per runtime from a
// lock with hashes. It's weak isolation, for the machine owner's own workloads only (the trust
// matrix); the control plane places nothing else here, and the agent checks again.
package macosnative

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/agent"
	"github.com/krishnanand20/einsora/controlplane/internal/agent/drivers/process"
	"github.com/krishnanand20/einsora/controlplane/internal/agent/drivers/pythonruntime"
	"github.com/krishnanand20/einsora/controlplane/internal/agent/protocol"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/redact"
)

//go:embed envs/*.txt
var envs embed.FS

//go:embed sandbox.sb
var sandboxProfile []byte

const (
	sandboxExec = "/usr/bin/sandbox-exec"
	// envTimeout bounds building an environment, which downloads packages the first time.
	envTimeout = 30 * time.Minute
	// healthTimeout bounds one health check.
	healthTimeout = time.Second
)

// Options configure the driver.
type Options struct {
	// Root is the driver's own directory, inside State.
	Root string
	// State is the agent's state directory, which workloads may not read or write (other workloads,
	// the machine's identity), apart from their own directory and environment.
	State string
	// Private are files no workload may read or write, wherever they are: the control plane's
	// database (with its -wal, -shm, and -journal files) and its log file.
	Private []string
	// Runs holds each workload's run directory: its server's Unix socket, its kernels' sockets, and
	// its temporary files. Socket paths are short on macOS (104 bytes), so it's a short path, not
	// inside State: empty means ~/.einsora/run.
	Runs string
	// UV is the uv binary; empty means the one on PATH.
	UV  string
	Log *slog.Logger
}

type Driver struct {
	opts      Options
	uv        string
	home      string // the owner's home directory
	runs      string // Options.Runs, resolved
	profile   string // the sandbox profile, with the rules the driver adds
	sandboxID string // the profile and directory mappings used by this version of the driver
	ctx       context.Context
	cancel    context.CancelFunc
	buildWG   sync.WaitGroup
	closed    bool // guarded by mu; no build may begin after Close starts waiting

	mu       sync.Mutex
	locks    map[uuid.UUID]*sync.Mutex    // one per workload
	envMu    map[string]*sync.Mutex       // one per environment
	trees    map[uuid.UUID][]process.Proc // each running workload's processes, as last seen
	stopping map[uuid.UUID]bool           // workloads being stopped in the background
	building map[uuid.UUID]string         // workload -> token of its active environment build
}

// New checks this Mac can run native workloads and prepares the driver's directory.
func New(opts Options) (*Driver, error) {
	if runtime.GOARCH != "arm64" {
		return nil, errors.New("macos-native needs an Apple Silicon Mac")
	}
	uv := opts.UV
	if uv == "" {
		var err error
		if uv, err = exec.LookPath("uv"); err != nil {
			return nil, errors.New("macos-native needs uv to build its Python environments: install it (brew install uv)")
		}
	}
	if _, err := os.Stat(sandboxExec); err != nil {
		return nil, fmt.Errorf("macos-native needs %s: %w", sandboxExec, err)
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return nil, errors.New("macos-native needs the owner's home directory, to keep workloads out of the credentials in it")
	}
	profile, err := sandbox(home, opts.Private)
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{opts.Root, filepath.Join(opts.Root, "workloads"), filepath.Join(opts.Root, "envs")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("macos-native directory: %w", err)
		}
	}
	runs := opts.Runs
	if runs == "" {
		runs = filepath.Join(home, ".einsora", "run")
	}
	if runs, err = privateDir(runs); err != nil {
		return nil, err
	}
	// The longest socket a workload has is a kernel's: <run>/kernel-<36-character ID>-ipc-<n>.
	if longest := len(runs) + 1 + runNameLen + len("/kernel-") + 36 + len("-ipc-99"); longest > protocol.MaxSocketPath {
		return nil, fmt.Errorf("macos-native needs a shorter directory for workloads' sockets than %s: macOS allows %d bytes in a socket's path", runs, protocol.MaxSocketPath+1)
	}
	sandboxSum := sha256.Sum256([]byte(profile + "\x00" + resolved(opts.State) + "\x00" + runs))
	ctx, cancel := context.WithCancel(context.Background())
	return &Driver{opts: opts, uv: uv, home: home, runs: runs, profile: profile, sandboxID: hex.EncodeToString(sandboxSum[:]),
		ctx: ctx, cancel: cancel,
		locks: map[uuid.UUID]*sync.Mutex{},
		envMu: map[string]*sync.Mutex{}, trees: map[uuid.UUID][]process.Proc{}, stopping: map[uuid.UUID]bool{},
		building: map[uuid.UUID]string{},
	}, nil
}

// privateDir makes dir, and each directory it makes on the way, readable by this user only, and
// checks dir is a real directory of this user's that no one else can use: a socket or temporary
// file inside it is this user's alone. It returns dir with symlinks resolved, as the sandbox sees it.
func privateDir(dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("macos-native: %s isn't an absolute path", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("macos-native run directory: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("macos-native run directory: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Getuid() || fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("macos-native run directory %s must be a directory (not a symlink) that only you can use: run chmod 700 %s", dir, dir)
	}
	return resolved(dir), nil
}

// credentialStores are where the owner's credentials usually are, relative to their home
// directory: workloads may neither read nor write them.
func credentialStores() []string {
	return []string{
		".ssh", ".aws", ".azure", ".gnupg", ".kube", ".docker", ".netrc", ".git-credentials", ".npmrc", ".pypirc",
		".password-store", ".terraform.d", ".config/gcloud", ".config/gh", ".config/git/credentials", ".config/op",
		".cache/huggingface/token", ".huggingface/token", ".local/share/keyrings",
		".zsh_history", ".zsh_sessions", ".bash_history", ".bash_sessions", ".python_history", ".psql_history", ".mysql_history",
		"Library/Keychains", "Library/Cookies", "Library/Safari", "Library/Mail", "Library/Messages",
		"Library/Application Support/Google/Chrome", "Library/Application Support/Firefox",
		"Library/Application Support/BraveSoftware", "Library/Application Support/Microsoft Edge",
		"Library/Application Support/Arc",
	}
}

// sandbox is the sandbox profile: the fixed rules, then a deny for each credential store and private
// file, by its path and, where that's a symlink, by its target too, since the sandbox matches the
// paths files resolve to.
func sandbox(home string, private []string) (string, error) {
	var paths []string
	for _, rel := range credentialStores() {
		paths = append(paths, filepath.Join(home, rel))
	}
	paths = append(paths, private...)
	var b strings.Builder
	b.Write(sandboxProfile)
	b.WriteString("\n;; Added by the driver: the owner's credential stores and the control plane's private files.\n(deny file-read* file-write*")
	seen := map[string]bool{}
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("macos-native sandbox: %q isn't an absolute path", p)
		}
		for _, q := range []string{filepath.Clean(p), resolved(p)} {
			if seen[q] {
				continue
			}
			seen[q] = true
			quoted, err := sbString(q)
			if err != nil {
				return "", err
			}
			b.WriteString("\n  (subpath " + quoted + ")")
		}
	}
	b.WriteString(")\n")
	return b.String(), nil
}

// sbString quotes a path for a sandbox profile. A path with control characters is refused rather
// than written into the profile.
func sbString(s string) (string, error) {
	if !utf8.ValidString(s) || strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", fmt.Errorf("macos-native sandbox: can't use the path %q", s)
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`, nil
}

func (d *Driver) Name() string { return domain.DriverMacOSNative }

// Close cancels and joins environment builds before this agent gives up its machine lock. Running
// workload processes stay alive and are adopted by the next agent.
func (d *Driver) Close() {
	d.mu.Lock()
	d.closed = true
	d.cancel()
	d.mu.Unlock()
	d.buildWG.Wait()
}

func (d *Driver) Accelerators() []domain.AcceleratorKind {
	return []domain.AcceleratorKind{domain.AcceleratorApple, domain.AcceleratorCPU}
}

// Phases of a workload, in its state file.
const (
	phasePreparing = "preparing" // building its environment, then starting
	phaseRunning   = "running"
	phaseFailed    = "failed"
	phaseStopping  = "stopping"
	phaseStopped   = "stopped"
)

// state is what the driver keeps about one workload, so it finds it again after a restart. It's
// owner-only: it holds the workload's token. The workload can read it but not change it.
type state struct {
	Attempt   int64               `json:"attempt"`
	Phase     string              `json:"phase"`
	Code      protocol.Code       `json:"code,omitempty"`
	Proc      process.Proc        `json:"proc"`
	Run       string              `json:"run"` // its run directory's name, in Options.Runs
	Token     string              `json:"token"`
	Runtime   string              `json:"runtime"`
	Sandbox   string              `json:"sandbox"`        // the profile and directory mappings this attempt started with
	Kind      domain.WorkloadKind `json:"kind,omitempty"` // empty in older notebook state files
	ModelURI  string              `json:"model_uri,omitempty"`
	MemoryMiB int64               `json:"memory_mib,omitempty"`
}

// runNameLen is the length of a run directory's name: short, for socket paths.
const runNameLen = 8

func (d *Driver) dir(id uuid.UUID) string {
	return filepath.Join(d.opts.Root, "workloads", id.String())
}

// runDir is a workload's run directory, and socket is its server's socket in it.
func (d *Driver) runDir(st state) string { return filepath.Join(d.runs, st.Run) }

func (d *Driver) socket(st state) string { return filepath.Join(d.runDir(st), "jupyter.sock") }

func (d *Driver) lock(id uuid.UUID) *sync.Mutex {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.locks[id] == nil {
		d.locks[id] = &sync.Mutex{}
	}
	return d.locks[id]
}

// readState and writeState never follow a symlink: the driver runs outside the sandbox, in a
// directory the workload can read.
func (d *Driver) readState(id uuid.UUID) (state, bool, error) {
	f, err := os.OpenFile(filepath.Join(d.dir(id), "state.json"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return state{}, false, nil
	}
	if err != nil {
		return state{}, false, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 1<<16))
	if err != nil {
		return state{}, false, err
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return state{}, false, fmt.Errorf("workload %s state: %w", id, err)
	}
	if st.Run != "" {
		if _, err := hex.DecodeString(st.Run); err != nil || len(st.Run) != runNameLen {
			return state{}, false, fmt.Errorf("workload %s state: invalid run directory", id)
		}
	}
	return st, true, nil
}

func (d *Driver) writeState(id uuid.UUID, st state) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := filepath.Join(d.dir(id), "state.json.tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // the driver's own state path, never through a symlink
	if err != nil {
		return err
	}
	_, err = f.Write(raw)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(d.dir(id), "state.json"))
}

// Run starts attempt w.Attempt of a notebook or endpoint. It returns once the attempt is recorded; building
// the environment and starting the server happen in the background, shown as progressing.
func (d *Driver) Run(_ context.Context, w protocol.Desired) error {
	if w.StrongIsolation {
		return agent.WithCode(protocol.CodeIsolation, errors.New("native workloads are for the machine owner only"))
	}
	if (w.Accelerator != domain.AcceleratorApple && w.Accelerator != domain.AcceleratorCPU) ||
		(w.Accelerator == domain.AcceleratorCPU && w.Devices != 0) || (w.Accelerator == domain.AcceleratorApple && w.Devices != 1) {
		return agent.WithCode(protocol.CodeInvalid, errors.New("native workloads need an explicit CPU allocation or the whole Apple GPU"))
	}
	if w.Kind != domain.KindNotebook && w.Kind != domain.KindEndpoint {
		return agent.WithCode(protocol.CodeInvalid, fmt.Errorf("macos-native runs notebooks and endpoints, not %s", w.Kind))
	}
	if w.Kind == domain.KindNotebook && w.Runtime.Ref != "jupyterlab-pytorch-mps" {
		return agent.WithCode(protocol.CodeImage, errors.New("native notebooks need the pinned Jupyter environment"))
	}
	if w.Kind == domain.KindEndpoint && (w.Accelerator != domain.AcceleratorApple || w.Runtime.Ref != "mlx-lm" || w.Runtime.Server != "mlx" ||
		w.Runtime.ModelFormat != "safetensors" || w.MemoryMiB <= 0 ||
		(!strings.HasPrefix(w.ModelURI, "hf://") && !strings.HasPrefix(w.ModelURI, "s3://"))) {
		return agent.WithCode(protocol.CodeInvalid, errors.New("native endpoints need the MLX safetensors runtime, a model source, and a memory allocation"))
	}
	lock, err := envs.ReadFile("envs/" + w.Runtime.Ref + ".txt")
	if err != nil {
		return agent.WithCode(protocol.CodeImage, fmt.Errorf("no environment for runtime %q", w.Runtime.Ref))
	}
	mu := d.lock(w.ID)
	mu.Lock()
	defer mu.Unlock()
	st, known, err := d.readState(w.ID)
	if err != nil {
		return err
	}
	if known && st.Attempt == w.Attempt && (st.Phase == phasePreparing || st.Phase == phaseRunning && process.Alive(st.Proc)) {
		return nil // already starting or running this attempt
	}
	if known && process.Alive(st.Proc) {
		return agent.WithCode(protocol.CodeInvalid, errors.New("another attempt is still running"))
	}
	data := filepath.Join(d.dir(w.ID), "data")
	if _, err := os.Stat(data); errors.Is(err, os.ErrNotExist) {
		if known {
			// It ran here before, and its data is gone: never hand it a fresh, empty disk.
			return agent.WithCode(protocol.CodeDiskMissing, fmt.Errorf("workload %s: its data directory is missing", w.ID))
		}
		for _, sub := range []string{"data", "home", "logs"} {
			if err := os.MkdirAll(filepath.Join(d.dir(w.ID), sub), 0o700); err != nil {
				return err
			}
		}
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	next := state{Attempt: w.Attempt, Phase: phasePreparing, Run: st.Run, Token: token, Runtime: w.Runtime.Ref, Kind: w.Kind,
		Sandbox: d.sandboxFingerprint(w.Kind), ModelURI: w.ModelURI, MemoryMiB: w.MemoryMiB}
	if next.Run == "" {
		if next.Run, err = d.newRunName(); err != nil {
			return err
		}
	}
	if err := d.writeState(w.ID, next); err != nil {
		if st.Run == "" {
			_ = os.Remove(d.runDir(next)) // only the empty directory this attempt reserved
		}
		return err
	}
	d.launchInBackground(w, next, lock)
	return nil
}

// A build can outlast a sync, and a control-plane restart can interrupt one. Only one goroutine per
// attempt rebuilds/resumes its environment; a stopped or replaced attempt can't start a process.
func (d *Driver) launchInBackground(w protocol.Desired, st state, lock []byte) {
	d.mu.Lock()
	if d.closed || d.building[w.ID] == st.Token {
		d.mu.Unlock()
		return
	}
	d.building[w.ID] = st.Token
	d.buildWG.Add(1)
	d.mu.Unlock()
	go func() {
		defer d.buildWG.Done()
		defer func() {
			d.mu.Lock()
			if d.building[w.ID] == st.Token {
				delete(d.building, w.ID)
			}
			d.mu.Unlock()
		}()
		d.launch(w, st, lock)
	}()
}

// newRunName reserves a run directory before any environment build starts. Checking for an unused
// name alone would let two workloads choose the same one while their environments are built.
func (d *Driver) newRunName() (string, error) {
	for range 10 {
		b := make([]byte, runNameLen/2)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		name := hex.EncodeToString(b)
		if err := os.Mkdir(filepath.Join(d.runs, name), 0o700); err == nil {
			return name, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("reserve workload run directory: %w", err)
		}
	}
	return "", errors.New("no free run directory name")
}

// launch builds the environment if needed and starts the server, unless the attempt was stopped
// or replaced meanwhile. Each run starts with an empty run directory: its sockets and temporary
// files are for one run only.
func (d *Driver) launch(w protocol.Desired, st state, lock []byte) {
	env, err := d.ensureEnv(w.Runtime.Ref, lock)
	if d.ctx.Err() != nil {
		return
	} // a new agent resumes preparation from the saved intent
	mu := d.lock(w.ID)
	mu.Lock()
	defer mu.Unlock()
	if d.ctx.Err() != nil {
		return
	}
	current, _, readErr := d.readState(w.ID)
	if readErr != nil || current.Attempt != st.Attempt || current.Token != st.Token || current.Phase != phasePreparing {
		return // stopped or replaced while the environment was built
	}
	if err != nil {
		d.opts.Log.Error("couldn't build a workload's environment", "workload", w.ID, "runtime", w.Runtime.Ref,
			"err", redact.Secrets(err.Error()))
		st.Phase, st.Code = phaseFailed, protocol.CodeImage
		_ = d.writeState(w.ID, st)
		return
	}
	err = d.prepareRun(st)
	if err != nil {
		d.opts.Log.Error("couldn't prepare a workload's run directory", "workload", w.ID, "err", err)
		st.Phase, st.Code = phaseFailed, protocol.CodeDriverProblem
		_ = d.writeState(w.ID, st)
		return
	}
	spec := d.notebook(w.ID, env, st)
	if w.Kind == domain.KindEndpoint {
		spec = d.endpoint(w, env, st)
	}
	proc, err := process.Start(spec)
	if err != nil {
		d.opts.Log.Error("couldn't start a workload", "workload", w.ID, "err", redact.Secrets(err.Error()))
		st.Phase, st.Code = phaseFailed, protocol.CodeDriverProblem
		_ = d.writeState(w.ID, st)
		return
	}
	st.Phase, st.Proc = phaseRunning, proc
	if err := d.writeState(w.ID, st); err != nil {
		process.Stop(proc, time.Second) // unrecorded, it couldn't be found or stopped later
		d.opts.Log.Error("couldn't record a workload's process; stopped it", "workload", w.ID, "err", err)
	}
}

// prepareRun clears the previous run's sockets and temporary files while keeping the directory
// reserved. The workload may write inside it, so refuse a symlink rather than following one.
func (d *Driver) prepareRun(st state) error {
	run := d.runDir(st)
	if _, err := privateDir(run); err != nil {
		return err
	}
	entries, err := os.ReadDir(run)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(run, entry.Name())); err != nil {
			return err
		}
	}
	return os.Mkdir(filepath.Join(run, "tmp"), 0o700)
}

// notebook is the JupyterLab server for a workload, under the sandbox. It listens on a Unix socket
// in the workload's run directory, and its kernels talk to it over IPC there: the sandbox lets a
// workload listen on no TCP or UDP port. Cookies ignore ports, so a page in the browser sent to a
// port a workload listened on would hand it the dashboard's session, or another workload's. The
// token goes in through the environment, which other users can't read, not the command line,
// which they can.
func (d *Driver) notebook(id uuid.UUID, env string, st state) process.Spec {
	data := filepath.Join(d.dir(id), "data")
	return d.command(id, env, st,
		filepath.Join(resolved(env), "bin", "jupyter-lab"),
		"--no-browser", "--ServerApp.sock="+d.socket(st), "--ServerApp.sock_mode=0600",
		"--KernelManager.transport=ipc", "--ServerApp.root_dir="+data, "--ServerApp.allow_remote_access=False")
}

// command applies the same sandbox, private directories, and minimal environment to every native
// runtime. Only the endpoint's fetch step gets the operator's model-download credentials.
func (d *Driver) command(id uuid.UUID, env string, st state, command ...string) process.Spec {
	env = resolved(env)
	dir, run := d.dir(id), d.runDir(st)
	home, data := filepath.Join(dir, "home"), filepath.Join(dir, "data")
	args := []string{
		"-p", d.profile,
		// The sandbox matches resolved paths (/var is /private/var), so every path is resolved:
		// a rule written with a symlinked path would never apply.
		"-D", "WORKLOAD=" + resolved(dir), "-D", "STATE=" + resolved(d.opts.State), "-D", "ENV=" + resolved(env),
		"-D", "PYTHONS=" + resolved(filepath.Join(d.opts.Root, "python")),
		"-D", "RUNS=" + d.runs, "-D", "RUN=" + resolved(run),
	}
	return process.Spec{
		Path: sandboxExec, Args: append(args, command...), Dir: data,
		Env: []string{
			"PATH=" + filepath.Join(env, "bin") + ":/usr/bin:/bin:/usr/sbin:/sbin",
			"HOME=" + home, "TMPDIR=" + filepath.Join(run, "tmp"), "LANG=en_US.UTF-8",
			"JUPYTER_TOKEN=" + st.Token,
			"JUPYTER_CONFIG_DIR=" + filepath.Join(home, ".jupyter"),
			"JUPYTER_DATA_DIR=" + filepath.Join(home, ".local", "share", "jupyter"),
			"JUPYTER_RUNTIME_DIR=" + run,
			"IPYTHONDIR=" + filepath.Join(home, ".ipython"),
			"MPLCONFIGDIR=" + filepath.Join(home, ".matplotlib"),
			"PYTHONDONTWRITEBYTECODE=1",
			"PYTORCH_ENABLE_MPS_FALLBACK=1",
		},
		Stdout: filepath.Join(dir, "logs", "stdout.log"),
		Stderr: filepath.Join(dir, "logs", "stderr.log"),
	}
}

// ensureEnv builds the environment for a runtime's lock once, and returns its directory. A lock
// that changes gets a new environment. uv's cache and the Python builds it downloads stay in the
// driver's directory. It runs outside the sandbox, so it installs only wheels (--no-build: no
// package's build code runs) checked against the lock's hashes, and gets only the environment it
// needs, not the control plane's.
func (d *Driver) ensureEnv(ref string, lock []byte) (string, error) {
	name, dir := d.envDir(ref, lock)
	sum := sha256.Sum256(lock)
	d.mu.Lock()
	if d.envMu[name] == nil {
		d.envMu[name] = &sync.Mutex{}
	}
	mu := d.envMu[name]
	d.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := context.WithTimeout(d.ctx, envTimeout)
	defer cancel()
	buildLock, err := lockEnvironment(ctx, filepath.Join(d.opts.Root, "envs", name+".build.lock"))
	if err != nil {
		return "", err
	}
	defer func() { _ = buildLock.Close() }()
	marker := filepath.Join(dir, ".einsora-ready")
	if _, err := os.Stat(marker); err == nil {
		return dir, nil
	}
	_ = os.RemoveAll(dir) // a build that didn't finish
	lockFile := filepath.Join(d.opts.Root, "envs", name+".lock.txt")
	if err := os.WriteFile(lockFile, lock, 0o600); err != nil {
		return "", err
	}
	uvEnv := []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + d.home,
		"UV_CACHE_DIR=" + filepath.Join(d.opts.Root, "uv-cache"),
		"UV_PYTHON_INSTALL_DIR=" + filepath.Join(d.opts.Root, "python"),
		"UV_PYTHON_PREFERENCE=only-managed"}
	// A proxy, certificates, and a package mirror the owner set still apply; the hashes still hold.
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "http_proxy", "https_proxy",
		"no_proxy", "all_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR", "UV_DEFAULT_INDEX", "UV_INDEX_URL", "UV_INDEX"} {
		if v, ok := os.LookupEnv(k); ok {
			uvEnv = append(uvEnv, k+"="+v)
		}
	}
	for _, args := range [][]string{
		{"venv", "--python", "3.12", "--no-project", dir},
		{"pip", "sync", "--python", filepath.Join(dir, "bin", "python"), "--require-hashes", "--no-build", lockFile},
	} {
		cmd := exec.CommandContext(ctx, d.uv, args...) //nolint:gosec // fixed arguments
		cmd.Env = uvEnv
		// The uv child retains the flock if Einsora crashes. A recovering process waits until
		// that child exits before removing or rebuilding the same environment directory.
		cmd.ExtraFiles = []*os.File{buildLock}
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("uv %s: %w: %s", args[0], err, tail(out))
		}
	}
	if ref == "mlx-lm" {
		if err := os.WriteFile(filepath.Join(dir, "einsora-runtime.zip"), pythonruntime.Bytes(), 0o600); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(marker, sum[:], 0o600); err != nil {
		return "", err
	}
	return dir, nil
}

// envDir is where the environment for a runtime's lock lives.
func (d *Driver) envDir(ref string, lock []byte) (name, dir string) {
	if ref == "mlx-lm" {
		lock = append(append([]byte(nil), lock...), pythonruntime.Bytes()...)
	}
	sum := sha256.Sum256(lock)
	name = ref + "-" + hex.EncodeToString(sum[:6])
	return name, filepath.Join(d.opts.Root, "envs", name)
}

// Stop stops a workload, keeping its data. It returns at once; its processes get a grace period.
func (d *Driver) Stop(_ context.Context, id uuid.UUID) error {
	mu := d.lock(id)
	mu.Lock()
	defer mu.Unlock()
	st, known, err := d.readState(id)
	if err != nil || !known {
		return err
	}
	switch st.Phase {
	case phasePreparing, phaseFailed:
		st.Phase = phaseStopped // a server starting in the background sees this and doesn't start
		if err := d.writeState(id, st); err != nil {
			return err
		}
		process.StopAll(d.forget(id), 0) // anything a failed server left
	case phaseRunning, phaseStopping:
		if st.Phase == phaseRunning {
			st.Phase = phaseStopping
			if err := d.writeState(id, st); err != nil {
				return err
			}
		}
		d.stopInBackground(id, st)
	}
	return nil
}

// stopInBackground stops a stopping workload's processes, all of them, then records it as stopped.
// It's also how a stop that a restart interrupted is finished.
func (d *Driver) stopInBackground(id uuid.UUID, st state) {
	d.mu.Lock()
	if d.stopping[id] {
		d.mu.Unlock()
		return
	}
	d.stopping[id] = true
	d.mu.Unlock()
	go func() {
		defer func() {
			d.mu.Lock()
			delete(d.stopping, id)
			d.mu.Unlock()
		}()
		if !process.StopAll(append(d.forget(id), st.Proc), time.Minute) {
			d.opts.Log.Warn("a workload's processes are still running after a stop", "workload", id)
			return
		}
		mu := d.lock(id)
		mu.Lock()
		defer mu.Unlock()
		if cur, _, err := d.readState(id); err == nil && cur.Attempt == st.Attempt && cur.Phase == phaseStopping {
			cur.Phase = phaseStopped
			if cur.Code == protocol.CodeSandboxChanged {
				cur.Phase = phaseFailed
			}
			_ = d.writeState(id, cur)
		}
	}()
}

// see records the processes of a running workload: its server and everything descended from it,
// such as kernels, which run in sessions of their own. They're stopped with it, and stopped if the
// server exits without them.
func (d *Driver) see(id uuid.UUID, p process.Proc) {
	d.mu.Lock()
	prev := d.trees[id]
	d.mu.Unlock()
	tree := process.Tree(append(prev, p)...)
	d.mu.Lock()
	d.trees[id] = tree
	d.mu.Unlock()
}

// forget returns the processes last seen for a workload, and forgets them.
func (d *Driver) forget(id uuid.UUID) []process.Proc {
	d.mu.Lock()
	defer d.mu.Unlock()
	procs := d.trees[id]
	delete(d.trees, id)
	return procs
}

// Remove stops a workload and deletes everything it stored.
func (d *Driver) Remove(ctx context.Context, id uuid.UUID) error {
	mu := d.lock(id)
	mu.Lock()
	defer mu.Unlock()
	st, known, err := d.readState(id)
	if err != nil {
		return err
	}
	procs := d.forget(id)
	if known {
		procs = append(procs, st.Proc)
	}
	wait := 10 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(deadline)/2)
	}
	if !process.StopAll(procs, wait) {
		return errors.New("its processes are still running")
	}
	if known && st.Run != "" {
		if err := os.RemoveAll(d.runDir(st)); err != nil {
			return err
		}
	}
	return os.RemoveAll(d.dir(id))
}

// Statuses reports every workload the driver has, from its state files: a process that's alive and
// answers on its socket is ready; one that exited by itself has failed.
func (d *Driver) Statuses(ctx context.Context) ([]protocol.Status, error) {
	entries, err := os.ReadDir(filepath.Join(d.opts.Root, "workloads"))
	if err != nil {
		return nil, err
	}
	var out []protocol.Status
	for _, e := range entries {
		id, err := uuid.Parse(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		mu := d.lock(id)
		mu.Lock()
		st, known, err := d.readState(id)
		if err != nil || !known {
			mu.Unlock()
			continue
		}
		// An older sandbox may allow TCP listeners. Stop its processes before reporting a failure;
		// a new attempt can reuse the saved notebook data under the current sandbox. This also
		// handles the old TCP state format, which has no run directory or sandbox fingerprint.
		if (st.Run == "" || st.Sandbox != d.sandboxFingerprint(st.Kind)) && (st.Phase == phaseRunning || st.Phase == phasePreparing) {
			d.see(id, st.Proc)
			st.Phase, st.Code = phaseStopping, protocol.CodeSandboxChanged
			if err := d.writeState(id, st); err != nil {
				mu.Unlock()
				return nil, err
			}
		}
		mu.Unlock()
		s := protocol.Status{ID: id, Attempt: st.Attempt}
		switch st.Phase {
		case phasePreparing:
			s.State, s.Code = protocol.StateProgressing, protocol.CodePreparing
			// No workload process exists until its environment is ready. After a restart, resume
			// that work from persisted intent instead of remaining in preparing forever.
			if lock, err := envs.ReadFile("envs/" + st.Runtime + ".txt"); err == nil {
				kind := st.Kind
				if kind == "" {
					kind = domain.KindNotebook
				}
				d.launchInBackground(protocol.Desired{ID: id, Attempt: st.Attempt, Kind: kind,
					ModelURI: st.ModelURI, MemoryMiB: st.MemoryMiB, Runtime: protocol.Runtime{Ref: st.Runtime}}, st, lock)
			}
		case phaseFailed:
			s.State, s.Code = protocol.StateFailed, st.Code
		case phaseStopping:
			s.State = protocol.StateProgressing
			d.stopInBackground(id, st) // after a restart, nothing else is stopping it
		case phaseStopped:
			s.State = protocol.StateStopped
		case phaseRunning:
			alive := process.Alive(st.Proc)
			if alive {
				d.see(id, st.Proc)
			}
			switch {
			case !alive:
				process.StopAll(d.forget(id), 0) // its kernels would go on without it
				s.State, s.Code = protocol.StateFailed, protocol.CodeExited
				if st.Kind == domain.KindEndpoint {
					s.Code = d.endpointCode(st, true)
				}
			case d.healthy(ctx, d.socket(st)):
				s.State, s.Access = protocol.StateReady, &protocol.Access{Socket: d.socket(st), Token: st.Token}
			default:
				s.State, s.Code = protocol.StateProgressing, protocol.CodeStarting
				if st.Kind == domain.KindEndpoint {
					s.Code = d.endpointCode(st, false)
				}
			}
		default:
			s.State = protocol.StateFailed
		}
		out = append(out, s)
	}
	return out, nil
}

// healthy reports whether the server on socket answers. Jupyter answers /api without a token.
func (d *Driver) healthy(ctx context.Context, socket string) bool {
	client := &http.Client{Timeout: healthTimeout, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives: true,
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/api", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// resolved is path with its symlinks resolved, as the sandbox sees it. A path that doesn't exist
// yet (Python builds before the first download) is resolved as far as it exists.
func resolved(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	if parent := filepath.Dir(path); parent != path {
		return filepath.Join(resolved(parent), filepath.Base(path))
	}
	return path
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// tail is the end of a command's output, for the operator's log.
func tail(out []byte) string {
	if len(out) > 2000 {
		out = out[len(out)-2000:]
	}
	return string(out)
}
