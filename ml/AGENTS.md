# AGENTS.md: ml/ (Python ML layer)

These rules add to the root [AGENTS.md](../AGENTS.md); read that first.

## What lives here

ML-specific tooling that the Go core platform orchestrates but doesn't implement. It runs on macOS
(Apple Silicon) and Linux.

- **`validate.py` is security-critical.** It decides which model files may be served. It's an allowlist: new file types are rejected unless explicitly added.
  - Never allowed: pickle formats, `.py` files, and `auto_map` or `trust_remote_code` in configs.
  - Today: safetensors only. Planned: GGUF, with header validation (ADR-0013), for llama.cpp on Macs and Linux.
  - Its `total_bytes` will also decide whether a model is small enough for CPU fallback, or fits a Mac's unified memory (ADR-0010, ADR-0013). Keep it accurate.
- **`ingest.py`:** download → validate → store with a checksum manifest. It stores only the files the validator accepted, in the store for the region where the model will be served (ADR-0009). That's an S3 bucket in a datacenter, or the machine's model cache in Solo and Lab.
- **`bench.py`:** load-tests any OpenAI-compatible endpoint (vLLM, llama.cpp, MLX servers): TTFT, latency, throughput.
- **`burnin.py`:** GPU acceptance tests for new machines. Needs the `burnin` extra (torch). CUDA today; Metal (MPS) planned.

## Rules

- **Changes to `validate.py`** need a test that proves the new behavior, plus review from someone who knows the trust model (see CONTRIBUTING.md). Never loosen the allowlist to make one model work. Ask the user to convert the model instead.
- **Heavy dependencies** (`torch`, `mlx`, `transformers`) are optional extras, imported inside functions. That keeps `validate`, `ingest`, and `bench` light and testable without a GPU.
- **Never load model weights** in the validator. Inspect headers and metadata only. Loading is exactly what an attacker wants us to do.
- **Credentials** come from the environment (`AWS_*`, `HF_TOKEN`, `TENSORAFI_BENCH_API_KEY`). Never pass them as CLI arguments, because those show up in process lists and job specs.
- **Output:** JSON on stdout, logs on stderr, and a failed check exits with code 1. The control plane and agents parse this.
- Python 3.12, full type hints, `mypy --strict` must pass, and code is formatted with ruff.

## Commands

```bash
uv sync
uv run pytest
uv run ruff check . && uv run ruff format --check . && uv run mypy
```
