# AGENTS.md: ml/ (Python ML layer)

These rules add to the root [AGENTS.md](../AGENTS.md); read that first.

## What lives here

ML-specific tooling that the Go core platform orchestrates but doesn't implement. It runs on macOS
(Apple Silicon) and Linux.

- **`validate.py` is security-critical.** It decides which model files may be served. It's an allowlist: new file types are rejected unless explicitly added.
  - Never allowed: pickle formats, `.py` files, and `auto_map` or `trust_remote_code` in configs.
  - Two formats (ADR-0013), named by the caller (`--format`, as the catalog's `model_format`): safetensors for vLLM and MLX, and GGUF for llama.cpp. Suffixes and config names are matched ignoring case, as macOS file systems match them.
  - A GGUF model is one file, or every part of one split model, with only documentation beside it. Its header framing and tensor layout are checked as llama.cpp's loader checks them, plus our bounds; the tokenizer and chat template inside are data the server interprets, and aren't examined. Tensor types are an allowlist (`_GGML_TYPES`): add a new type only with its block size from ggml, and a test against a real file.
  - Its `total_bytes` will also decide whether a model is small enough for CPU fallback, or fits a Mac's unified memory (ADR-0010, ADR-0013). Keep it accurate.
- **`ingest.py`:** download → validate → store with a checksum manifest. It stores only the files the validator accepted, in the store for the region where the model will be served (ADR-0009). That's an S3 bucket in a datacenter, or the machine's model cache in Solo and Lab. It also parses sources: `hf://org/model[@rev][:TAG]`, where the tag names one GGUF quantization, and `select_gguf` picks that model's files. It never guesses: no match, or several, is a `ModelSourceError` (rule 10).
- **`local.py` and the endpoint workers** (`native.py` for MLX, `vllm_server.py`, `llama_server.py`): fetch into the workload's own storage, validate in the endpoint's format, then serve. A worker reports only fixed status codes (`failure()`), and logs only our own messages or an error's type, never its text, which could carry a credential. Before a server starts, the worker removes download credentials (`HF_*`, `AWS_*`) from its environment; `llama_server.py` also removes llama.cpp's settings (`LLAMA_*`, `AIP_*`, `GGML_*`) and passes every setting as a flag. Never enable llama-server's router, tools, MCP, LoRA, projectors, slots, web UI, host-memory prompt cache, or memory fitting: see ADR-0022.
- **`bench.py`:** load-tests any OpenAI-compatible endpoint (Einsora's, vLLM, llama.cpp, MLX servers): TTFT, latency, throughput. It takes a server's root or `/v1` URL, and finds the model from `/v1/models`.
- **`burnin.py`:** GPU acceptance tests for new machines. Needs the `burnin` extra (torch). CUDA on NVIDIA; Metal (MPS) on Apple Silicon, where the GPU shares memory with macOS, so the memory test covers half of Metal's recommended working set by default.

## Rules

- **Changes to `validate.py`** need a test that proves the new behavior, plus review from someone who knows the trust model (see CONTRIBUTING.md). Never loosen the allowlist to make one model work. Ask the user to convert the model instead.
- **Heavy dependencies** (`torch`, `mlx`, `transformers`) are optional extras, imported inside functions. That keeps `validate`, `ingest`, and `bench` light and testable without a GPU.
- **Never load model weights** in the validator. Inspect headers and metadata only. Loading is exactly what an attacker wants us to do.
- **Credentials** come from the environment (`AWS_*`, `HF_TOKEN`, `EINSORA_BENCH_API_KEY`). Never pass them as CLI arguments, because those show up in process lists and job specs.
- **Output:** JSON on stdout, logs on stderr, and a failed check exits with code 1. The control plane and agents parse this.
- Python 3.12, full type hints, `mypy --strict` must pass, and code is formatted with ruff.
- The `einsora` binary embeds these sources. After changing `src/einsora_ml/*.py`, run `python3 ml/scripts/bundle_native.py` from the repository root; a Go test fails until you do.

## Commands

```bash
uv sync
uv run pytest
uv run ruff check . && uv run ruff format --check . && uv run mypy
```
