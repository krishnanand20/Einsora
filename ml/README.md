# ml: Einsora ML layer (Python)

The ML-specific work behind the platform, shipped as the `einsora-ml` CLI. It runs on macOS
(Apple Silicon) and Linux, and is used by agents and by datacenter jobs.

| Command | What it does | Where it runs |
|---|---|---|
| `einsora-ml validate <dir> [--format gguf]` | Enforces the model policy: code-free formats only (safetensors, or GGUF for llama.cpp), no pickle, no custom code, sane headers, a size limit | Anywhere |
| `einsora-ml ingest hf://org/model[@rev][:TAG] s3://bucket/prefix [--format gguf]` | Downloads only allowed file types (for GGUF, only the one quantization `:TAG` names), checks their sizes first, validates them, then stores them with a sha256 manifest | A laptop today; agents and datacenter jobs later |
| `einsora-ml bench <url>` | Load-tests any OpenAI-compatible endpoint (Einsora's, vLLM, llama.cpp, MLX) and reports TTFT p50/p95 and tokens/s | Anywhere with network access |
| `einsora-ml burnin` | Sustained matmul (bf16 on NVIDIA, fp16 on Apple GPUs), NaN checks, and a memory pattern test on every visible GPU | NVIDIA GPUs (CUDA) and Apple Silicon (Metal) |

All output is JSON. A failed check exits with code 1.

The drivers also run this package's endpoint workers. `local.py` fetches Hugging Face or S3 files
into the workload's own storage, validates them, and publishes a checksum manifest; on a restart it
re-validates that snapshot instead of downloading again. Then:

- `einsora_ml.native` (macOS) serves a single validated MLX model on a private Unix socket. It restarts
  offline without fetch credentials before loading, and can't select another model or enable remote
  code through an API request. MLX and its tokenizer dependencies are the optional `serve` extra and
  import only inside the serving path.
- `einsora_ml.vllm_server` (Docker, NVIDIA) execs a pinned vLLM offline.
- `einsora_ml.llama_server` (macOS, and Docker on NVIDIA or CPU) execs a pinned llama-server
  ([ADR-0022](../docs/adr/0022-llama-cpp-endpoints.md)) with the validated GGUF file's absolute path,
  on the allocated devices only, with memory fitting, the host-memory prompt cache, the web UI, and
  slots off, after removing download credentials and llama.cpp's environment settings.

A GGUF source names one quantization: `hf://Qwen/Qwen3-0.6B-GGUF:Q8_0` takes the model whose file names
carry `Q8_0`, ignoring case, and all of a split model's parts. A repository with only one model needs no
tag, and multimodal projectors (`mmproj`) are never taken. A tag that matches no model, or several, is
refused (status `model-not-found`) with the models the repository has; nothing is guessed. A
quantization tag on a safetensors endpoint is refused too.

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
uv run einsora-ml bench http://127.0.0.1:8081   # e.g. a local llama.cpp server
```

## Validate a model

```bash
uv run einsora-ml validate ./qwen2.5-0.5b               # safetensors, for vLLM and MLX
uv run einsora-ml validate ./qwen3-0.6b --format gguf   # GGUF, for llama.cpp
```

A safetensors model is a Hugging Face directory: `config.json`, `.safetensors` weights, and tokenizer
files. A GGUF model is exactly one model: a single `.gguf` file, or every part of one split model
(`name-00001-of-00003.gguf` and the rest), with only documentation beside it (`README.md`, `LICENSE`).
Its tokenizer and chat template are inside the file, so configs aren't accepted next to it, and a
second quantization or a vision projector (`mmproj`) counts as a second model. The validator reads
only headers. For GGUF it checks the header framing and tensor layout that llama.cpp's loader checks,
with tighter limits, and reports the file a server loads as `model_file`. Tensor types are an
allowlist, so a type newer than the validator is refused until it's added. It doesn't examine the
tokenizer or the chat template inside: those are data the server interprets, the template in its
sandboxed Jinja engine, as with a safetensors model's `tokenizer_config.json`. In either format, file
names may not start with `-` or contain control characters.

## Benchmark an endpoint

`einsora-ml bench` sends `--requests` streamed chat requests (default 64), `--concurrency` at a time
(default 8), and reports time to first token, latency, and output tokens per second. Give it the
server's address or its `/v1` base URL. Without `--model`, it uses the one model the server lists at
`/v1/models`. An API key goes in `EINSORA_BENCH_API_KEY`, never on the command line. Token counts come
from the server's usage report; if a server doesn't send one, streamed chunks are counted instead,
which is about one token each for llama.cpp and MLX.

**An Einsora endpoint** (MLX on a Mac, vLLM on NVIDIA, llama.cpp anywhere): use the base URL from **API connection** on the
workload's page, and an API token from **API tokens**:

```bash
EINSORA_BENCH_API_KEY="$EINSORA_TOKEN" uv run einsora-ml bench "$BASE_URL" --concurrency 1 --requests 16
```

An MLX endpoint on a Mac generates one response at a time and answers 503 while it's busy, so keep
`--concurrency 1` there: more would count as errors, not load. vLLM batches requests, so on NVIDIA
raise `--concurrency` step by step to find where latency starts to climb.

**An Einsora GGUF endpoint** (llama.cpp) runs 4 requests at a time and queues the rest, so benchmark it
at `--concurrency 4` or less.

**A llama.cpp or MLX server you run yourself**, to compare servers on the same machine:

```bash
llama-server -m ./model.gguf --port 8081 --parallel 4     # e.g. after `brew install llama.cpp`
uv run einsora-ml bench http://127.0.0.1:8081 --concurrency 4

uv run --extra serve mlx_lm.server --model mlx-community/Qwen2.5-0.5B-Instruct-4bit --port 8082
uv run einsora-ml bench http://127.0.0.1:8082 --model mlx-community/Qwen2.5-0.5B-Instruct-4bit --concurrency 1
```

llama-server queues requests beyond its `--parallel` slots, so higher concurrency there measures
waiting. `mlx_lm.server` lists the models in your Hugging Face cache, not only the one it serves, so name it.

## Burn in a GPU

Run it on every GPU before anyone else's workloads use it, for at least 30 minutes:

```bash
uv sync --extra burnin    # PyTorch
uv run einsora-ml burnin --seconds 1800
```

It runs sustained matrix multiplications (bf16 on NVIDIA, fp16 on Apple GPUs, which all run fp16
natively), counts results that aren't finite, then writes bit patterns across a share of the GPU's free
memory and reads them back. A Mac's GPU shares memory with macOS and your apps, so there it fills half
of the working set Metal recommends for the GPU; `--memory-fraction` changes the share (at most 0.95,
and 0.85 by default on NVIDIA). Close heavy apps first. A GPU passes with no bad results and every
pattern intact, and the command exits 1 otherwise. Each result names the GPU, backend, dtype,
sustained TFLOPS, and how much memory was tested. On a Datacenter node, follow
[runbook 02](../docs/runbooks/02-gpu-passthrough.md#burn-in-required-before-anyone-else-uses-a-gpu).

## Ingest a model into a datacenter object store

```bash
export AWS_ENDPOINT_URL=https://s3.einsora.internal AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
uv run einsora-ml ingest hf://Qwen/Qwen2.5-0.5B-Instruct@<commit-sha> s3://einsora-models/internal/qwen2.5-0.5b
```

Pin a commit SHA, because branch names like `main` can change under you. Store models in the region where
they'll be served, because model data never leaves its region (ADR-0009).
