from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any

import pytest

from einsora_ml import vllm_server
from einsora_ml.ingest import ModelRejectedError
from einsora_ml.validate import Report


def test_vllm_command_is_fixed_to_validated_local_safetensors() -> None:
    args = vllm_server.server_command(Path("/work/model"), "hf://org/model", "/run/own.sock", 2)
    assert args[:3] == [sys.executable, "-m", "vllm.entrypoints.openai.api_server"]
    assert args[args.index("--model") + 1] == "/work/model"
    assert args[args.index("--tokenizer") + 1] == "/work/model"
    assert args[args.index("--load-format") + 1] == "safetensors"
    assert args[args.index("--served-model-name") + 1] == "hf://org/model"
    assert args[args.index("--uds") + 1] == "/run/own.sock"
    assert args[args.index("--tensor-parallel-size") + 1] == "2"
    assert args[args.index("--cpu-offload-gb") + 1] == "0"
    assert "--trust-remote-code" not in args
    assert "--api-key" not in args
    with pytest.raises(ValueError, match="NVIDIA"):
        vllm_server.server_command(Path("/work/model"), "hf://org/model", "/run/own.sock", 0)


def arguments(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    model = tmp_path / "model"
    model.mkdir()
    (model / "config.json").write_text('{"max_position_embeddings":2048}', encoding="utf-8")
    monkeypatch.setattr(
        sys,
        "argv",
        [
            "worker",
            "--source",
            "hf://org/model",
            "--model-dir",
            str(model),
            "--socket",
            str(tmp_path / "model.sock"),
            "--status",
            str(tmp_path / "status.json"),
            "--max-bytes",
            "1048576",
            "--devices",
            "2",
        ],
    )
    return model


def test_fetch_credentials_are_removed_before_vllm_import(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    model = arguments(tmp_path, monkeypatch)
    monkeypatch.setenv("HF_TOKEN", "test-fetch-key")
    monkeypatch.setenv("AWS_SECRET_ACCESS_KEY", "test-fetch-credential")
    monkeypatch.setenv("EINSORA_NATIVE_TOKEN", "internal-worker-key")
    calls: list[str] = []

    def fetch(source: str, dest: Path, limit: int, **kwargs: Any) -> dict[str, Any]:
        assert source == "hf://org/model" and dest == model and limit == 1048576
        calls.append("validated")
        return {}

    def execute(executable: str, args: list[str], env: dict[str, str]) -> None:
        assert calls == ["validated"]
        assert not any(key.startswith("AWS_") for key in env)
        assert "HF_TOKEN" not in env
        assert "EINSORA_NATIVE_TOKEN" not in env
        assert env["VLLM_API_KEY"] == "internal-worker-key"
        assert env["HF_HUB_OFFLINE"] == "1" and env["TRANSFORMERS_OFFLINE"] == "1"
        assert env["VLLM_ALLOW_RUNTIME_LORA_UPDATING"] == "0"
        assert args[args.index("--max-model-len") + 1] == "2048"
        raise SystemExit(0)

    monkeypatch.setattr(vllm_server, "fetch_model", fetch)
    monkeypatch.setattr(vllm_server.os, "execvpe", execute)
    with pytest.raises(SystemExit):
        vllm_server.main()


def test_rejected_model_never_starts_vllm(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    model = arguments(tmp_path, monkeypatch)

    def rejected(*args: Any, **kwargs: Any) -> dict[str, Any]:
        raise ModelRejectedError(Report(model, problems=["custom model code is forbidden"]))

    def must_not_run(*args: Any, **kwargs: Any) -> None:
        pytest.fail("unvalidated weights reached the model server")

    monkeypatch.setattr(vllm_server, "fetch_model", rejected)
    monkeypatch.setattr(vllm_server.os, "execvpe", must_not_run)
    assert vllm_server.main() == 1
    assert json.loads((tmp_path / "status.json").read_text())["code"] == "model-rejected"
