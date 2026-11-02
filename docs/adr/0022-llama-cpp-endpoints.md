# 0022. Serve GGUF models with a pinned llama.cpp on node agents

- **Status:** Proposed (roadmap step 10b)
- **Date:** 2026-11-02
- **Deciders:** Krishnanand
- **Builds on:** ADR-0010 (no silent fallbacks), ADR-0013 (GGUF and llama.cpp), ADR-0017 (native
  drivers), ADR-0018 (Docker driver), ADR-0019 (driver-specific runtimes)

## Context

ADR-0013 chose llama.cpp for GGUF models: endpoints on a Mac's GPU or CPU, and CPU endpoints on
Linux. `einsora-ml validate --format gguf` now checks a GGUF model's header and tensor layout
(step 10a), but no driver ran llama.cpp, so the catalog refused GGUF. Three things had to be
decided:

- **Where llama.cpp comes from.** It's native code that runs a user's model. It must be the same
  build everywhere, checked before use, and never what happens to be on the machine.
- **How a user names a GGUF model.** A GGUF repository on Hugging Face usually holds a dozen
  quantizations of one model, and sometimes a multimodal projector (`mmproj`) as well. The
  validator accepts exactly one model per directory.
- **How it's chosen.** The catalog picks a runtime by accelerator, so one image can't offer MLX
  and llama.cpp on the same Mac. Whether a model is safetensors or GGUF is the user's choice, and
  the format decides the server.

llama-server, llama.cpp's server, has a much larger surface than the endpoints need: a web UI,
slot save and restore, a router that loads other models, MCP tools, a CORS proxy, and settings
read from `LLAMA_ARG_*` environment variables. By default it also fits a model to free memory,
moving layers to the CPU when the GPU is short. That would be a silent CPU fallback (rule 10).

## Decision

**One pinned release, v0.5.0 (build b11146), on every platform.**

- **Macs:** the driver downloads the release's macOS arm64 archive from GitHub. It checks the
  archive's SHA-256 against the value pinned in `macosnative/llamacpp.go` before using anything
  in it. It unpacks only regular files, and links to files beside them, into the runtime's
  environment. The environment's name includes the pin, so a new release builds a new
  environment, and running endpoints ask to be restarted (the sandbox fingerprint changes).
- **Linux (Docker):** `images/containers/llama-endpoint` builds on llama.cpp's own
  `server-v0.5.0` and `server-cuda-v0.5.0` images, pinned by digest. The catalog names our image
  by version, as it does the vLLM image, until releases publish images and pin them by digest
  (roadmap step 11). It adds Ubuntu's Python 3.12
  and the worker's dependencies, installed from wheels with hashes.

**A separate catalog image per format.**

- `openai-endpoint` serves safetensors with vLLM on NVIDIA and with MLX on Apple Silicon.
- `gguf-endpoint` serves GGUF with llama.cpp: `docker` on NVIDIA and CPU, and `macos-native` on
  Apple Silicon and CPU.
- The public catalog shows each runtime's `model_format`.
- Kubernetes has no GGUF runtime, so `LaunchRuntime` serves GGUF only with the `docker`,
  `macos-native` and `sim` drivers, and the Datacenter catalog doesn't offer it.

**A GGUF source names one model.**

- `hf://org/repo:TAG` selects the model whose file name carries the quantization tag, ignoring
  case: `Q4_K_M` in `model-Q4_K_M.gguf`, or a `Q4_K_M/` folder. A split model is all of its
  parts. With no tag, the repository's only model is used.
- Multimodal projectors aren't models. A tag that matches no model, or several, fails with
  `model-not-found`, whose reason says to check the quantization tag. Nothing is guessed.
- An S3 prefix must hold one model, as `einsora-ml ingest --format gguf` writes it.
- Only the selected files are downloaded, after their sizes are checked.

**One worker, `einsora_ml.llama_server`, on both drivers.**

- It fetches the model into the workload's own storage and validates it with `--format gguf`.
- It then execs llama-server with the validated file's absolute path and fixed flags:
  - a Unix socket path ending in `.sock` (llama-server binds anything else as TCP);
  - an 8192-token context shared by 4 slots, and at most 4096 tokens per response, as for the
    other endpoints, and no prompt cache in host memory (`--cache-ram 0`; it's 8 GiB by default,
    outside the allocation);
  - `--device` naming exactly the allocated devices (`MTL0`, `CUDA0..n-1`, or `none` for CPU),
    every layer on them, and `--fit off`, so a model that doesn't fit fails instead of spilling
    onto the CPU;
  - Jinja chat templates;
  - `--no-ui`, `--no-slots`, `--offline`.
- No router, tools, MCP, LoRA, projector, metrics, or slot saving is ever enabled.
- Its token goes in as `LLAMA_API_KEY`. The worker removes `HF_*`, `AWS_*`, `LLAMA_*`, `AIP_*`
  and `GGML_*` variables before the exec; `GGML_CUDA_ENABLE_UNIFIED_MEMORY`, for one, would let GPU
  memory spill into system memory.
- The access proxy still forwards only `/v1/models` and the two completion routes.

**Native CPU endpoints reserve the Mac's GPU**, as native CPU notebooks do (ADR-0019). llama.cpp is
started with `--device none`, but the sandbox can't revoke Metal.

## Consequences

- CPU endpoints work, on Macs and on Linux. Mac owners can choose MLX or llama.cpp by choosing the
  image.
- The first GGUF endpoint on a Mac downloads about 11 MB of llama.cpp, plus the worker's Python
  packages. A Docker host builds the image once per variant; the CUDA base is several GB.
- Updating llama.cpp is a reviewed change: a new pin, a new digest, and new hashes. Security fixes
  wait for that change.
- A chat template inside a GGUF file is data that llama.cpp's own Jinja engine interprets. The
  validator doesn't examine it (SECURITY.md). Endpoints remain owner-only on these drivers.
- "Every layer on the GPU" is about layers: like other GPU servers, llama.cpp still does a few small
  operations, such as looking up token embeddings, on the CPU.
- The model-size check covers the weights, not the KV cache. A model that fits but leaves too little
  room for its context fails to load on a GPU, or in Docker meets the container's memory limit; a
  native CPU endpoint has no hard limit on a Mac.
- AMD has no endpoint runtime yet. A ROCm or Vulkan build would be a new pin and a new image.

## Alternatives considered

- **Homebrew's `llama.cpp`, or whatever is on PATH:** not pinned, and it differs between machines.
- **Building llama.cpp from source on each Mac:** needs Xcode and CMake, takes minutes, and runs
  build code outside the sandbox.
- **`llama-cpp-python` wheels:** PyPI has no Metal wheels, and the uv environments install
  wheels only.
- **One endpoint image for both formats, with the format chosen per class:** the same image would
  take a safetensors repository on one class and a GGUF one on another, and Macs couldn't offer
  llama.cpp next to MLX.
- **Let llama.cpp pick the quantization (`-hf org/repo:TAG`):** it downloads at load time, outside
  the validator. With no tag, it picks Q4_K_M, or else the repository's first file.
- **A Python proxy in front of llama-server, like the MLX worker:** another hop for every token.
  The access proxy already restricts routes, and llama-server's own limits bound requests.
