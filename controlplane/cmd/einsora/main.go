// Command einsora runs the Einsora control plane. One binary, one image; the subcommand
// picks the role. See `einsora help`.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/access"
	"github.com/krishnanand20/einsora/controlplane/internal/agent"
	"github.com/krishnanand20/einsora/controlplane/internal/agent/drivers/docker"
	"github.com/krishnanand20/einsora/controlplane/internal/agent/drivers/hostlock"
	"github.com/krishnanand20/einsora/controlplane/internal/agent/drivers/inventory"
	"github.com/krishnanand20/einsora/controlplane/internal/agent/drivers/macosnative"
	"github.com/krishnanand20/einsora/controlplane/internal/agent/drivers/sim"
	"github.com/krishnanand20/einsora/controlplane/internal/api"
	"github.com/krishnanand20/einsora/controlplane/internal/auth"
	"github.com/krishnanand20/einsora/controlplane/internal/catalog"
	"github.com/krishnanand20/einsora/controlplane/internal/cluster"
	"github.com/krishnanand20/einsora/controlplane/internal/cluster/agents"
	"github.com/krishnanand20/einsora/controlplane/internal/cluster/fake"
	"github.com/krishnanand20/einsora/controlplane/internal/cluster/kube"
	"github.com/krishnanand20/einsora/controlplane/internal/config"
	"github.com/krishnanand20/einsora/controlplane/internal/controller"
	"github.com/krishnanand20/einsora/controlplane/internal/devseed"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/metering"
	"github.com/krishnanand20/einsora/controlplane/internal/redact"
	"github.com/krishnanand20/einsora/controlplane/internal/service"
	"github.com/krishnanand20/einsora/controlplane/internal/store"
	"github.com/krishnanand20/einsora/controlplane/internal/store/memstore"
	"github.com/krishnanand20/einsora/controlplane/internal/store/pgstore"
	"github.com/krishnanand20/einsora/controlplane/internal/store/sqlitestore"
	"github.com/krishnanand20/einsora/controlplane/internal/webui"
)

const usage = `einsora — free, self-hosted GPU cloud

Usage: einsora <command> [arguments]

Server commands:
  up [-no-open]
              run Einsora on this machine (Solo): the API and dashboard on http://127.0.0.1:8080, the
              controller with this machine's agent, and usage accounting, with the data in one private
              directory (EINSORA_DATA_DIR, or the usual place on this OS). The first time, it asks
              for the owner's account. Any EINSORA_* setting overrides its default.
  api         serve the REST API
  controller  reconcile workloads with the runtime (Kubernetes today; node agents planned)
  metering    usage accounting: turn workload events into hourly usage records (no money)
  migrate     apply database migrations to EINSORA_DATABASE_URL (Postgres or a SQLite file;
              a SQLite file is also migrated whenever it's opened)
  seed        create dev tenants and quotas (EINSORA_ENV=dev only)
  setup -email you@example.com [-tenant home] [-password-stdin]
              create the first account, the machine's owner, for EINSORA_AUTH_MODE=local; asks for
              the password without echoing it (or reads one line from stdin). Without
              EINSORA_DATABASE_URL, it uses the same database as einsora up
  dev         api + controller + metering in one process with seeded data; uses an in-memory
              store unless EINSORA_DATABASE_URL is set (postgres://… or sqlite:<path>)

Client commands (talk to a running API):
  workloads [-all]      list your workloads (-all includes deleted ones)
  machines              list your machines: online or not, region, CPUs, memory, GPUs, your workloads on them
  events <id> [-json]   show a workload's complete timeline: every state change and note, when, and why
  export <id> [-o file] [-f]
                        save everything recorded about a workload as one JSON file, readable by
                        you only (default einsora-<name>-<id>.json; -o - writes to stdout;
                        -f replaces an existing file)

Server configuration: EINSORA_* environment variables (see .env.example). Set EINSORA_LOG_LEVEL=debug
for detailed logs and EINSORA_LOG_FILE=path to keep a complete copy on disk.
Client configuration: EINSORA_API_URL (default http://127.0.0.1:8080), and EINSORA_TOKEN (bearer)
or EINSORA_DEV_TENANT (dev mode). A token is never sent over plain http:// to another machine
unless EINSORA_INSECURE_HTTP=true.
`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "help" || os.Args[1] == "-h" || os.Args[1] == "--help" {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch {
	case isClientCommand(cmd):
		err = runClient(ctx, cmd, args, os.Stdout)
	case cmd == "setup":
		err = runSetup(ctx, args, os.Stdin, os.Stdout)
	case cmd == "up":
		err = runUp(ctx, args, os.Stdin, os.Stdout)
	case len(args) > 0:
		err = fmt.Errorf("%s takes no arguments (see einsora help)", cmd)
	default:
		err = run(ctx, cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "einsora:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log, closeLog, err := newLogger(cfg)
	if err != nil {
		return err
	}
	defer closeLog()

	if cmd == "migrate" {
		db, err := cfg.Database()
		if err != nil {
			return err
		}
		var n int
		switch db.Driver {
		case "postgres":
			n, err = pgstore.Migrate(ctx, db.DSN)
		case "sqlite":
			log.Info("store: sqlite", "path", db.DSN)
			n, err = sqlitestore.Migrate(ctx, db.DSN)
		default:
			return errors.New("EINSORA_DATABASE_URL is required")
		}
		log.Info("migrations applied", "count", n)
		return err
	}

	cat, err := catalog.Load(cfg.CatalogPath)
	if err != nil {
		return err
	}
	st, err := openStore(ctx, cfg, log, cmd == "dev")
	if err != nil {
		return err
	}
	defer st.Close()

	switch cmd {
	case "api":
		return serveAPI(ctx, cfg, log, st, cat, nil)
	case "controller":
		runController, err := newController(cfg, log, st, cat)
		if err != nil {
			return err
		}
		return runController(ctx)
	case "metering":
		return metering.New(st, log, nil).Run(ctx, cfg.MeteringInterval)
	case "seed":
		if cfg.Env != "dev" {
			return errors.New("seed only runs with EINSORA_ENV=dev")
		}
		return devseed.Seed(ctx, st, time.Now())
	case "dev":
		if cfg.Env != "dev" {
			return errors.New("dev only runs with EINSORA_ENV=dev")
		}
		if err := devseed.Seed(ctx, st, time.Now()); err != nil {
			return err
		}
		runController, err := newController(cfg, log, st, cat)
		if err != nil {
			return err
		}
		log.Info("dev stack: seeded tenants acme, globex (untrusted) and internal (trusted); use header X-Dev-Tenant")
		return runAll(ctx,
			func(ctx context.Context) error { return serveAPI(ctx, cfg, log, st, cat, nil) },
			runController,
			func(ctx context.Context) error { return metering.New(st, log, nil).Run(ctx, cfg.MeteringInterval) },
		)
	}
	return fmt.Errorf("unknown command %q (see einsora help)", cmd)
}

// newLogger writes to stderr and, if EINSORA_LOG_FILE is set, a complete copy to that file
// (appended, readable by the owner only). Nothing is sampled or dropped. People read the terminal
// and programs read files: stderr gets text in development or when it's a terminal (einsora up),
// and JSON otherwise (a service's journal, Loki); the file always gets JSON.
func newLogger(cfg config.Config) (*slog.Logger, func(), error) {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var console slog.Handler = slog.NewJSONHandler(os.Stderr, opts)
	if cfg.Env == "dev" || isTerminal(os.Stderr) {
		console = slog.NewTextHandler(os.Stderr, opts)
	}
	if cfg.LogFile == "" {
		return slog.New(console), func() {}, nil
	}
	f, err := openLogFile(cfg.LogFile)
	if err != nil {
		return nil, nil, fmt.Errorf("open EINSORA_LOG_FILE: %w", err)
	}
	return slog.New(slog.NewMultiHandler(console, slog.NewJSONHandler(f, opts))), func() { _ = f.Close() }, nil
}

// openLogFile appends to path, which must be readable by its owner only. It never changes a file's
// permissions, so a link swapped in by someone else can't redirect a chmod. A new file is created
// 0600; an existing file that others can read is refused with the fix. Symlinks, devices, and a
// file replaced while it was being opened are refused too.
func openLogFile(path string) (*os.File, error) {
	path = filepath.Clean(path)
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file, not a symlink or device", path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err == nil {
		var now os.FileInfo
		now, err = os.Lstat(path)
		switch {
		case err != nil:
		case !now.Mode().IsRegular() || !os.SameFile(opened, now):
			err = fmt.Errorf("%s changed while it was being opened", path)
		case opened.Mode().Perm()&0o077 != 0:
			err = fmt.Errorf("%s can be read by other users (mode %v); run: chmod 600 %s", path, opened.Mode().Perm(), path)
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// listensOnlyOnLoopback reports whether addr ("127.0.0.1:8080", ":8080") accepts only this machine.
func listensOnlyOnLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsLoopback()
}

func openStore(ctx context.Context, cfg config.Config, log *slog.Logger, allowMemory bool) (store.Store, error) {
	db, err := cfg.Database()
	if err != nil {
		return nil, err
	}
	switch db.Driver {
	case "postgres":
		log.Info("store: postgres") // never log the URL: it can hold a password
		return pgstore.Open(ctx, db.DSN)
	case "sqlite":
		log.Info("store: sqlite", "path", db.DSN)
		return sqlitestore.Open(ctx, db.DSN) // applies pending migrations itself
	}
	if !allowMemory {
		return nil, errors.New("EINSORA_DATABASE_URL is required (only `einsora dev` can run in-memory)")
	}
	return memstore.New(), nil
}

// newController builds the controller and returns what runs it: with the agent backend, the
// controller and this machine's in-process agent (Solo).
func newController(cfg config.Config, log *slog.Logger, st store.Store, cat *catalog.Catalog) (func(context.Context) error, error) {
	// A controller for a region users can't create workloads in would leave everything pending.
	if _, ok := cat.Region(cfg.Region); !ok {
		return nil, fmt.Errorf("EINSORA_REGION %q isn't in the catalog's regions", cfg.Region)
	}
	var backend cluster.Backend
	var hub *agents.Hub
	switch cfg.ClusterBackend {
	case "agent":
		hub = agents.New(st, cat, agents.Options{Region: cfg.Region, AllowSim: cfg.Env == "dev"}, log.With("component", "agents"), nil)
		backend = hub
	case "kubernetes":
		b, err := kube.New(cat, kube.Options{
			StorageClass:          cfg.StorageClass,
			ImagesNamespace:       cfg.ImagesNamespace,
			UntrustedRuntimeClass: cfg.UntrustedRuntimeClass,
		})
		if err != nil {
			return nil, err
		}
		backend = b
	default:
		backend = fake.New(cfg.FakeCapacity)
	}
	ctrl := controller.New(st, backend, cat, controller.Options{
		Region:           cfg.Region,
		ProvisionTimeout: cfg.ProvisionTimeout,
	}, log.With("component", "controller"), nil)
	run := func(ctx context.Context) error { return ctrl.Run(ctx, cfg.ControllerInterval) }
	if hub == nil {
		return func(ctx context.Context) error {
			defer ctrl.Release(ctx)
			return run(ctx)
		}, nil
	}
	return func(ctx context.Context) error {
		defer ctrl.Release(ctx) // only once this machine's agent and the access proxy have stopped too
		return runAll(ctx, run, runLocalAgent(cfg, log, st, hub, ctrl.Leading()),
			runAccessProxy(cfg, log, st, hub, cat, ctrl.Leading()))
	}, nil
}

// runAccessProxy serves workloads' web servers at their own hosts (ADR-0017), once this process's
// controller leads its region: the agent backend that knows where they listen is the leader's.
func runAccessProxy(cfg config.Config, log *slog.Logger, st store.Store, hub *agents.Hub, cat *catalog.Catalog, leading <-chan struct{}) func(context.Context) error {
	return func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return nil
		case <-leading:
		}
		log = log.With("component", "access")
		svc := service.New(st, cat, nil)
		srv := &http.Server{Addr: cfg.AccessAddr, Handler: access.New(st, hub, log, nil).WithAPITokens(svc), ReadHeaderTimeout: 10 * time.Second}
		errc := make(chan error, 1)
		go func() { errc <- srv.ListenAndServe() }()
		log.Info("access proxy listening", "addr", cfg.AccessAddr)
		select {
		case err := <-errc:
			return fmt.Errorf("access proxy: %w", err)
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			return srv.Shutdown(shutdown)
		}
	}
}

// runLocalAgent registers this machine, owned by EINSORA_MACHINE_OWNER, and runs its agent in this
// process (Solo, ADR-0016), once this process's controller leads its region: a standby's agent
// would sync with a controller that isn't reconciling. It waits for the owner's tenant, which
// `einsora setup` creates.
func runLocalAgent(cfg config.Config, log *slog.Logger, st store.Store, hub *agents.Hub, leading <-chan struct{}) func(context.Context) error {
	return func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return nil
		case <-leading:
		}
		log = log.With("component", "agent")
		name := machineName()
		idFile, err := machineIDFile(cfg)
		if err != nil {
			return err
		}
		known, err := readMachineID(idFile)
		if err != nil {
			return err
		}
		if idFile != "" {
			// One agent per machine, whatever the lease's timing: another may still be stopping.
			release, err := waitForHostLock(ctx, filepath.Join(filepath.Dir(idFile), "agent.lock"), log)
			if err != nil || release == nil {
				return err
			}
			defer release()
		}
		warned := false
		for {
			m, err := agents.RegisterLocal(ctx, st, known, cfg.MachineOwner, name, runtime.GOOS, runtime.GOARCH, cfg.Region, time.Now())
			if err == nil && known == uuid.Nil && idFile != "" {
				err = writeMachineID(idFile, m.ID)
			}
			if err == nil {
				log.Info("this machine is registered", "machine", m.Name, "id", m.ID, "owner", cfg.MachineOwner, "region", m.Region)
				drivers := localDrivers(cfg, idFile, log)
				defer func() {
					for _, driver := range drivers {
						if closer, ok := driver.(interface{ Close() }); ok {
							closer.Close()
						}
					}
				}()
				if len(drivers) == 0 {
					log.Warn("this machine's agent has no drivers it can use, so workloads wait for a machine that can run them; " +
						"EINSORA_AGENT_DRIVERS=sim simulates them in dev")
				}
				return agent.New(hub.Conn(m.ID), drivers, inventory.Detect, "dev", log).Run(ctx)
			}
			if !domain.IsNotFound(err) {
				return fmt.Errorf("register this machine: %w", err)
			}
			if !warned {
				log.Warn("this machine's owner tenant doesn't exist yet; create it with `einsora setup`, or set EINSORA_MACHINE_OWNER",
					"tenant", cfg.MachineOwner)
				warned = true
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(5 * time.Second):
			}
		}
	}
}

// machineIDFile is where this machine's ID is kept between starts: EINSORA_AGENT_STATE_DIR, or the
// SQLite database's directory. With the in-memory store (dev) nothing is kept, since the database
// itself isn't.
func machineIDFile(cfg config.Config) (string, error) {
	dir := cfg.AgentStateDir
	if dir == "" {
		db, err := cfg.Database()
		if err != nil || db.Driver != "sqlite" {
			return "", err
		}
		dir = filepath.Dir(db.DSN)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("agent state directory: %w", err)
	}
	// Whoever can write machine-id decides which machine this agent acts as, so the directory gets
	// the same rule as the database's: no one else may write to it.
	di, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("agent state directory: %w", err)
	}
	if di.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("agent state directory %s can be written by other users (mode %v); run: chmod go-w %s", dir, di.Mode().Perm(), dir)
	}
	return filepath.Join(dir, "machine-id"), nil
}

// localDrivers builds this machine's drivers: the ones EINSORA_AGENT_DRIVERS names, or every real
// one this machine can run. A driver that can't run here is skipped, and the log says why.
func localDrivers(cfg config.Config, idFile string, log *slog.Logger) []agent.Driver {
	names := cfg.AgentDrivers
	if len(names) == 0 {
		names = []string{domain.DriverMacOSNative, domain.DriverDocker}
	}
	var drivers []agent.Driver
	for _, name := range names {
		switch name {
		case domain.DriverSim:
			drivers = append(drivers, sim.New(domain.AcceleratorNVIDIA, domain.AcceleratorApple, domain.AcceleratorAMD, domain.AcceleratorCPU))
		case domain.DriverDocker:
			if idFile == "" {
				log.Warn("docker needs a persistent agent state directory")
				continue
			}
			d, err := docker.New(docker.Options{Root: filepath.Join(filepath.Dir(idFile), "docker-state"), Runs: cfg.AgentRunDir, Socket: cfg.DockerSocket, Log: log.With("driver", name)})
			if err != nil {
				log.Info("driver not available here", "driver", name, "why", redact.Secrets(err.Error()))
				continue
			}
			drivers = append(drivers, d)
		case domain.DriverMacOSNative:
			if idFile == "" {
				log.Warn("macos-native needs a state directory: use a SQLite database (EINSORA_DATABASE_URL=sqlite:...) or set EINSORA_AGENT_STATE_DIR")
				continue
			}
			private, err := privateFiles(cfg)
			if err != nil {
				log.Warn("macos-native can't keep workloads out of the control plane's files; not starting it", "err", err)
				continue
			}
			state := filepath.Dir(idFile)
			d, err := macosnative.New(macosnative.Options{Root: filepath.Join(state, "native"), State: state, Private: private,
				Runs: cfg.AgentRunDir, Log: log.With("driver", name)})
			if err != nil {
				log.Info("driver not available here", "driver", name, "why", err.Error())
				continue
			}
			drivers = append(drivers, d)
		}
	}
	return drivers
}

// privateFiles are the control plane's files that no workload may read or write, wherever they are:
// the SQLite database with its journal files, and the log file.
func privateFiles(cfg config.Config) ([]string, error) {
	db, err := cfg.Database()
	if err != nil {
		return nil, err
	}
	var out []string
	if db.Driver == "sqlite" {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			out = append(out, db.DSN+suffix)
		}
	}
	if cfg.LogFile != "" {
		abs, err := filepath.Abs(cfg.LogFile)
		if err != nil {
			return nil, fmt.Errorf("EINSORA_LOG_FILE: %w", err)
		}
		out = append(out, abs)
	}
	return out, nil
}

// waitForHostLock takes this machine's agent lock, waiting while another agent (one still
// stopping) holds it. It returns a nil release if ctx ends first.
func waitForHostLock(ctx context.Context, path string, log *slog.Logger) (func(), error) {
	warned := false
	for {
		release, err := hostlock.Acquire(path)
		switch {
		case err == nil:
			return release, nil
		case !errors.Is(err, hostlock.ErrHeld):
			return nil, err
		case !warned:
			log.Warn("another agent is still running for this machine; waiting for it to stop")
			warned = true
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(time.Second):
		}
	}
}

// readMachineID returns the ID saved by an earlier start, or uuid.Nil before the first.
func readMachineID(path string) (uuid.UUID, error) {
	if path == "" {
		return uuid.Nil, nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("read this machine's ID: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return uuid.Nil, fmt.Errorf("%s must be a regular file only its owner can read and write (not a symlink); run: chmod 600 %s", path, path)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the path comes from operator config
	if err != nil {
		return uuid.Nil, fmt.Errorf("read this machine's ID: %w", err)
	}
	id, err := uuid.Parse(strings.TrimSpace(string(raw)))
	if err != nil {
		return uuid.Nil, fmt.Errorf("%s doesn't hold a machine ID; restore it, or remove it to register this machine anew", path)
	}
	return id, nil
}

// writeMachineID saves the machine's ID, readable by its owner only. It never replaces a file.
func writeMachineID(path string, id uuid.UUID) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the path comes from operator config
	if err != nil {
		return fmt.Errorf("save this machine's ID: %w", err)
	}
	if _, err := f.WriteString(id.String() + "\n"); err != nil {
		_ = f.Close()
		return fmt.Errorf("save this machine's ID: %w", err)
	}
	return f.Close()
}

// machineName is this machine's name for its record: its hostname, as a slug. It's a label: the
// machine is found by its saved ID, so a new hostname changes nothing else.
func machineName() string {
	host, _ := os.Hostname()
	host = strings.TrimSuffix(strings.ToLower(host), ".local")
	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 40 {
		name = strings.Trim(name[:40], "-")
	}
	if !domain.ValidSlug(name) {
		return "this-machine"
	}
	return name
}

// apiServer is the API, able to open workloads through the access proxy when the agent backend
// runs it (ADR-0017).
func apiServer(cfg config.Config, svc *service.Service, authn auth.Authenticator, log *slog.Logger, st store.Store) *api.Server {
	s := api.New(svc, authn, log.With("component", "api"), st.Ping)
	if cfg.ClusterBackend == "agent" {
		if _, port, err := net.SplitHostPort(cfg.AccessAddr); err == nil {
			if p, err := strconv.Atoi(port); err == nil {
				s.WithAccessPort(p)
			}
		}
	}
	return s
}

// serveAPI serves the API, and the dashboard when this binary carries it, on l (nil: listen on
// EINSORA_HTTP_ADDR).
func serveAPI(ctx context.Context, cfg config.Config, log *slog.Logger, st store.Store, cat *catalog.Catalog, l net.Listener) error {
	svc := service.New(st, cat, nil)
	var authn auth.Authenticator
	switch cfg.AuthMode {
	case "oidc":
		o, err := auth.NewOIDC(ctx, cfg.OIDCIssuer, cfg.OIDCAudience, cfg.OIDCTenantClaim)
		if err != nil {
			return err
		}
		authn = o
	case "local":
		authn = auth.NewLocal(svc, auth.Transport{TrustedProxies: cfg.TrustedProxies, AllowPlainHTTP: cfg.InsecureHTTP})
		if n, err := countUsers(ctx, st); err == nil && n == 0 {
			log.Warn("local auth: no accounts yet; create the owner with `einsora setup`")
		}
		if !listensOnlyOnLoopback(cfg.HTTPAddr) && !cfg.InsecureHTTP && len(cfg.TrustedProxies) == 0 {
			log.Warn("local auth: listening beyond this machine, but sign-in from other machines needs HTTPS "+
				"through a trusted proxy (EINSORA_TRUSTED_PROXIES), or EINSORA_INSECURE_HTTP=true on a network you trust",
				"addr", cfg.HTTPAddr)
		}
		for _, p := range cfg.TrustedProxies {
			if p.Bits() < p.Addr().BitLen() {
				log.Warn("local auth: EINSORA_TRUSTED_PROXIES lists a range; every address in it can claim any client "+
					"address and claim HTTPS, so list only your proxies' own addresses", "range", p.String())
			}
		}
	default:
		authn = auth.Dev{}
		log.Warn("dev auth enabled: the X-Dev-Tenant header is trusted")
	}
	handler := apiServer(cfg, svc, authn, log, st).Handler()
	if files := webui.Files(); webui.Built(files) {
		handler = withDashboard(handler, webui.Handler(files))
	}
	if l == nil {
		var err error
		if l, err = listen(cfg.HTTPAddr); err != nil {
			return err
		}
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		log.Info("api listening", "addr", l.Addr().String())
		errc <- srv.Serve(l)
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// withDashboard serves the API's paths from api, and everything else from dashboard: the dashboard
// and the API share one origin, so the dashboard's session cookie and CSRF header just work.
func withDashboard(api, dashboard http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/v1" || strings.HasPrefix(p, "/v1/") || p == "/healthz" || p == "/readyz" {
			api.ServeHTTP(w, r)
			return
		}
		dashboard.ServeHTTP(w, r)
	})
}

// runAll runs fns concurrently; when any returns, the rest are cancelled. Returns the first error.
func runAll(ctx context.Context, fns ...func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, len(fns))
	for _, fn := range fns {
		go func() { errc <- fn(ctx) }()
	}
	var first error
	for range fns {
		if err := <-errc; err != nil && first == nil {
			first = err
		}
		cancel()
	}
	return first
}
