# 00: Quickstart (Solo: one Mac or Linux machine)

> **Status: planned (Phase 1).** The install commands below describe the target experience and don't
> work yet. What works **today** on macOS and Linux is the development stack (see the end of this page).
> Turn this runbook into a verified one when Phase 1 lands.

## Requirements

| Machine | Minimum | For bigger models |
|---|---|---|
| Mac | Apple Silicon (M1 or newer), macOS 14+, 16 GB unified memory, 30 GB free disk | 64 GB+ unified memory |
| Linux + NVIDIA | Ubuntu 22.04+ (or similar), NVIDIA driver installed, Docker with the NVIDIA container toolkit, 16 GB RAM | A GPU with 24 GB+ VRAM |
| Linux, CPU only | Any 64-bit Linux with Docker, 16 GB RAM | — |

## Install (target experience)

```bash
# macOS
brew install tensora/tap/tensora

# Linux
curl -fsSL https://get.tensora.example/install.sh | sh
```

## Start

```bash
tensora up
```

`tensora up` does the following:

- creates its data directory (macOS: `~/Library/Application Support/Tensora`; Linux: `~/.local/share/tensora`)
- starts the control plane and the local agent in one process, on `127.0.0.1` only
- asks you to create the first account
- opens the dashboard

## Launch something

1. **Notebook:** Launch → Notebook → "PyTorch + JupyterLab". It uses Metal on a Mac, CUDA on NVIDIA, or CPU.
2. **Model endpoint:** Launch → Model endpoint. Pick a small model (for example a 0.5B–3B instruct model in GGUF or MLX format). You get an OpenAI-compatible URL:

   ```bash
   curl -s http://127.0.0.1:<port>/v1/chat/completions -H 'Content-Type: application/json' \
     -d '{"model":"<name>","messages":[{"role":"user","content":"Hello"}]}'
   ```

3. **VM** (Phase 2): Launch → GPU VM.

## See what happened

Every workload keeps a full timeline: each state change, when, why, and by whom. Open the workload in
the dashboard and click **Download full log** to save everything about it, or use the CLI:

```bash
tensora events <id>      # the timeline
tensora export <id>      # a complete copy, as JSON
tensora workloads -all   # deleted workloads too: their history stays available
```

If something fails, the error shows a request ID. It points to the exact lines in the log
(`TENSORA_LOG_LEVEL=debug` for more detail). See [logs and transparency](../architecture/logs-and-transparency.md).

## Keep it private

`tensora up` listens only on localhost, and your machine is **private** until you choose otherwise.
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
cd controlplane && go build -o bin/tensora ./cmd/tensora
TENSORA_DEV_TENANT=acme bin/tensora workloads
TENSORA_DEV_TENANT=acme bin/tensora events <id>
TENSORA_DEV_TENANT=acme bin/tensora export <id>
```

To try local accounts (sign-in and API tokens) instead of the dev header, keep the data in a SQLite file:

```bash
cd controlplane
export TENSORA_DATABASE_URL=sqlite:$PWD/tensora.db TENSORA_AUTH_MODE=local
go run ./cmd/tensora setup -email you@example.com   # once: asks for a password (12+ characters)
go run ./cmd/tensora dev                            # then `make web-dev`, open :5173, and sign in
```

On the dashboard's **API tokens** page, create a token for the CLI and set `TENSORA_TOKEN` to it. Forgot the
password? `go run ./cmd/tensora setup -reset-password -email you@example.com` sets a new one.

Sign-in works over HTTPS or from this machine. To sign in from another machine, put an HTTPS reverse proxy in
front (Caddy, nginx, cloudflared, or Tailscale Serve, sending `X-Forwarded-For` and `X-Forwarded-Proto`)
and list its address in `TENSORA_TRUSTED_PROXIES`, even when it runs on this machine. Only on a network you
trust, `TENSORA_INSECURE_HTTP=true` allows plain HTTP instead.

To try the node agent on this machine ([ADR-0016](../adr/0016-agent-protocol-and-machine-placement.md)) without
running anything, use the agent backend with the `sim` driver. It only pretends to run workloads, but
everything else is real: this machine is registered and its inventory detected, placement picks it and a driver,
and the timeline says so.

```bash
cd controlplane
TENSORA_CLUSTER_BACKEND=agent TENSORA_AGENT_DRIVERS=sim TENSORA_MACHINE_OWNER=internal go run ./cmd/tensora dev
# in another terminal: a notebook on this Mac's GPU (use the tier its memory holds, or a CPU class such as cpu-2c-8g)
curl -s -X POST -H 'X-Dev-Tenant: internal' localhost:8080/v1/workloads \
  -d '{"name":"nb","kind":"notebook","image_id":"pytorch-notebook","gpu_class":"apple-silicon-32gb","gpu_count":1}'
TENSORA_DEV_TENANT=internal go run ./cmd/tensora events <id>   # admitted: 1 × apple-silicon-32gb on <this machine> with sim
```

### Run a real notebook on your Mac

On an Apple Silicon Mac, the `macos-native` driver runs JupyterLab with PyTorch on the Mac's GPU (Metal), in a
sandbox ([ADR-0017](../adr/0017-local-workload-access-and-native-drivers.md)). It needs
[uv](https://docs.astral.sh/uv/) and a SQLite file for its state:

```bash
brew install uv
cd controlplane
export TENSORA_DATABASE_URL=sqlite:$PWD/tensora.db TENSORA_AUTH_MODE=local TENSORA_CLUSTER_BACKEND=agent
go run ./cmd/tensora setup -email you@example.com   # once, if you haven't
go run ./cmd/tensora dev                            # then `make web-dev`, open :5173, and sign in
```

Launch a **Notebook** on the Apple Silicon class your Mac's memory holds. The first one builds its Python
environment, a download of a few hundred MB (about 750 MB on disk, in `native/` next to the database), so it
stays provisioning for a minute or more, depending on your connection; later ones start in seconds. Once it's running, **Open** on its page opens
JupyterLab in a new tab at `w-<id>.localhost:8090`, its own site, through the access proxy. Its files are in
`native/workloads/<id>/data`, and survive stopping and starting it, and restarting `tensora`. Deleting the
workload deletes them. CPU classes don't run natively on a Mac yet: such a notebook waits, and its timeline says
why. A native notebook can't listen on a network port, so web apps started inside it (TensorBoard, Gradio) and
JupyterLab's debugger don't work yet; this keeps code in a notebook from getting your dashboard session. Its
sockets and temporary files are in `~/.tensora/run` (`TENSORA_AGENT_RUN_DIR`).

After an upgrade that changes the native sandbox, existing notebooks stop and their timeline asks
you to restart them to use the updated protections. Their files are kept. This also applies when
upgrading from the older implementation that used TCP ports. Start the workload again from the
dashboard; an ordinary `tensora` restart with the same sandbox keeps notebooks running.

### Run a model endpoint on your Mac

With the same local-account setup on macOS 14+ and Apple Silicon, launch a **Model endpoint**, choose
the Apple Silicon class your Mac supports, and supply a safetensors model such as
[`hf://mlx-community/Qwen2.5-0.5B-Instruct-4bit`](https://huggingface.co/mlx-community/Qwen2.5-0.5B-Instruct-4bit). The first launch installs a separate pinned MLX
environment. Its timeline shows downloading, validation, and loading. A model must use an MLX-supported
architecture and code-free safetensors; custom model code, pickle, and GGUF are refused. The model's
files must fit the workload's memory allocation, and runtime overhead also needs room.

Once it is running, **API connection** on the workload page shows the OpenAI-compatible base URL.
Alternatively, request it with `POST /v1/workloads/<id>/access`. Create an API token on **API tokens**
and keep it in `TENSORA_TOKEN`. Use the endpoint URL as `BASE_URL`:

```bash
curl -s "$BASE_URL/models" -H "Authorization: Bearer $TENSORA_TOKEN"
curl -s "$BASE_URL/chat/completions" -H "Authorization: Bearer $TENSORA_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"model":"hf://mlx-community/Qwen2.5-0.5B-Instruct-4bit","messages":[{"role":"user","content":"Hello"}],"max_tokens":64}'
```

Use the exact model ID returned by `/models`. `stream: true` enables SSE; `stream_options:
{"include_usage": true}` includes token counts. Supported sampling options are `max_tokens`,
`temperature`, and `top_p`. The endpoint serves text requests for this single model; tools, images,
alternate models, and adapters are refused. It serializes generation and returns 503 while busy.
The API accepts up to 1 MiB per request, 8192 prompt-plus-output tokens, and 4096 output tokens.

For private model downloads, start `tensora` with `HF_TOKEN` or the standard explicit AWS credential
environment variables (and `AWS_ENDPOINT_URL_S3` for another S3-compatible store). Credentials never
go in `model_uri` or the endpoint URL. They are removed before the model-serving process starts,
which loads the validated local snapshot offline. Snapshots stay private to each workload; stopping
or deleting an endpoint removes its model data. Existing API-token expiry, revocation, and account
disablement apply to every inference request. Response headers identify region, actual compute class,
and whether a fallback was used. This access path is local to the control plane's machine; remote
relaying and endpoint-scoped keys are Phase 2.
