# ml: Einsora ML layer (Python)

The ML-specific work behind the platform, shipped as the `einsora-ml` CLI. It runs on macOS
(Apple Silicon) and Linux, and is used by agents and by datacenter jobs.

| Command | What it does | Where it runs |
|---|---|---|
| `einsora-ml validate <dir>` | Enforces the model policy: code-free formats only (safetensors today; GGUF planned), no pickle, no custom code, sane headers, a size limit | Anywhere |
| `einsora-ml ingest hf://org/model[@rev] s3://bucket/prefix` | Downloads only allowed file types, validates them, then stores them with a sha256 manifest | A laptop today; agents and datacenter jobs later |
| `einsora-ml bench <url> --model <name>` | Load-tests any OpenAI-compatible endpoint (vLLM, llama.cpp, MLX) and reports TTFT p50/p95 and tokens/s | Anywhere with network access |
| `einsora-ml burnin` | Sustained bf16 matmul, NaN checks, and a memory pattern test on every visible GPU | NVIDIA GPUs today; Apple GPUs (MPS) planned |

All output is JSON. A failed check exits with code 1.

The native macOS driver also runs `einsora_ml.native`: a single validated MLX model on a private Unix
socket. `local.py` fetches Hugging Face or S3 files into that workload's own storage, validates them,
and publishes a checksum manifest. The worker restarts offline without fetch credentials before
loading; it cannot select another model or enable remote code through an API request. MLX and its
tokenizer dependencies are the optional `serve` extra and import only inside the serving path.

The Go binary embeds these Python sources so it uses the same validator without needing a repository
checkout at runtime. After changing `ml/src/einsora_ml/*.py`, regenerate the deterministic bundle:

```bash
python3 ml/scripts/bundle_native.py   # from the repository root
```

On Apple Silicon, test a real sandboxed endpoint with a tiny randomly generated safetensors model
and a local S3 fixture (no model downloads):

```bash
cd ml
uv sync --extra serve
cd ../controlplane
EINSORA_TEST_NATIVE_ENDPOINT_ENV="$PWD/../ml/.venv" \
  go test ./internal/agent/drivers/macosnative -run TestRealEndpoint -count=1 -v
```

## Develop

```bash
uv sync                  # add --extra burnin on a GPU machine
uv run pytest
uv run ruff check . && uv run mypy
uv run einsora-ml validate ./some-model
uv run einsora-ml bench http://127.0.0.1:8080 --model my-model   # e.g. a local llama.cpp server
```

## Ingest a model into a datacenter object store

```bash
export AWS_ENDPOINT_URL=https://s3.einsora.internal AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
uv run einsora-ml ingest hf://Qwen/Qwen2.5-0.5B-Instruct@<commit-sha> s3://einsora-models/internal/qwen2.5-0.5b
```

Pin a commit SHA, because branch names like `main` can change under you. Store models in the region where
they'll be served, because model data never leaves its region (ADR-0009).
