from __future__ import annotations

import io
import json
import struct
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest

from einsora_ml.ingest import ModelRejectedError, ModelSourceError, sha256_file
from einsora_ml.local import _s3, failure, fetch_model, safe_name
from einsora_ml.validate import Report
from test_validate import gguf_bytes


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
        "einsora-manifest.json",
        "model.safetensors",
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


@pytest.fixture
def gguf_repo(hf: Any) -> tuple[dict[str, bytes], list[str]]:
    files, revisions = hf
    files.clear()
    files.update(
        {
            "README.md": b"# A GGUF repository",
            "model-Q4_K_M.gguf": gguf_bytes(),
            "model-Q8_0.gguf": gguf_bytes(),
            "mmproj-model-F16.gguf": gguf_bytes(architecture="clip"),
            "config.json": b"{}",
        }
    )
    return files, revisions


def test_gguf_fetch_takes_only_the_named_quantization(tmp_path: Path, gguf_repo: Any) -> None:
    _, downloads = gguf_repo
    dest = tmp_path / "model"
    manifest = fetch_model("hf://org/model-GGUF:q4_k_m", dest, 4096, model_format="gguf")
    assert manifest["model_file"] == "model-Q4_K_M.gguf"
    assert manifest["model_format"] == "gguf"
    assert len(downloads) == 1
    assert sorted(p.name for p in dest.iterdir()) == ["einsora-manifest.json", "model-Q4_K_M.gguf"]
    # A restart re-validates the snapshot, with its manifest beside the model, and downloads
    # nothing; the file a server loads comes from that validation.
    assert fetch_model("hf://org/model-GGUF:q4_k_m", dest, 4096, model_format="gguf") == manifest
    assert len(downloads) == 1
    with pytest.raises(ValueError, match="does not match"):
        fetch_model("hf://org/model-GGUF:q4_k_m", dest, 4096)  # another format


@pytest.mark.parametrize(
    ("source", "model_format", "error"),
    [
        ("hf://org/model-GGUF", "gguf", "has 2 GGUF models"),
        ("hf://org/model-GGUF:Q2_K", "gguf", "matches Q2_K"),
        ("hf://org/model:Q4_K_M", "safetensors", "names a GGUF quantization"),
    ],
)
def test_gguf_source_must_name_one_model(
    tmp_path: Path, gguf_repo: Any, source: str, model_format: str, error: str
) -> None:
    _, downloads = gguf_repo
    with pytest.raises(ModelSourceError, match=error):
        fetch_model(source, tmp_path / "model", 4096, model_format=model_format)
    assert not downloads
    assert list(tmp_path.iterdir()) == []


def test_gguf_from_s3_must_be_one_model(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    objects = {"m/a-Q4_K_M.gguf": gguf_bytes(), "m/b-Q8_0.gguf": gguf_bytes(), "m/x.json": b"{}"}
    client = SimpleNamespace(
        get_paginator=lambda _: SimpleNamespace(
            paginate=lambda **_: [
                {"Contents": [{"Key": k, "Size": len(v)} for k, v in objects.items()]}
            ]
        ),
        get_object=lambda Bucket, Key: {"Body": io.BytesIO(objects[Key])},
    )
    monkeypatch.setattr("boto3.client", lambda _: client)
    with pytest.raises(ModelRejectedError, match="more than one GGUF model"):
        fetch_model("s3://models/m", tmp_path / "model", 4096, model_format="gguf")
    del objects["m/b-Q8_0.gguf"]
    manifest = fetch_model("s3://models/m", tmp_path / "model", 4096, model_format="gguf")
    assert manifest["model_file"] == "a-Q4_K_M.gguf"
    assert sorted(manifest["files"]) == ["a-Q4_K_M.gguf"]  # never the .json


def test_failures_have_fixed_codes() -> None:
    rejected = ModelRejectedError(Report(root=Path("/m"), problems=["x.bin: pickle"]))
    assert failure(rejected, "validating") == (
        "model-rejected",
        {"stage": "validating", "error_type": "ModelRejectedError", "problems": ["x.bin: pickle"]},
    )
    code, detail = failure(ModelSourceError("no GGUF model matches Q2_K"), "fetching")
    assert (code, detail["message"]) == ("model-not-found", "no GGUF model matches Q2_K")
    # Anything else logs only its type: its text could carry a URL with credentials.
    code, detail = failure(OSError("https://user:secret@host/x"), "fetching")
    assert code == "image-unavailable" and "secret" not in json.dumps(detail)
    assert failure(RuntimeError(), "loading")[0] == "driver-problem"
