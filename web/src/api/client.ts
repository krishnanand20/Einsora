import type {
  AccessGrant,
  ApiErrorBody,
  ApiToken,
  Catalog,
  CreatedApiToken,
  CreateWorkload,
  List,
  Machine,
  Me,
  OutputPage,
  PasswordChanged,
  Workload,
  WorkloadEvent,
} from "./types";

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    readonly requestId?: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

export function toApiError(status: number, body: unknown, requestId?: string | null): ApiError {
  const err = (body as Partial<ApiErrorBody> | undefined)?.error;
  const id = err?.request_id ?? requestId ?? undefined;
  const message = err?.message ?? `Request failed (HTTP ${status})`;
  // Show the request ID so users can hand it to an operator to find the exact log lines.
  return new ApiError(status, err?.code ?? "unknown", id ? `${message} (request ID ${id})` : message, id);
}

/** Reads the file name from a Content-Disposition header; never returns a path. */
export function filenameFromDisposition(header: string | null, fallback: string): string {
  const match = header?.match(/filename\*?=(?:UTF-8'')?"?([^";]+)"?/i);
  let name = fallback;
  try {
    name = match?.[1] ? decodeURIComponent(match[1]) : fallback;
  } catch {
    // Malformed percent-encoding: keep the fallback.
  }
  return name.split(/[\\/]/).pop() || fallback;
}

/** Path to one workload. The ID is encoded, so it can never point at a different API path. */
export function workloadPath(id: string, suffix = ""): string {
  return `/v1/workloads/${encodeURIComponent(id)}${suffix}`;
}

function authHeaders(): Record<string, string> {
  // Local accounts sign in with a session cookie, which the browser sends by itself (same origin).
  // Every request also carries the CSRF header: another site can't add it, so the API knows the
  // request came from this dashboard. Dev mode adds X-Dev-Tenant. OIDC with PKCE arrives with the
  // Datacenter profile (phase 3). Tokens never go in localStorage.
  const headers: Record<string, string> = { "X-Einsora-CSRF": "1" };
  const tenant = import.meta.env.VITE_DEV_TENANT ?? (import.meta.env.DEV ? "acme" : undefined);
  if (tenant) headers["X-Dev-Tenant"] = tenant;
  return headers;
}

/** Where to go after signing in: only a path inside the dashboard, never another site. */
export function safeNext(next: string | null): string {
  return next && next.startsWith("/") && !next.startsWith("//") && !next.startsWith("/\\") ? next : "/";
}

async function send(method: string, path: string, body?: unknown): Promise<Response> {
  const headers: Record<string, string> = authHeaders();
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const res = await fetch(path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) {
    const text = await res.text();
    throw toApiError(res.status, text ? JSON.parse(text) : undefined, res.headers.get("X-Request-ID"));
  }
  return res;
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const text = await (await send(method, path, body)).text();
  return (text ? JSON.parse(text) : undefined) as T;
}

/** Downloads everything Einsora records about a workload, exactly as the server sent it. */
async function exportWorkload(id: string): Promise<{ blob: Blob; filename: string }> {
  const res = await send("GET", workloadPath(id, "/export"));
  return {
    blob: await res.blob(),
    filename: filenameFromDisposition(res.headers.get("Content-Disposition"), `einsora-workload-${id}.json`),
  };
}

export const api = {
  catalog: () => request<Catalog>("GET", "/v1/catalog"),
  /** includeDeleted adds deleted workloads, whose timeline and full log stay available (ADR-0015). */
  me: () => request<Me>("GET", "/v1/auth/me"),
  login: (email: string, password: string) => request<Me>("POST", "/v1/auth/login", { email, password }),
  logout: () => request<unknown>("POST", "/v1/auth/logout"),
  changePassword: (current_password: string, new_password: string) =>
    request<PasswordChanged>("POST", "/v1/auth/password", { current_password, new_password }),
  listTokens: () => request<List<ApiToken>>("GET", "/v1/tokens"),
  /** Needs the current password: a token outlives the session that makes it. */
  createToken: (name: string, expires_in_days: number, password: string) =>
    request<CreatedApiToken>("POST", "/v1/tokens", { name, expires_in_days, password }),
  revokeToken: (id: string) => request<unknown>("DELETE", `/v1/tokens/${encodeURIComponent(id)}`),
  listWorkloads: (includeDeleted = false) =>
    request<List<Workload>>("GET", includeDeleted ? "/v1/workloads?include_deleted=true" : "/v1/workloads"),
  listMachines: () => request<List<Machine>>("GET", "/v1/machines"),
  getWorkload: (id: string, includeDeleted = false) =>
    request<Workload>("GET", workloadPath(id, includeDeleted ? "?include_deleted=true" : "")),
  workloadEvents: (id: string) => request<List<WorkloadEvent>>("GET", workloadPath(id, "/events")),
  /** Output after the chunk ID after (ADR-0024). */
  workloadOutput: (id: string, after: number) =>
    request<OutputPage>("GET", workloadPath(id, `/logs?after=${encodeURIComponent(String(after))}`)),
  exportWorkload,
  /** A notebook's one-time link or an endpoint's API base URL (ADR-0017). */
  openWorkload: (id: string) => request<AccessGrant>("POST", workloadPath(id, "/access")),
  createWorkload: (input: CreateWorkload) => request<Workload>("POST", "/v1/workloads", input),
  stopWorkload: (id: string) => request<Workload>("POST", workloadPath(id, "/stop")),
  startWorkload: (id: string) => request<Workload>("POST", workloadPath(id, "/start")),
  deleteWorkload: (id: string) => request<Workload>("DELETE", workloadPath(id)),
};
