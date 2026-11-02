"""Validate one container-local model, then exec a pinned vLLM server offline on a Unix socket."""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path

from einsora_ml.local import failure, fetch_model


def server_command(
    model: Path, source: str, socket: str, devices: int, context_tokens: int = 8192
) -> list[str]:
    if not 1 <= devices <= 64:
        raise ValueError("vLLM requires an explicit NVIDIA allocation")
    return [
        sys.executable,
        "-m",
        "vllm.entrypoints.openai.api_server",
        "--model",
        str(model),
        "--tokenizer",
        str(model),
        "--served-model-name",
        source,
        "--load-format",
        "safetensors",
        "--generation-config",
        "vllm",
        "--uds",
        socket,
        "--tensor-parallel-size",
        str(devices),
        "--max-model-len",
        str(context_tokens),
        "--max-num-seqs",
        "16",
        "--swap-space",
        "0",
        "--cpu-offload-gb",
        "0",
        "--disable-log-requests",
        "--disable-fastapi-docs",
        "--enable-request-id-headers",
    ]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", required=True)
    parser.add_argument("--model-dir", required=True, type=Path)
    parser.add_argument("--socket", required=True)
    parser.add_argument("--status", required=True, type=Path)
    parser.add_argument("--max-bytes", required=True, type=int)
    parser.add_argument("--devices", required=True, type=int)
    args = parser.parse_args()
    os.umask(0o077)

    def stage(name: str, code: str = "") -> None:
        temporary = args.status.with_suffix(".tmp")
        temporary.write_text(json.dumps({"stage": name, "code": code}), encoding="utf-8")
        temporary.replace(args.status)

    phase = "fetching"
    try:
        if not 1 <= args.devices <= 64:
            raise ValueError("vLLM requires an explicit NVIDIA allocation")
        fetch_model(args.source, args.model_dir, args.max_bytes, stage=stage)
        config = json.loads((args.model_dir / "config.json").read_text(encoding="utf-8"))
        limits = [8192]
        for key in ("max_position_embeddings", "n_positions"):
            value = config.get(key)
            if type(value) is int and value > 0:
                limits.append(value)
        command = server_command(
            args.model_dir, args.source, args.socket, args.devices, min(limits)
        )
        phase = "loading"
        stage(phase)
        environ = {
            key: value for key, value in os.environ.items() if not key.startswith(("HF_", "AWS_"))
        }
        environ.update(
            HF_HUB_OFFLINE="1", TRANSFORMERS_OFFLINE="1", VLLM_ALLOW_RUNTIME_LORA_UPDATING="0"
        )
        environ["VLLM_API_KEY"] = environ.pop("EINSORA_NATIVE_TOKEN")
        # A fresh process image establishes offline mode before Hub/Transformers are imported.
        os.execvpe(sys.executable, command, environ)  # noqa: S606 - pinned interpreter and fixed module
    except Exception as exc:
        code, detail = failure(exc, phase)
        stage("failed", code)
        print(json.dumps(detail), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
