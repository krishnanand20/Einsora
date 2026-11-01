from __future__ import annotations

import io
import json
import struct
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest

from tensora_ml.ingest import ModelRejectedError, sha256_file
from tensora_ml.local import _s3, fetch_model, safe_name


def model_files() -> dict[str, bytes]:
    header = json.dumps({"w": {"dtype": "F32", "shape": [1], "data_offsets": [0, 4]}}).encode()
    return {
        "config.json": b'{"model_type":"llama"}',
        "model.safetensors": struct.pack("<Q", len(header)) + header + b"\0" * 4,
        "tokenizer.json": b"{}",
    }


@pytest.fixture
def hf(monkeypatch: pytest.MonkeyPatch) -> tuple[dict[str, bytes], list[str]]:
    files = model_files()
    files["modeling_unsafe.py"] = b"this must never be downloaded"
    revisions: list[str] = []

    def info(*args: Any, **kwargs: Any) -> SimpleNamespace:
        return SimpleNamespace(
            sha="a" * 40,
            siblings=[SimpleNamespace(rfilename=n, size=len(b)) for n, b in files.items()],
        )

    def download(repo: str, name: str, *, revision: str, local_dir: Path) -> str:
        assert not name.endswith(".py")
        revisions.append(revision)
        path = local_dir / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(files[name])
        (local_dir / ".cache").mkdir(exist_ok=True)
        (local_dir / ".cache" / "private-metadata").write_text("not served")
        return str(path)

    monkeypatch.setattr("huggingface_hub.HfApi", lambda: SimpleNamespace(model_info=info))
    monkeypatch.setattr("huggingface_hub.hf_hub_download", download)
    return files, revisions


def test_local_fetch_is_validated_pinned_and_private(tmp_path: Path, hf: Any) -> None:
    _, revisions = hf
    stages: list[str] = []
    dest = tmp_path / "model"
    manifest = fetch_model("hf://org/model", dest, 4096, stage=stages.append)
    assert stages == ["fetching", "validating"]
    assert revisions == ["a" * 40] * 3
    assert manifest["revision"] == "a" * 40
    assert manifest["files"]["model.safetensors"]["sha256"] == sha256_file(
        dest / "model.safetensors"
    )
    assert sorted(p.name for p in dest.iterdir()) == [
        "config.json",
        "model.safetensors",
        "tensora-manifest.json",
        "tokenizer.json",
    ]
    assert sorted(p.name for p in tmp_path.iterdir()) == ["model"]


def test_rejected_models_are_never_published(tmp_path: Path, hf: Any) -> None:
    files, _ = hf
    files["config.json"] = b'{"auto_map":{"AutoModel":"evil.Model"}}'
    with pytest.raises(ModelRejectedError):
        fetch_model("hf://org/model", tmp_path / "model", 4096)
    assert list(tmp_path.iterdir()) == []


def test_size_limit_precedes_download(tmp_path: Path, hf: Any) -> None:
    _, downloads = hf
    with pytest.raises(ValueError, match="size limit"):
        fetch_model("hf://org/model", tmp_path / "model", 8)
    assert not downloads
    assert list(tmp_path.iterdir()) == []


def test_restart_reuses_only_the_verified_snapshot(tmp_path: Path, hf: Any) -> None:
    _, downloads = hf
    dest = tmp_path / "model"
    first = fetch_model("hf://org/model", dest, 4096)
    assert fetch_model("hf://org/model", dest, 4096) == first
    assert len(downloads) == 3
    (dest / "config.json").write_text('{"model_type":"llama","changed":true}')
    with pytest.raises(ValueError, match="checksum"):
        fetch_model("hf://org/model", dest, 4096)


@pytest.mark.parametrize(
    "name",
    [
        "../escape.json",
        "/absolute.json",
        "a/../x.json",
        "a//b.json",
        "a/./b.json",
        "a\\b.json",
        "a\n.json",
    ],
)
def test_unsafe_model_paths_are_rejected(name: str) -> None:
    with pytest.raises(ValueError, match="unsafe"):
        safe_name(name)


@pytest.mark.parametrize("actual", [b"123", b"12345"])
def test_s3_download_rechecks_object_size(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, actual: bytes
) -> None:
    client = SimpleNamespace(
        get_paginator=lambda _: SimpleNamespace(
            paginate=lambda **_: [{"Contents": [{"Key": "tiny/config.json", "Size": 4}]}]
        ),
        get_object=lambda **_: {"Body": io.BytesIO(actual)},
    )
    monkeypatch.setattr("boto3.client", lambda _: client)
    with pytest.raises(ValueError, match=r"grew|truncated"):
        _s3("s3://models/tiny", tmp_path, 4096)


def test_s3_prefix_cannot_write_outside_model(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    client = SimpleNamespace(
        get_paginator=lambda _: SimpleNamespace(
            paginate=lambda **_: [{"Contents": [{"Key": "tiny/../escape.json", "Size": 4}]}]
        )
    )
    monkeypatch.setattr("boto3.client", lambda _: client)
    with pytest.raises(ValueError, match="unsafe"):
        _s3("s3://models/tiny", tmp_path, 4096)
