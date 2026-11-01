// Mirrors controlplane/internal/api/json.go. Change both together.

export type WorkloadKind = "vm" | "notebook" | "endpoint";

export type WorkloadState =
  | "pending"
  | "queued"
  | "provisioning"
  | "running"
  | "stopping"
  | "stopped"
  | "failed"
  | "deleting"
  | "deleted";

export interface Workload {
  id: string;
  name: string;
  kind: WorkloadKind;
  state: WorkloadState;
  image_id: string;
  /** The class requested. */
  gpu_class: string;
  gpu_count: number;
  created_at: string;
  updated_at: string;
  /** Where it runs (ADR-0009). */
  region: string;
  /** What it was admitted on; absent until admitted, and different from gpu_class only after an opt-in fallback (ADR-0010). */
  placed_class?: string;
  /** On node agents: the machine that runs it, and the driver (ADR-0016). */
  machine_id?: string;
  driver?: string;
}

export interface CreateWorkload {
  name: string;
  kind: WorkloadKind;
  image_id: string;
  gpu_class: string;
  gpu_count: number;
  ssh_public_key?: string;
  model_uri?: string;
  /** Where it runs and its data stays (ADR-0009); omitted means the catalog's first region. */
  region?: string;
}

export type Accelerator = "nvidia" | "apple" | "amd" | "cpu";

export interface Region {
  id: string;
  display_name: string;
}

export interface GpuClass {
  id: string;
  display_name: string;
  accelerator: Accelerator;
  vram_gb: number;
  supports_vm: boolean;
  /** Regions offering it. */
  regions: string[];
}

/** What serves an image on which accelerators, such as vLLM on NVIDIA or MLX on Apple Silicon. */
export interface ImageRuntime {
  accelerators: Accelerator[];
  server?: string;
}

export interface CatalogImage {
  id: string;
  display_name: string;
  kind: WorkloadKind;
  description: string;
  /** In order of preference; the first that supports a class's accelerator is used. */
  runtimes: ImageRuntime[];
}

export interface Catalog {
  /** The first is the default. */
  regions: Region[];
  gpu_classes: GpuClass[];
  images: CatalogImage[];
}

export interface List<T> {
  items: T[];
}

/** One entry in a workload's timeline: a state change, or a note when `from` equals `to` (ADR-0015). */
export interface WorkloadEvent {
  id: number;
  from?: WorkloadState;
  to: WorkloadState;
  reason: string;
  at: string;
}

/** Who's signed in (GET /v1/auth/me). */
export interface Me {
  subject: string;
  email?: string;
  tenant: string;
  auth_mode: "local" | "oidc" | "dev";
}

/** After a password change: other sessions are signed out, but API tokens keep working. */
export interface PasswordChanged {
  status: string;
  /** API tokens that still work; revoke them too if the password leaked. */
  api_tokens_active: number;
}

/** An API token, as listed. The token itself is shown only once, when it's created. */
export interface ApiToken {
  id: string;
  name: string;
  prefix: string;
  created_at: string;
  expires_at?: string;
  last_used_at?: string;
  revoked_at?: string;
}

export interface CreatedApiToken extends ApiToken {
  token: string;
}

export interface ApiErrorBody {
  /** request_id matches the X-Request-ID header and the operator's log lines. */
  error: { code: string; message: string; request_id?: string };
}

/** Notebook links are one-time; endpoint base URLs require an API token on every request. */
export interface AccessGrant {
  url: string;
  expires_at?: string;
}

/** A GPU (or an Apple Silicon GPU) on a machine, as its agent reported it. Mirrors domain.Accelerator. */
export interface MachineAccelerator {
  kind: Accelerator;
  model: string;
  count: number;
  memory_mib: number; // per device; unified memory on Apple Silicon
}

/**
 * One of your machines (GET /v1/machines). Mirrors api.machineView. Its status comes from when its
 * agent was last seen: online if within the last 3 minutes.
 */
export interface Machine {
  id: string;
  name: string;
  region: string;
  os: "darwin" | "linux";
  arch: "arm64" | "amd64";
  cpus: number;
  memory_mib: number;
  accelerators: MachineAccelerator[];
  sharing: "private" | "invite" | "community";
  status: "online" | "offline";
  last_seen_at?: string;
  workloads: number; // your workloads holding it now
  created_at: string;
}
