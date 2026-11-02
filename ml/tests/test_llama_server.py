from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any

import pytest

from einsora_ml import llama_server
from einsora_ml.ingest import ModelSourceError

SERVER = Path("/env/llama/llama-server")
MODEL = Path("/work/model/model-Q4_K_M.gguf")


def command(accelerator: str = "apple", devices: int = 1, **kwargs: Any) -> list[str]:
    args: dict[str, Any] = {
        "server": SERVER,
        "model_file": MODEL,
        "source": "hf://org/model-GGUF:Q4_K_M",
        "socket": "/run/einsora/server.sock",
        "accelerator": accelerator,
        "devices": devices,
        "threads": 6,
    }
    return llama_server.server_command(**{**args, **kwargs})


def flag(args: list[str], name: str) -> str:
    return args[args.index(name) + 1]


def test_command_serves_one_validated_file_on_a_private_socket() -> None:
    args = command()
    assert args[0] == str(SERVER)
    assert flag(args, "--model") == str(MODEL)
    assert flag(args, "--alias") == "hf://org/model-GGUF:Q4_K_M"
    assert flag(args, "--host") == "/run/einsora/server.sock"
    assert flag(args, "--ctx-size") == "8192"
    assert flag(args, "--n-predict") == "4096"
    assert flag(args, "--threads") == flag(args, "--threads-batch") == "6"
    # No fitting: a model that doesn't fit fails rather than running partly on the CPU.
    assert flag(args, "--fit") == "off"
    assert flag(args, "--cache-ram") == "0"  # no host-memory prompt cache beyond the allocation
    for off in ("--no-ui", "--no-slots", "--offline"):
        assert off in args
    # Nothing that loads more models or code, opens ports, or talks to other servers.
    for absent in (
        "--port",
        "--api-key",
        "--hf-repo",
        "--models-dir",
        "--lora",
        "--mmproj",
        "--tools",
        "--mcp-servers-config",
        "--connect",
        "--slot-save-path",
        "--metrics",
        "--props",
    ):
        assert absent not in args


@pytest.mark.parametrize(
    ("accelerator", "devices", "device", "layers"),
    [
        ("apple", 1, "MTL0", "all"),
        ("nvidia", 1, "CUDA0", "all"),
        ("nvidia", 3, "CUDA0,CUDA1,CUDA2", "all"),
        ("cpu", 0, "none", "0"),
    ],
)
def test_layers_go_only_on_the_allocation(
    accelerator: str, devices: int, device: str, layers: str
) -> None:
    args = command(accelerator, devices)
    assert (flag(args, "--device"), flag(args, "--n-gpu-layers")) == (device, layers)


@pytest.mark.parametrize(
    ("accelerator", "devices"), [("apple", 0), ("apple", 2), ("nvidia", 0), ("cpu", 1), ("amd", 1)]
)
def test_allocation_must_be_explicit(accelerator: str, devices: int) -> None:
    with pytest.raises(ValueError, match="Apple GPU"):
        command(accelerator, devices)


@pytest.mark.parametrize(
    ("change", "error"),
    [
        ({"socket": "127.0.0.1"}, "Unix socket"),
        ({"socket": "relative.sock"}, "Unix socket"),
        ({"socket": "/run/einsora/server"}, "Unix socket"),
        ({"model_file": Path("model.gguf")}, "absolute"),
        ({"server": Path("llama-server")}, "absolute"),
        ({"threads": 0}, "thread"),
    ],
)
def test_command_refuses_unsafe_values(change: dict[str, Any], error: str) -> None:
    with pytest.raises(ValueError, match=error):
        command(**change)


def test_model_names_are_never_split() -> None:
    with pytest.raises(ModelSourceError, match="comma"):
        command(source="s3://bucket/a,b")


def worker(tmp_path: Path, monkeypatch: pytest.MonkeyPatch, accelerator: str = "cpu") -> Path:
    monkeypatch.setattr(
        sys,
        "argv",
        [
            "worker",
            "--source",
            "hf://org/model-GGUF:Q4_K_M",
            "--model-dir",
            str(tmp_path / "model"),
            "--socket",
            str(tmp_path / "server.sock"),
            "--status",
            str(tmp_path / "status.json"),
            "--max-bytes",
            "1048576",
            "--server",
            str(SERVER),
            "--accelerator",
            accelerator,
            "--devices",
            "0",
            "--threads",
            "4",
        ],
    )
    return tmp_path / "status.json"


def test_server_starts_without_credentials_or_llama_settings(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    status = worker(tmp_path, monkeypatch)
    fetched: dict[str, Any] = {}

    def fetch(*args: Any, **kwargs: Any) -> dict[str, Any]:
        fetched.update(kwargs)
        return {"model_file": "Q4_K_M/model-Q4_K_M.gguf"}

    def execve(path: str, args: list[str], env: dict[str, str]) -> None:
        raise SystemExit((path, args, env))

    monkeypatch.setattr(llama_server, "fetch_model", fetch)
    monkeypatch.setattr(llama_server.os, "execve", execve)
    for key, value in {
        "EINSORA_NATIVE_TOKEN": "internal-token",
        "HF_TOKEN": "not-a-real-token",
        "AWS_SECRET_ACCESS_KEY": "not-a-real-credential",
        "LLAMA_ARG_HOST": "127.0.0.1",
        "AIP_MODE": "PREDICTION",
        "GGML_CUDA_ENABLE_UNIFIED_MEMORY": "1",
        "LD_LIBRARY_PATH": "/usr/local/cuda/lib64",
    }.items():
        monkeypatch.setenv(key, value)
    with pytest.raises(SystemExit) as exited:
        llama_server.main()
    path, args, env = exited.value.code  # type: ignore[misc]
    assert fetched["model_format"] == "gguf"
    assert path == str(SERVER)
    assert flag(args, "--model") == str(tmp_path / "model" / "Q4_K_M" / "model-Q4_K_M.gguf")
    assert env["LLAMA_API_KEY"] == "internal-token"
    assert env["LD_LIBRARY_PATH"] == "/usr/local/cuda/lib64"  # CUDA images need it
    for removed in (
        "EINSORA_NATIVE_TOKEN",
        "HF_TOKEN",
        "AWS_SECRET_ACCESS_KEY",
        "LLAMA_ARG_HOST",
        "AIP_MODE",
        "GGML_CUDA_ENABLE_UNIFIED_MEMORY",
    ):
        assert removed not in env
    assert json.loads(status.read_text()) == {"stage": "loading", "code": ""}


def test_source_problems_have_their_own_code(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    status = worker(tmp_path, monkeypatch)

    def fetch(*args: Any, **kwargs: Any) -> dict[str, Any]:
        raise ModelSourceError("no GGUF model in hf://org/model-GGUF matches Q4_K_M")

    monkeypatch.setattr(llama_server, "fetch_model", fetch)
    assert llama_server.main() == 1
    assert json.loads(status.read_text()) == {"stage": "failed", "code": "model-not-found"}
    assert "matches Q4_K_M" in capsys.readouterr().err


def test_bad_allocation_fails_before_any_download(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    status = worker(tmp_path, monkeypatch, accelerator="apple")  # with 0 devices
    monkeypatch.setattr(llama_server, "fetch_model", lambda *a, **k: pytest.fail("fetched"))
    assert llama_server.main() == 1
    assert json.loads(status.read_text())["code"] == "driver-problem"
