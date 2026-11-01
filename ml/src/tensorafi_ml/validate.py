"""Model validation: the enforcement point for AGENTS.md rule 6.

A tenant model is accepted only if every file is on an allowlist, its weights are
safetensors with a well-formed header, and no config asks for custom code. Pickle-based
formats (.bin, .pt, .pkl, .ckpt) execute code when loaded, so they are rejected outright.
"""

from __future__ import annotations

import json
import os
import struct
from dataclasses import dataclass, field
from pathlib import Path

DEFAULT_MAX_TOTAL_BYTES = 200 * 1024**3
MAX_SAFETENSORS_HEADER_BYTES = 100 * 1024**2

#: File types a served model may contain. Anything else is rejected.
ALLOWED_SUFFIXES = frozenset(
    {".safetensors", ".json", ".txt", ".model", ".tiktoken", ".jinja", ".md"}
)
ALLOWED_NAMES = frozenset({"LICENSE", "NOTICE", ".gitattributes"})

#: Formats that can execute code, with a reason the user can act on.
DANGEROUS_SUFFIXES = {
    ".bin": "pickle-based PyTorch weights can execute code on load; convert to safetensors",
    ".pt": "pickle-based PyTorch weights can execute code on load; convert to safetensors",
    ".pth": "pickle-based PyTorch weights can execute code on load; convert to safetensors",
    ".pkl": "pickle files can execute code on load",
    ".pickle": "pickle files can execute code on load",
    ".ckpt": "checkpoint files are pickle-based and can execute code on load",
    ".py": "custom model code is not allowed (trust_remote_code is never enabled)",
    ".pyc": "compiled Python is not allowed",
    ".so": "native libraries are not allowed",
}

#: Configs that can request custom code via auto_map.
CODE_BEARING_CONFIGS = frozenset(
    {
        "config.json",
        "tokenizer_config.json",
        "processor_config.json",
        "preprocessor_config.json",
        "feature_extractor_config.json",
    }
)

#: Directories created by our own download tooling. Skipped, and never uploaded.
TOOL_DIRS = frozenset({".cache"})


@dataclass
class Report:
    root: Path
    files: list[str] = field(default_factory=list)
    total_bytes: int = 0
    problems: list[str] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return not self.problems

    def as_dict(self) -> dict[str, object]:
        return {
            "root": str(self.root),
            "ok": self.ok,
            "files": self.files,
            "total_bytes": self.total_bytes,
            "problems": self.problems,
        }


def validate_model_dir(root: Path, max_total_bytes: int = DEFAULT_MAX_TOTAL_BYTES) -> Report:
    """Check a local model directory. Report.files lists exactly what may be uploaded."""
    report = Report(root=root)
    for dirpath, dirnames, filenames in os.walk(root, followlinks=False):
        here = Path(dirpath)
        rel_dir = here.relative_to(root)
        if rel_dir.parts and rel_dir.parts[0] in TOOL_DIRS:
            dirnames.clear()
            continue
        for d in list(dirnames):
            if (here / d).is_symlink():
                report.problems.append(f"{(rel_dir / d).as_posix()}: symlinks are not allowed")
                dirnames.remove(d)
        for name in sorted(filenames):
            path = here / name
            rel = (rel_dir / name).as_posix()
            if path.is_symlink():
                report.problems.append(f"{rel}: symlinks are not allowed")
                continue
            if not path.is_file():
                report.problems.append(f"{rel}: not a regular file")
                continue
            reason = _reject_reason(path) or _check_contents(path)
            if reason:
                report.problems.append(f"{rel}: {reason}")
                continue
            report.files.append(rel)
            report.total_bytes += path.stat().st_size

    report.files.sort()
    if "config.json" not in report.files:
        report.problems.append("config.json is required at the top level")
    if not any(f.endswith(".safetensors") for f in report.files):
        report.problems.append("no .safetensors weights found")
    if report.total_bytes > max_total_bytes:
        report.problems.append(
            f"model is {report.total_bytes} bytes, over the {max_total_bytes}-byte limit"
        )
    return report


def _reject_reason(path: Path) -> str | None:
    suffix = path.suffix.lower()
    if suffix in DANGEROUS_SUFFIXES:
        return DANGEROUS_SUFFIXES[suffix]
    if suffix not in ALLOWED_SUFFIXES and path.name not in ALLOWED_NAMES:
        return f"file type not allowed ({suffix or 'no extension'})"
    return None


def _check_contents(path: Path) -> str | None:
    if path.suffix == ".safetensors":
        return _check_safetensors(path)
    if path.name in CODE_BEARING_CONFIGS:
        return _check_config(path)
    return None


def _check_config(path: Path) -> str | None:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        return f"invalid JSON: {exc}"
    if not isinstance(data, dict):
        return "expected a JSON object"
    if "auto_map" in data:
        return "auto_map requires custom model code, which is not allowed"
    if data.get("trust_remote_code"):
        return "trust_remote_code is not allowed"
    return None


def _check_safetensors(path: Path) -> str | None:
    """Verify the header is well formed and every tensor lies inside the file."""
    size = path.stat().st_size
    with path.open("rb") as f:
        prefix = f.read(8)
        if len(prefix) < 8:
            return "truncated safetensors file"
        (header_len,) = struct.unpack("<Q", prefix)
        if header_len > MAX_SAFETENSORS_HEADER_BYTES or 8 + header_len > size:
            return "safetensors header length is invalid"
        try:
            header = json.loads(f.read(header_len))
        except (UnicodeDecodeError, json.JSONDecodeError):
            return "safetensors header is not valid JSON"
    if not isinstance(header, dict):
        return "safetensors header must be a JSON object"
    data_len = size - 8 - header_len
    for name, info in header.items():
        if name == "__metadata__":
            continue
        offsets = info.get("data_offsets") if isinstance(info, dict) else None
        if not (
            isinstance(offsets, list)
            and len(offsets) == 2
            and all(isinstance(o, int) for o in offsets)
            and 0 <= offsets[0] <= offsets[1] <= data_len
        ):
            return f"tensor {name!r} has invalid data offsets"
    return None
