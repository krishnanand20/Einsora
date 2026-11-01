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
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from tensora_ml.validate import (
    ALLOWED_NAMES,
    ALLOWED_SUFFIXES,
    DEFAULT_MAX_TOTAL_BYTES,
    Report,
    validate_model_dir,
)

MANIFEST_NAME = "tensora-manifest.json"

_HF_URI = re.compile(r"^hf://(?P<repo>[A-Za-z0-9][\w.-]*/[\w.-]+)(?:@(?P<rev>[\w.-]+))?$")
_S3_URI = re.compile(r"^s3://(?P<bucket>[a-z0-9][a-z0-9.-]{1,61}[a-z0-9])(?:/(?P<prefix>.*))?$")


class ModelRejectedError(Exception):
    def __init__(self, report: Report) -> None:
        super().__init__("; ".join(report.problems))
        self.report = report


@dataclass(frozen=True)
class HFSource:
    repo_id: str
    revision: str | None


@dataclass(frozen=True)
class S3Dest:
    bucket: str
    prefix: str

    def key(self, name: str) -> str:
        return f"{self.prefix}/{name}" if self.prefix else name


def parse_source(uri: str) -> HFSource:
    """Parse hf://org/model or hf://org/model@revision. Pin a commit SHA for reproducibility."""
    m = _HF_URI.match(uri)
    if not m:
        raise ValueError(f"source must look like hf://org/model[@revision], got {uri!r}")
    return HFSource(repo_id=m["repo"], revision=m["rev"])


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
    workdir: Path | None = None,
) -> dict[str, Any]:
    """Download → validate → upload. Only files the validator accepted are uploaded."""
    src, dst = parse_source(source), parse_dest(dest)

    import boto3
    from huggingface_hub import snapshot_download

    with tempfile.TemporaryDirectory(dir=workdir) as tmp:
        # Never even download disallowed file types.
        patterns = [f"*{s}" for s in sorted(ALLOWED_SUFFIXES)] + sorted(ALLOWED_NAMES)
        local = Path(
            snapshot_download(
                repo_id=src.repo_id,
                revision=src.revision,
                local_dir=tmp,
                allow_patterns=patterns,
            )
        )
        report = validate_model_dir(local, max_total_bytes)
        if not report.ok:
            raise ModelRejectedError(report)

        s3 = boto3.client("s3")
        manifest: dict[str, Any] = {"source": source, "files": {}}
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
