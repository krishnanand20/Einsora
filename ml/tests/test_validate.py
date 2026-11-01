from __future__ import annotations

import json
import struct
from pathlib import Path

import pytest

from einsora_ml.validate import validate_model_dir


def write_safetensors(path: Path, *, data_len: int = 8, offsets: tuple[int, int] = (0, 8)) -> None:
    header = json.dumps(
        {
            "__metadata__": {"format": "pt"},
            "w": {"dtype": "F32", "shape": [2], "data_offsets": list(offsets)},
        }
    ).encode()
    path.write_bytes(struct.pack("<Q", len(header)) + header + b"\0" * data_len)


@pytest.fixture
def model(tmp_path: Path) -> Path:
    (tmp_path / "config.json").write_text(json.dumps({"model_type": "llama"}))
    (tmp_path / "tokenizer.json").write_text("{}")
    (tmp_path / "tokenizer_config.json").write_text(json.dumps({"chat_template": "{{ x }}"}))
    write_safetensors(tmp_path / "model.safetensors")
    return tmp_path


def test_clean_model_passes(model: Path) -> None:
    report = validate_model_dir(model)
    assert report.ok, report.problems
    assert report.files == [
        "config.json",
        "model.safetensors",
        "tokenizer.json",
        "tokenizer_config.json",
    ]


@pytest.mark.parametrize(
    ("name", "expected"),
    [
        ("pytorch_model.bin", "pickle"),
        ("weights.pt", "pickle"),
        ("optimizer.pkl", "pickle"),
        ("modeling_custom.py", "custom model code"),
        ("lib.so", "native"),
        ("weights.gguf", "not allowed"),
        ("run.sh", "not allowed"),
    ],
)
def test_dangerous_files_are_rejected(model: Path, name: str, expected: str) -> None:
    (model / name).write_bytes(b"x")
    report = validate_model_dir(model)
    assert not report.ok
    assert any(name in p and expected in p for p in report.problems), report.problems
    assert name not in report.files


@pytest.mark.parametrize("config", ["config.json", "tokenizer_config.json"])
def test_auto_map_is_rejected(model: Path, config: str) -> None:
    (model / config).write_text(json.dumps({"auto_map": {"AutoModel": "modeling_x.Model"}}))
    report = validate_model_dir(model)
    assert any("auto_map" in p for p in report.problems)


def test_trust_remote_code_flag_is_rejected(model: Path) -> None:
    (model / "config.json").write_text(json.dumps({"trust_remote_code": True}))
    assert any("trust_remote_code" in p for p in validate_model_dir(model).problems)


def test_malformed_safetensors_is_rejected(model: Path) -> None:
    write_safetensors(model / "bad.safetensors", data_len=4, offsets=(0, 8))
    (model / "garbage.safetensors").write_bytes(struct.pack("<Q", 10**12) + b"{}")
    report = validate_model_dir(model)
    assert any("bad.safetensors" in p and "offsets" in p for p in report.problems)
    assert any("garbage.safetensors" in p and "header length" in p for p in report.problems)


def test_symlinks_are_rejected(model: Path, tmp_path_factory: pytest.TempPathFactory) -> None:
    outside = tmp_path_factory.mktemp("outside") / "secret.json"
    outside.write_text("{}")
    (model / "link.json").symlink_to(outside)
    assert any("symlink" in p for p in validate_model_dir(model).problems)


def test_requires_weights_and_config(tmp_path: Path) -> None:
    (tmp_path / "README.md").write_text("hi")
    problems = validate_model_dir(tmp_path).problems
    assert any("config.json" in p for p in problems)
    assert any("no .safetensors" in p for p in problems)


def test_size_limit(model: Path) -> None:
    assert any("limit" in p for p in validate_model_dir(model, max_total_bytes=10).problems)


def test_download_cache_is_ignored(model: Path) -> None:
    cache = model / ".cache" / "huggingface"
    cache.mkdir(parents=True)
    (cache / "evil.py").write_text("import os")
    report = validate_model_dir(model)
    assert report.ok, report.problems
    assert not any(f.startswith(".cache") for f in report.files)
