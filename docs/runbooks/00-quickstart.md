# 00: Quickstart (Solo: one Mac or Linux machine)

> **Status: `einsora up` works today, built from source** (below). The one-line installs (Homebrew and an
> install script) arrive with packaging, roadmap step 11. The development stack is at the end of this page.

## Requirements

| Machine | Minimum | For bigger models |
|---|---|---|
| Mac | Apple Silicon (M1 or newer), macOS 14+, 16 GB unified memory, 30 GB free disk | 64 GB+ unified memory |
| Linux + NVIDIA | Ubuntu 22.04+ (or similar), NVIDIA driver installed, Docker with the NVIDIA container toolkit, 16 GB RAM | A GPU with 24 GB+ VRAM |
| Linux, CPU only | Any 64-bit Linux with Docker, 16 GB RAM | — |

## Install

**Today, from source** (Go, Node, and uv: `make doctor` checks them; `brew install go node uv` on a Mac):

```bash
git clone https://github.com/krishnanand20/Einsora.git && cd Einsora
make build                      # the einsora binary, with its dashboard built in
sudo cp controlplane/bin/einsora /usr/local/bin/
```

**Planned** (roadmap step 11): `brew install krishnanand20/tap/einsora` on a Mac, and
`curl -fsSL https://einsora.com/install.sh | sh` on Linux.

## Start

```bash
einsora up
```

`einsora up` does the following:

- creates its data directory, readable by you only (macOS: `~/Library/Application Support/Einsora`; Linux:
  `~/.local/share/einsora`; `EINSORA_DATA_DIR` picks another): the database, this machine's identity, and
  workloads' data, plus `einsora.log`, a complete copy of the log
- starts the API and dashboard, the controller with this machine's agent, and usage accounting in one
  process, on `127.0.0.1:8080` only (`EINSORA_HTTP_ADDR` picks another)
- the first time, asks you to create the owner account in the terminal (without a terminal, create it with
  `einsora setup -email you@example.com`: there's no sign-up page anyone else could use)
- opens the dashboard in your browser (`einsora up -no-open` doesn't)

Ctrl+C stops it. Notebooks and endpoints that are running keep running, and `einsora up` finds them
again the next time. Every `EINSORA_*` setting in [.env.example](../../.env.example) still applies.

## Launch something

1. **Notebook:** Launch → Notebook → "PyTorch + JupyterLab". It uses Metal on a Mac, CUDA on NVIDIA, or CPU.
2. **Model endpoint:** Launch → Model endpoint. Pick a small model in safetensors (on a Mac, an MLX
   model such as `hf://mlx-community/Qwen2.5-0.5B-Instruct-4bit`; GGUF arrives with llama.cpp, roadmap step 10b).
   Its page shows the OpenAI-compatible base URL under **API connection**; call it with an API token
   (see [Run a model endpoint on your Mac](#run-a-model-endpoint-on-your-mac)).

3. **VM** (Phase 2): Launch → GPU VM.

## See what happened

Every workload keeps a full timeline: each state change, when, why, and by whom. Open the workload in
the dashboard and click **Download full log** to save everything about it, or use the CLI:

```bash
einsora events <id>      # the timeline
einsora export <id>      # a complete copy, as JSON
einsora workloads -all   # deleted workloads too: their history stays available
```

If something fails, the error shows a request ID. It points to the exact lines in the log
(`EINSORA_LOG_LEVEL=debug` for more detail). See [logs and transparency](../architecture/logs-and-transparency.md).

## Keep it private

`einsora up` listens only on localhost, and your machine is **private** until you choose otherwise.
To add more machines, or to share with others, see the Lab and sharing sections of the
[product spec](../product/product-spec.md#how-you-deploy-it).

## What works today

The development stack runs on macOS (Apple Silicon) and Linux. By default it uses a fake backend, so
nothing runs on your GPU; on a Mac, a real notebook can (below):

```bash
brew install go uv node       # on a Mac, if `make doctor` reports them missing
make setup && make dev        # API on :8080
make web-dev                  # dashboard on :5173
```

The CLI's read commands work today against `make dev`:

```bash
cd controlplane && go build -o bin/einsora ./cmd/einsora
EINSORA_DEV_TENANT=acme bin/einsora workloads
EINSORA_DEV_TENANT=acme bin/einsora events <id>
EINSORA_DEV_TENANT=acme bin/einsora export <id>
```

To try local accounts (sign-in and API tokens) instead of the dev header, keep the data in a SQLite file:

```bash
cd controlplane
export EINSORA_DATABASE_URL=sqlite:$PWD/einsora.db EINSORA_AUTH_MODE=local
go run ./cmd/einsora setup -email you@example.com   # once: asks for a password (12+ characters)
go run ./cmd/einsora dev                            # then `make web-dev`, open :5173, and sign in
```

On the dashboard's **API tokens** page, create a token for the CLI and set `EINSORA_TOKEN` to it. Forgot the
password? `go run ./cmd/einsora setup -reset-password -email you@example.com` sets a new one.

Sign-in works over HTTPS or from this machine. To sign in from another machine, put an HTTPS reverse proxy in
front (Caddy, nginx, cloudflared, or Tailscale Serve, sending `X-Forwarded-For` and `X-Forwarded-Proto`)
and list its address in `EINSORA_TRUSTED_PROXIES`, even when it runs on this machine. Only on a network you
trust, `EINSORA_INSECURE_HTTP=true` allows plain HTTP instead.

To try the node agent on this machine ([ADR-0016](../adr/0016-agent-protocol-and-machine-placement.md)) without
running anything, use the agent backend with the `sim` driver. It only pretends to run workloads, but
everything else is real: this machine is registered and its inventory detected, placement picks it and a driver,
and the timeline says so.

```bash
cd controlplane
EINSORA_CLUSTER_BACKEND=agent EINSORA_AGENT_DRIVERS=sim EINSORA_MACHINE_OWNER=internal go run ./cmd/einsora dev
# in another terminal: a notebook on this Mac's GPU (use the tier its memory holds, or a CPU class such as cpu-2c-8g)
curl -s -X POST -H 'X-Dev-Tenant: internal' localhost:8080/v1/workloads \
  -d '{"name":"nb","kind":"notebook","image_id":"pytorch-notebook","gpu_class":"apple-silicon-32gb","gpu_count":1}'
EINSORA_DEV_TENANT=internal go run ./cmd/einsora events <id>   # admitted: 1 × apple-silicon-32gb on <this machine> with sim
```

### Run a real notebook on your Mac

On an Apple Silicon Mac, the `macos-native` driver runs JupyterLab with PyTorch on the Mac's GPU (Metal), in a
sandbox ([ADR-0017](../adr/0017-local-workload-access-and-native-drivers.md)). It needs
[uv](https://docs.astral.sh/uv/) and a SQLite file for its state:

```bash
brew install uv
cd controlplane
export EINSORA_DATABASE_URL=sqlite:$PWD/einsora.db EINSORA_AUTH_MODE=local EINSORA_CLUSTER_BACKEND=agent
go run ./cmd/einsora setup -email you@example.com   # once, if you haven't
go run ./cmd/einsora dev                            # then `make web-dev`, open :5173, and sign in
```

Launch a **Notebook** on the Apple Silicon class your Mac's memory holds. The first one builds its Python
environment, a download of a few hundred MB (about 750 MB on disk, in `native/` next to the database), so it
stays provisioning for a minute or more, depending on your connection; later ones start in seconds. Once it's running, **Open** on its page opens
JupyterLab in a new tab at `w-<id>.localhost:8090`, its own site, through the access proxy. Its files are in
`native/workloads/<id>/data`, and survive stopping and starting it, and restarting `einsora`. Deleting the
workload deletes them. You can also explicitly choose a CPU class: the native driver uses the same
pinned environment and supports normal CPU PyTorch operations. Native code can still use Metal, so
the controller and agent reserve the Apple GPU exclusively while that CPU notebook runs; the
admission timeline says so. CPU-native notebooks and GPU workloads wait for each other, even when
there is spare CPU and memory. Usage remains on the requested CPU class. CPU endpoints await
llama.cpp (roadmap step 10b). A native notebook can't listen on a network port, so web apps started inside it (TensorBoard, Gradio) and
JupyterLab's debugger don't work yet; this keeps code in a notebook from getting your dashboard session. Its
sockets and temporary files are in `~/.einsora/run` (`EINSORA_AGENT_RUN_DIR`).

After an upgrade that changes the native sandbox, existing notebooks stop and their timeline asks
you to restart them to use the updated protections. Their files are kept. This also applies when
upgrading from the older implementation that used TCP ports. Start the workload again from the
dashboard; an ordinary `einsora` restart with the same sandbox keeps notebooks running.

### Run a model endpoint on your Mac

With the same local-account setup on macOS 14+ and Apple Silicon, launch a **Model endpoint**, choose
the Apple Silicon class your Mac supports, and supply a safetensors model such as
[`hf://mlx-community/Qwen2.5-0.5B-Instruct-4bit`](https://huggingface.co/mlx-community/Qwen2.5-0.5B-Instruct-4bit). The first launch installs a separate pinned MLX
environment. Its timeline shows downloading, validation, and loading. A model must use an MLX-supported
architecture and code-free safetensors; custom model code, pickle, and GGUF are refused. The model's
files must fit the workload's memory allocation, and runtime overhead also needs room.

Once it is running, **API connection** on the workload page shows the OpenAI-compatible base URL.
Alternatively, request it with `POST /v1/workloads/<id>/access`. Create an API token on **API tokens**
and keep it in `EINSORA_TOKEN`. Use the endpoint URL as `BASE_URL`:

```bash
curl -s "$BASE_URL/models" -H "Authorization: Bearer $EINSORA_TOKEN"
curl -s "$BASE_URL/chat/completions" -H "Authorization: Bearer $EINSORA_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"model":"hf://mlx-community/Qwen2.5-0.5B-Instruct-4bit","messages":[{"role":"user","content":"Hello"}],"max_tokens":64}'
```

Use the exact model ID returned by `/models`. `stream: true` enables SSE; `stream_options:
{"include_usage": true}` includes token counts. Supported sampling options are `max_tokens`,
`temperature`, and `top_p`. The endpoint serves text requests for this single model; tools, images,
alternate models, and adapters are refused. It serializes generation and returns 503 while busy.
The API accepts up to 1 MiB per request, 8192 prompt-plus-output tokens, and 4096 output tokens.
To measure it, run `einsora-ml bench` with the same URL and token, one request at a time
([Benchmark an endpoint](../../ml/README.md#benchmark-an-endpoint)).

For private model downloads, start `einsora` with `HF_TOKEN` or the standard explicit AWS credential
environment variables (and `AWS_ENDPOINT_URL_S3` for another S3-compatible store). Credentials never
go in `model_uri` or the endpoint URL. They are removed before the model-serving process starts,
which loads the validated local snapshot offline. Snapshots stay private to each workload; stopping
or deleting an endpoint removes its model data. Existing API-token expiry, revocation, and account
disablement apply to every inference request. Response headers identify region, actual compute class,
and whether a fallback was used. This access path is local to the control plane's machine; remote
relaying and endpoint-scoped keys are Phase 2.

### Run real workloads on Linux

The `docker` driver runs the machine owner's notebooks on CPU or NVIDIA, and safetensors model
endpoints on NVIDIA ([ADR-0018](../adr/0018-linux-docker-driver.md)). Linux/CUDA hardware validation
is pending; the daemon-backed notebook test below is opt-in.

Use an already configured local **rootful Docker Engine**, without `userns-remap`, accepting Engine
API 1.41–1.47 and enforcing CPU, memory, swap, and PID cgroup limits. The user running `einsora`
needs access to its Unix socket. NVIDIA also needs the NVIDIA container runtime and `nvidia-smi`
physical GPU discovery. Einsora reports an unavailable driver if these daemon requirements fail;
it does not install or reconfigure Docker or the host. Rootless and remote daemons aren't supported.

Build the [local pinned notebook and endpoint images](../../images/README.md#build-linux-solo-images)
first. Then, from the repository root:

```bash
cd controlplane
export EINSORA_DATABASE_URL=sqlite:$PWD/einsora.db EINSORA_AUTH_MODE=local EINSORA_CLUSTER_BACKEND=agent
export EINSORA_AGENT_DRIVERS=docker
go run ./cmd/einsora setup -email you@example.com   # once
go run ./cmd/einsora dev                            # then `make web-dev` from the repo root
```

Sign in as the owner and launch a notebook on an available **CPU** class or a class matching the
NVIDIA GPU's exact catalog model. If no NVIDIA runtime is usable, only CPU is advertised: a GPU
request waits and is never silently converted to CPU. An NVIDIA model endpoint uses the same API
connection and local API-token flow as the Mac example, with a vLLM-supported safetensors model
such as `hf://Qwen/Qwen2.5-0.5B-Instruct`. Only the validated local snapshot is loaded; GGUF and
CPU endpoints wait for llama.cpp (roadmap step 10b).

Each workload has a non-root container, resource limits, private directories and network, and no
published ports. Notebook **Open** and the endpoint API use the existing access proxy and private
Unix socket. `EINSORA_DOCKER_SOCKET` selects an absolute local socket path (default
`/var/run/docker.sock`). State and notebook files live in `docker-state/` next to the database.
The default socket directory is `docker-state/run/`; set `EINSORA_AGENT_RUN_DIR` to a shorter,
private persistent path if the default exceeds the socket path limit.

Stopping keeps notebook files; deletion removes them. Endpoints' files are removed on stop by the
controller's explicit removal intent. Running containers survive a Einsora restart and are adopted
again; interrupted preparation and cleanup resume. Changed worker policies stop old endpoints and
ask for a restart. Lost saved notebook disks fail rather than creating empty replacements.
These containers are only for the owner's own workloads: runc shares the host kernel and outbound
network access includes the LAN. Disk quotas and exporting container output are still planned.

On a Linux test host with the notebook image built, verify real Jupyter startup, private socket
access, file persistence across stop/start, and cleanup:

```bash
cd controlplane
EINSORA_TEST_DOCKER_NOTEBOOK_IMAGE=ghcr.io/krishnanand20/einsora-pytorch-notebook:0.1.0 \
  go test ./internal/agent/drivers/docker -run TestRealDockerNotebook -count=1 -v
```

This test creates and removes only its own workload on the existing daemon. The regular Go suite
uses a local Engine API fixture to test cancellation, ownership, cgroup refusal, exact GPU UUID
reservations, and recovery, without Docker or a GPU.

On a Mac with an existing driver-built Jupyter environment, the optional notebook integration test
checks real sandboxed kernels over IPC on both CPU and Metal, without downloading packages:

```bash
cd controlplane
EINSORA_TEST_NATIVE_ENV=/absolute/path/to/native/envs/jupyterlab-pytorch-mps-<hash> \
  go test ./internal/agent/drivers/macosnative -run TestRealNotebook -count=1 -v
```
