"""Fetch code-free models into one workload's private directory on its own machine."""

from __future__ import annotations

import json
import tempfile
from collections.abc import Callable
from pathlib import Path, PurePosixPath
from typing import Any

from tensorafi_ml.ingest import (
    MANIFEST_NAME,
    ModelRejectedError,
    parse_dest,
    parse_source,
    sha256_file,
)
from tensorafi_ml.validate import ALLOWED_NAMES, ALLOWED_SUFFIXES, validate_model_dir

MAX_FILES = 10_000


def safe_name(name: str) -> str:
    path = PurePosixPath(name)
    if (
        not name
        or path.is_absolute()
        or path.as_posix() != name
        or any(p in {".", ".."} for p in path.parts)
        or "\\" in name
        or any(ord(c) < 32 or ord(c) == 127 for c in name)
    ):
        raise ValueError("model contains an unsafe file name")
    return name


def allowed_name(name: str) -> bool:
    path = PurePosixPath(safe_name(name))
    return (
        not any(p.startswith(".") for p in path.parts)
        and path.name != MANIFEST_NAME
        and (path.suffix in ALLOWED_SUFFIXES or path.name in ALLOWED_NAMES)
    )


def _check_sizes(files: list[tuple[str, int]], limit: int) -> None:
    if not files or len(files) > MAX_FILES:
        raise ValueError("model must contain between 1 and 10000 allowed files")
    if any(size < 0 for _, size in files) or sum(size for _, size in files) > limit:
        raise ValueError("model exceeds the workload's model size limit")


def _hf(source: str, dest: Path, limit: int) -> str:
    from huggingface_hub import HfApi, hf_hub_download

    src = parse_source(source)
    info = HfApi().model_info(src.repo_id, revision=src.revision, files_metadata=True)
    if not info.sha or not info.siblings:
        raise ValueError("model has no resolved revision or file metadata")
    files = []
    for item in info.siblings:
        if allowed_name(item.rfilename):
            if item.size is None:
                raise ValueError("model file size is unknown")
            files.append((item.rfilename, item.size))
    _check_sizes(files, limit)
    for name, _ in files:
        hf_hub_download(src.repo_id, name, revision=info.sha, local_dir=dest)
    return info.sha


def _s3(source: str, dest: Path, limit: int) -> str:
    import boto3

    src = parse_dest(source)
    prefix = src.prefix + "/" if src.prefix else ""
    client = boto3.client("s3")
    files: list[tuple[str, int]] = []
    for page in client.get_paginator("list_objects_v2").paginate(Bucket=src.bucket, Prefix=prefix):
        for item in page.get("Contents", []):
            key = item["Key"]
            if not key.startswith(prefix) or key.endswith("/"):
                continue
            name = key[len(prefix) :]
            if allowed_name(name):
                files.append((name, item["Size"]))
                if len(files) > MAX_FILES:
                    raise ValueError("too many model files")
    _check_sizes(files, limit)
    for name, size in files:
        path = dest / name
        path.parent.mkdir(parents=True, exist_ok=True)
        result = client.get_object(Bucket=src.bucket, Key=prefix + name)
        body = result["Body"]
        try:
            with path.open("xb") as out:
                remaining = size
                while chunk := body.read(min(1 << 20, remaining + 1)):
                    remaining -= len(chunk)
                    if remaining < 0:
                        raise ValueError("model file grew while it was downloaded")
                    out.write(chunk)
                if remaining:
                    raise ValueError("model file was truncated while it was downloaded")
        finally:
            body.close()
    return ""  # S3 has no repository revision; the manifest records every file's checksum.


def fetch_model(
    source: str,
    dest: Path,
    max_total_bytes: int,
    *,
    stage: Callable[[str], None] = lambda _: None,
) -> dict[str, Any]:
    """Download allowed files, validate without loading weights, and publish a checksum manifest.

    Nothing is shared between workloads. Interrupted downloads never become a usable model.
    Credentials are supplied by the worker's environment, never a URI or argument.
    """
    if max_total_bytes <= 0:
        raise ValueError("model size limit must be positive")
    if dest.is_symlink():
        raise ValueError("model destination must not be a symlink")
    if dest.exists():
        # A quick restart can arrive before the agent removed an endpoint's old data. Reuse only
        # the exact validated snapshot, never an arbitrary directory left by a failed download.
        stage("validating")
        manifest_path = dest / MANIFEST_NAME
        manifest_bytes = manifest_path.stat().st_size
        if manifest_bytes > 4 * 1024**2:
            raise ValueError("cached model manifest is too large")
        # Our manifest is metadata, not part of the model's original size allowance.
        report = validate_model_dir(dest, max_total_bytes + manifest_bytes)
        if not report.ok:
            raise ModelRejectedError(report)
        cached = json.loads(manifest_path.read_text(encoding="utf-8"))
        names = set(report.files) - {MANIFEST_NAME}
        if cached.get("source") != source or set(cached.get("files", {})) != names:
            raise ValueError("cached model does not match its source and manifest")
        for name in names:
            path = dest / name
            if cached["files"][name] != {
                "sha256": sha256_file(path),
                "bytes": path.stat().st_size,
            }:
                raise ValueError("cached model checksum does not match its manifest")
        return dict(cached)
    dest.parent.mkdir(parents=True, exist_ok=True)
    with (
        tempfile.TemporaryDirectory(dir=dest.parent, prefix="model-download-") as tmp,
        tempfile.TemporaryDirectory(dir=dest.parent, prefix="model-validated-") as checked,
    ):
        download = Path(tmp)
        stage("fetching")
        if source.startswith("hf://"):
            revision = _hf(source, download, max_total_bytes)
        elif source.startswith("s3://"):
            revision = _s3(source, download, max_total_bytes)
        else:
            raise ValueError("model source must use hf:// or s3://")
        stage("validating")
        report = validate_model_dir(download, max_total_bytes)
        if not report.ok:
            raise ModelRejectedError(report)
        # Discard download-tool metadata. Only files accepted by the validator are served.
        ready = Path(checked)
        manifest: dict[str, Any] = {"source": source, "revision": revision, "files": {}}
        for name in report.files:
            path = download / name
            target = ready / name
            target.parent.mkdir(parents=True, exist_ok=True)
            path.rename(target)
            manifest["files"][name] = {
                "sha256": sha256_file(target),
                "bytes": target.stat().st_size,
            }
        (ready / MANIFEST_NAME).write_text(json.dumps(manifest, indent=2), encoding="utf-8")
        ready.rename(dest)
    return manifest
