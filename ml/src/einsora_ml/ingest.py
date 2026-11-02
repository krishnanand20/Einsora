"""Model ingestion: download from Hugging Face, validate, upload to S3 with a checksum manifest.

Runs as an operator CLI today. Planned: node agents run it to fill a machine's model cache
(Phases 1-2), and in a datacenter the controller launches it as a Kubernetes Job (Phase 3).
S3 endpoint and credentials come from the standard AWS_* environment variables
(AWS_ENDPOINT_URL for the in-cluster object store).
"""

from __future__ import annotations

import hashlib
import json
import re
import tempfile
from collections.abc import Iterable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from einsora_ml.validate import (
    DEFAULT_MAX_TOTAL_BYTES,
    Report,
    validate_model_dir,
)

MANIFEST_NAME = "einsora-manifest.json"

_HF_URI = re.compile(
    r"^hf://(?P<repo>[A-Za-z0-9][\w.-]*/[\w.-]+)(?:@(?P<rev>[\w.-]+))?"
    r"(?::(?P<tag>[A-Za-z0-9][\w.-]*))?$"
)
#: The parts of a split GGUF model, as llama.cpp names them.
_GGUF_PART = re.compile(r"(?P<model>.+)-\d{5}-of-(?P<count>\d{5})\.gguf", re.IGNORECASE)
_S3_URI = re.compile(r"^s3://(?P<bucket>[a-z0-9][a-z0-9.-]{1,61}[a-z0-9])(?:/(?P<prefix>.*))?$")


class ModelRejectedError(Exception):
    def __init__(self, report: Report) -> None:
        super().__init__("; ".join(report.problems))
        self.report = report


class ModelSourceError(ValueError):
    """The source doesn't name exactly one model: nothing to serve there, or a GGUF quantization
    tag that matches no model, or several. The message is shown to the model's owner."""


@dataclass(frozen=True)
class HFSource:
    repo_id: str
    revision: str | None
    #: The GGUF quantization to serve, such as Q4_K_M in hf://org/model-GGUF:Q4_K_M.
    tag: str | None = None


@dataclass(frozen=True)
class S3Dest:
    bucket: str
    prefix: str

    def key(self, name: str) -> str:
        return f"{self.prefix}/{name}" if self.prefix else name


def parse_source(uri: str) -> HFSource:
    """Parse hf://org/model, with an optional @revision (pin a commit SHA for reproducibility) and,
    for a GGUF model, :TAG naming one quantization, as in hf://org/model-GGUF:Q4_K_M."""
    m = _HF_URI.match(uri)
    if not m:
        raise ValueError(f"source must look like hf://org/model[@revision][:TAG], got {uri!r}")
    return HFSource(repo_id=m["repo"], revision=m["rev"], tag=m["tag"])


def select_gguf(names: Iterable[str], tag: str | None, where: str) -> list[str]:
    """The files of the one GGUF model a source names: the model whose name carries the
    quantization tag, ignoring case (Q4_K_M in model-Q4_K_M.gguf, or a Q4_K_M/ folder), or the
    source's only model when there's no tag. A split model is all of its parts; multimodal
    projectors (mmproj) aren't models. Never guesses between models (rule 10)."""
    models: dict[str, list[str]] = {}
    for name in names:
        if not name.lower().endswith(".gguf") or name.rsplit("/", 1)[-1].lower().startswith(
            "mmproj"
        ):
            continue
        part = _GGUF_PART.fullmatch(name)
        stem = part["model"] if part else name[: -len(".gguf")]
        models.setdefault(f"{stem}\0{part['count'] if part else ''}", []).append(name)
    found = sorted(key.split("\0")[0] for key in models)
    if tag is not None:
        token = re.compile(rf"(?:^|[-./]){re.escape(tag)}(?:$|[-./])", re.IGNORECASE)
        models = {k: v for k, v in models.items() if token.search(k.split("\0")[0])}
    if len(models) == 1:
        return sorted(next(iter(models.values())))
    listed = ", ".join(found[:20]) + (", …" if len(found) > 20 else "")
    if not found:
        raise ModelSourceError(f"{where} has no GGUF model")
    if not models:
        raise ModelSourceError(f"no GGUF model in {where} matches {tag}; it has {listed}")
    raise ModelSourceError(
        f"{where} has {len(models)} GGUF models{f' matching {tag}' if tag else ''} ({listed});"
        " name one with its quantization, as in hf://org/model-GGUF:Q4_K_M"
    )


def parse_dest(uri: str) -> S3Dest:
    m = _S3_URI.match(uri)
    if not m:
        raise ValueError(f"destination must look like s3://bucket[/prefix], got {uri!r}")
    prefix = (m["prefix"] or "").strip("/")
    if ".." in prefix.split("/"):
        raise ValueError("destination prefix must not contain '..'")
    return S3Dest(bucket=m["bucket"], prefix=prefix)


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def ingest(
    source: str,
    dest: str,
    *,
    max_total_bytes: int = DEFAULT_MAX_TOTAL_BYTES,
    model_format: str = "safetensors",
    workdir: Path | None = None,
) -> dict[str, Any]:
    """Download → validate → upload. Only files the validator accepted are uploaded, so a GGUF
    destination holds exactly one model, which agents then fetch from it."""
    dst = parse_dest(dest)

    import boto3

    from einsora_ml.local import download_hf

    with tempfile.TemporaryDirectory(dir=workdir) as tmp:
        # Only the files the format allows are downloaded, after checking their sizes.
        local = Path(tmp)
        revision = download_hf(source, local, max_total_bytes, model_format)
        report = validate_model_dir(local, max_total_bytes, model_format)
        if not report.ok:
            raise ModelRejectedError(report)

        s3 = boto3.client("s3")
        manifest: dict[str, Any] = {
            "source": source,
            "revision": revision,
            "model_format": model_format,
            "files": {},
        }
        for rel in report.files:
            path = local / rel
            manifest["files"][rel] = {"sha256": sha256_file(path), "bytes": path.stat().st_size}
            s3.upload_file(str(path), dst.bucket, dst.key(rel))
        s3.put_object(
            Bucket=dst.bucket,
            Key=dst.key(MANIFEST_NAME),
            Body=json.dumps(manifest, indent=2).encode(),
            ContentType="application/json",
        )
    return manifest
