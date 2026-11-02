"""Validate one GGUF model in the workload's private storage, then exec a pinned llama.cpp server
offline on a Unix socket: natively on Macs (Metal or CPU), and in the Docker endpoint image (NVIDIA
or CPU). The access proxy forwards only /v1/models and the two completion routes to it.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path

from einsora_ml.ingest import ModelSourceError
from einsora_ml.local import failure, fetch_model

#: The same bounds as Einsora's other endpoints: an 8192-token context shared by four requests at
#: a time, and at most 4096 generated tokens per request.
CONTEXT_TOKENS = 8192
MAX_OUTPUT_TOKENS = 4096
PARALLEL = 4
ACCELERATORS = ("apple", "nvidia", "cpu")
#: What the worker removes from llama-server's environment: model-download credentials, and the
#: variables llama.cpp reads as settings: LLAMA_ARG_*, Google Cloud's AIP_* serving mode, and
#: ggml's backend switches, such as GGML_CUDA_ENABLE_UNIFIED_MEMORY, which would let a model that
#: doesn't fit in GPU memory spill into system memory.
_REMOVED_ENV = ("HF_", "AWS_", "LLAMA_", "AIP_", "GGML_")


def offload(accelerator: str, devices: int) -> tuple[str, str]:
    """llama.cpp's --device and --n-gpu-layers for an allocation. Every layer goes on exactly the
    devices allocated, or none: without memory fitting, a model that doesn't fit fails to load
    rather than running partly on the CPU (rule 10)."""
    if accelerator == "apple" and devices == 1:
        return "MTL0", "all"
    if accelerator == "nvidia" and 1 <= devices <= 64:
        # The container sees only its allocated GPUs, numbered from 0.
        return ",".join(f"CUDA{i}" for i in range(devices)), "all"
    if accelerator == "cpu" and devices == 0:
        return "none", "0"
    raise ValueError(
        "llama.cpp needs the whole Apple GPU, explicit NVIDIA GPUs, or a CPU allocation"
    )


def server_command(
    server: Path,
    model_file: Path,
    source: str,
    socket: str,
    accelerator: str,
    devices: int,
    threads: int,
) -> list[str]:
    if not server.is_absolute() or not model_file.is_absolute():
        raise ValueError("llama-server and its model need absolute paths")
    if not socket.startswith("/") or not socket.endswith(".sock"):
        # llama-server reads any other --host as a TCP address.
        raise ValueError(
            "llama-server listens on a Unix socket only at an absolute path ending in .sock"
        )
    if "," in source:
        raise ModelSourceError("llama.cpp splits model names at commas; use a source without one")
    if not 1 <= threads <= 4096:
        raise ValueError("thread count must be between 1 and 4096")
    device, layers = offload(accelerator, devices)
    return [
        str(server),
        "--model",
        str(model_file),
        "--alias",
        source,
        "--host",
        socket,
        "--ctx-size",
        str(CONTEXT_TOKENS),
        "--n-predict",
        str(MAX_OUTPUT_TOKENS),
        "--parallel",
        str(PARALLEL),
        "--kv-unified",
        # No prompt cache in host memory (8 GiB by default), which the allocation doesn't cover.
        "--cache-ram",
        "0",
        "--threads",
        str(threads),
        "--threads-batch",
        str(threads),
        "--device",
        device,
        "--n-gpu-layers",
        layers,
        "--fit",
        "off",
        "--jinja",
        "--no-ui",
        "--no-slots",
        "--offline",
    ]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", required=True)
    parser.add_argument("--model-dir", required=True, type=Path)
    parser.add_argument("--socket", required=True)
    parser.add_argument("--status", required=True, type=Path)
    parser.add_argument("--max-bytes", required=True, type=int)
    parser.add_argument("--server", required=True, type=Path)
    parser.add_argument("--accelerator", required=True, choices=ACCELERATORS)
    parser.add_argument("--devices", required=True, type=int)
    parser.add_argument("--threads", required=True, type=int)
    args = parser.parse_args()
    os.umask(0o077)  # llama-server's socket inherits it

    def stage(name: str, code: str = "") -> None:
        temporary = args.status.with_suffix(".tmp")
        temporary.write_text(json.dumps({"stage": name, "code": code}), encoding="utf-8")
        temporary.replace(args.status)

    phase = "starting"
    try:
        offload(args.accelerator, args.devices)  # before downloading anything
        phase = "fetching"
        manifest = fetch_model(
            args.source, args.model_dir, args.max_bytes, model_format="gguf", stage=stage
        )
        # The validated file, by an absolute path: never a name a server could read as a flag.
        model_file = args.model_dir.absolute() / str(manifest["model_file"])
        command = server_command(
            args.server,
            model_file,
            args.source,
            args.socket,
            args.accelerator,
            args.devices,
            args.threads,
        )
        phase = "loading"
        stage(phase)
        environ = {k: v for k, v in os.environ.items() if not k.startswith(_REMOVED_ENV)}
        environ["LLAMA_API_KEY"] = environ.pop("EINSORA_NATIVE_TOKEN")
        os.execve(command[0], command, environ)  # noqa: S606 - pinned server and fixed arguments
    except Exception as exc:
        code, detail = failure(exc, phase)
        stage("failed", code)
        print(json.dumps(detail), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
