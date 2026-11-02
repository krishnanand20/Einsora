"""Model validation: the enforcement point for AGENTS.md rule 6.

A tenant model is accepted only if every file is on the allowlist for its weight format, its
weights have a well-formed header, and no config asks for custom code. There are two formats:

- safetensors (vLLM, MLX): a Hugging Face directory with config.json and .safetensors weights.
- GGUF (llama.cpp): exactly one model, as one .gguf file or every part of one split model. A GGUF
  file carries its own tokenizer and chat template, so only documentation may come with it.

Pickle-based formats (.bin, .pt, .pkl, .ckpt) execute code when loaded, so they are rejected
outright. Only headers are read; weights are never loaded.
"""

from __future__ import annotations

import json
import math
import os
import re
import struct
from dataclasses import dataclass, field
from pathlib import Path
from typing import BinaryIO

DEFAULT_MAX_TOTAL_BYTES = 200 * 1024**3
MAX_SAFETENSORS_HEADER_BYTES = 100 * 1024**2
MAX_GGUF_HEADER_BYTES = 100 * 1024**2

#: Weight formats, named as the catalog's model_format names them.
FORMATS = ("safetensors", "gguf")

#: File types a served safetensors model may contain. Anything else is rejected.
ALLOWED_SUFFIXES = frozenset(
    {".safetensors", ".json", ".txt", ".model", ".tiktoken", ".jinja", ".md"}
)
#: File types a served GGUF model may contain: its weights and documentation.
GGUF_ALLOWED_SUFFIXES = frozenset({".gguf", ".txt", ".md"})
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
    model_format: str = "safetensors"
    files: list[str] = field(default_factory=list)
    total_bytes: int = 0
    problems: list[str] = field(default_factory=list)
    #: The file a GGUF server loads: the model, or a split model's first part. A safetensors
    #: server loads the whole directory, so it has none.
    model_file: str | None = None

    @property
    def ok(self) -> bool:
        return not self.problems

    def as_dict(self) -> dict[str, object]:
        return {
            "root": str(self.root),
            "model_format": self.model_format,
            "ok": self.ok,
            "files": self.files,
            "model_file": self.model_file,
            "total_bytes": self.total_bytes,
            "problems": self.problems,
        }


def validate_model_dir(
    root: Path,
    max_total_bytes: int = DEFAULT_MAX_TOTAL_BYTES,
    model_format: str = "safetensors",
) -> Report:
    """Check a local model directory. Report.files lists exactly what may be uploaded."""
    if model_format not in FORMATS:
        raise ValueError(f"model format must be one of: {', '.join(FORMATS)}")
    report = Report(root=root, model_format=model_format)
    ggufs: dict[str, _GGUFFile] = {}
    gguf_rejected = False
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
            is_gguf = path.suffix.lower() == ".gguf"
            if any(p.startswith("-") or _has_control(p) for p in Path(rel).parts):
                # A server is given model_file on its command line, where "-x" would be a flag.
                report.problems.append(
                    f"{_quoted(rel)}: names may not start with '-' or contain control characters"
                )
                gguf_rejected |= is_gguf
                continue
            try:
                reason = _reject_reason(path, model_format)
                if reason is None and is_gguf:
                    ggufs[rel] = _read_gguf(path)
                elif reason is None:
                    reason = _check_contents(path)
            except _Invalid as exc:
                reason = str(exc)
            except OSError as exc:
                reason = f"can't be read ({exc.strerror or exc})"
            if reason:
                report.problems.append(f"{rel}: {reason}")
                gguf_rejected |= is_gguf
                continue
            report.files.append(rel)
            report.total_bytes += path.stat().st_size

    report.files.sort()
    if model_format == "safetensors":
        if "config.json" not in report.files:
            report.problems.append("config.json is required at the top level")
        if not any(f.endswith(".safetensors") for f in report.files):  # as loaders glob
            report.problems.append("no .safetensors weights found")
    elif not gguf_rejected:
        # One model, or the message would only repeat what a broken file already said.
        report.model_file, problems = _check_gguf_model(ggufs)
        report.problems.extend(problems)
    if report.total_bytes > max_total_bytes:
        report.problems.append(
            f"model is {report.total_bytes} bytes, over the {max_total_bytes}-byte limit"
        )
    if report.problems:
        report.model_file = None  # nothing to load from a rejected model
    return report


def _reject_reason(path: Path, model_format: str) -> str | None:
    suffix = path.suffix.lower()
    if suffix in DANGEROUS_SUFFIXES:
        return DANGEROUS_SUFFIXES[suffix]
    allowed = GGUF_ALLOWED_SUFFIXES if model_format == "gguf" else ALLOWED_SUFFIXES
    if suffix not in allowed and path.name not in ALLOWED_NAMES:
        return f"file type not allowed in a {model_format} model ({suffix or 'no extension'})"
    return None


def _check_contents(path: Path) -> str | None:
    # Case-insensitive: on macOS, "CONFIG.JSON" is the file a loader opens as "config.json".
    if path.suffix.lower() == ".safetensors":
        return _check_safetensors(path)
    if path.name.lower() in CODE_BEARING_CONFIGS:
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


# GGUF (https://github.com/ggml-org/ggml/blob/master/docs/gguf.md): a header, metadata key-value
# pairs, tensor descriptions, then aligned tensor data. The checks match the ones llama.cpp's own
# loader makes, and add bounds of ours, so a malformed file is refused here before any server
# parses it. All GGUF integers are little-endian.

_GGUF_STRING, _GGUF_ARRAY = 8, 9
#: Fixed-size metadata value types: type id -> struct format.
_GGUF_SCALARS = {
    0: "<B",  # uint8
    1: "<b",  # int8
    2: "<H",  # uint16
    3: "<h",  # int16
    4: "<I",  # uint32
    5: "<i",  # int32
    6: "<f",  # float32
    7: "<B",  # bool
    10: "<Q",  # uint64
    11: "<q",  # int64
    12: "<d",  # float64
}
_GGUF_TYPE_NAMES = {2: "uint16", 4: "uint32", 5: "int32", _GGUF_STRING: "string"}
#: Metadata this validator reads, and the type llama.cpp requires for each.
_GGUF_KEYS = {
    "general.architecture": _GGUF_STRING,
    "general.alignment": 4,
    "split.no": 2,
    "split.count": 2,
    "split.tensors.count": 5,
}
_GGUF_DEFAULT_ALIGNMENT = 32
_GGUF_MAX_KEY_BYTES = 65535  # the GGUF spec's limit
_GGUF_MAX_VALUE_BYTES = 256  # for the string values read here (general.architecture)
_GGUF_MAX_ENTRIES = 65536  # tensors, or metadata pairs, per file: real models have a few thousand
_GGML_MAX_NAME_BYTES = 63  # GGML_MAX_NAME, less its terminating NUL
_GGML_MAX_DIMS = 4
_INT64_MAX = 2**63 - 1

#: Tensor types llama.cpp stores in model files: ggml type id -> (name, values per block, bytes
#: per block). An allowlist like the file types: removed types, the q8_1 and q8_K types ggml uses
#: only while computing, and types newer than this table are refused.
_GGML_TYPES = {
    0: ("F32", 1, 4),
    1: ("F16", 1, 2),
    2: ("Q4_0", 32, 18),
    3: ("Q4_1", 32, 20),
    6: ("Q5_0", 32, 22),
    7: ("Q5_1", 32, 24),
    8: ("Q8_0", 32, 34),
    10: ("Q2_K", 256, 84),
    11: ("Q3_K", 256, 110),
    12: ("Q4_K", 256, 144),
    13: ("Q5_K", 256, 176),
    14: ("Q6_K", 256, 210),
    16: ("IQ2_XXS", 256, 66),
    17: ("IQ2_XS", 256, 74),
    18: ("IQ3_XXS", 256, 98),
    19: ("IQ1_S", 256, 50),
    20: ("IQ4_NL", 32, 18),
    21: ("IQ3_S", 256, 110),
    22: ("IQ2_S", 256, 82),
    23: ("IQ4_XS", 256, 136),
    24: ("I8", 1, 1),
    25: ("I16", 1, 2),
    26: ("I32", 1, 4),
    27: ("I64", 1, 8),
    28: ("F64", 1, 8),
    29: ("IQ1_M", 256, 56),
    30: ("BF16", 1, 2),
    34: ("TQ1_0", 256, 54),
    35: ("TQ2_0", 256, 66),
    39: ("MXFP4", 32, 17),
}

#: How llama.cpp names the parts of a split model, which is how it finds them.
_SPLIT_NAME = re.compile(r"(?P<prefix>.+)-(?P<no>\d{5})-of-(?P<count>\d{5})\.gguf")


class _Invalid(Exception):
    """A file breaks the model policy. The message is shown to the model's owner."""


@dataclass(frozen=True)
class _GGUFFile:
    architecture: str | None
    split_no: int | None
    split_count: int | None
    split_tensors: int | None
    tensors: tuple[str, ...]


class _GGUFReader:
    """Reads a GGUF header from the start of a file, never past the file or the header limit."""

    def __init__(self, f: BinaryIO, size: int) -> None:
        self._f, self._size = f, size
        self._buf = bytearray()
        self.pos = 0

    def _take(self, n: int) -> int:
        start, end = self.pos, self.pos + n
        if end > self._size:
            raise _Invalid("GGUF header is truncated")
        if end > MAX_GGUF_HEADER_BYTES:
            raise _Invalid(f"GGUF header is over the {MAX_GGUF_HEADER_BYTES}-byte limit")
        if end > len(self._buf):
            # Read ahead in large chunks: a header can hold a vocabulary of 250,000 strings.
            room = min(self._size, MAX_GGUF_HEADER_BYTES) - len(self._buf)
            chunk = self._f.read(min(max(end - len(self._buf), 1 << 20), room))
            self._buf += chunk
            if end > len(self._buf):
                raise _Invalid("GGUF file changed while it was read")
        self.pos = end
        return start

    def raw(self, n: int) -> bytes:
        start = self._take(n)
        return bytes(self._buf[start : start + n])

    def scalar(self, fmt: str) -> int | float:
        start = self._take(struct.calcsize(fmt))
        value: int | float = struct.unpack_from(fmt, self._buf, start)[0]
        return value

    def u32(self) -> int:
        return int(self.scalar("<I"))

    def u64(self) -> int:
        return int(self.scalar("<Q"))

    def string(self, limit: int, what: str) -> bytes:
        n = self.u64()
        if n > limit:
            raise _Invalid(f"GGUF {what} is too long ({n} bytes)")
        return self.raw(n)

    def skip(self, n: int) -> None:
        self._take(n)

    def skip_string(self) -> None:
        self._take(self.u64())


def _quoted(text: str) -> str:
    return repr(text if len(text) <= 64 else text[:64] + "…")


def _has_control(text: str) -> bool:
    return any(ord(c) < 32 or ord(c) == 127 for c in text)


def _pad(n: int, alignment: int) -> int:
    return -(-n // alignment) * alignment


def _read_gguf(path: Path) -> _GGUFFile:
    """Check one GGUF file's header and layout without reading its tensor data."""
    size = path.stat().st_size
    with path.open("rb") as f:
        r = _GGUFReader(f, size)
        if r.raw(4) != b"GGUF":
            raise _Invalid("not a GGUF file")
        version = r.u32()
        if version == 1:
            raise _Invalid("GGUF version 1 is no longer supported; convert the model again")
        if version not in (2, 3):
            raise _Invalid(
                f"GGUF version {version} isn't supported (little-endian versions 2 and 3 are)"
            )
        n_tensors, n_kv = r.u64(), r.u64()
        if n_tensors > _GGUF_MAX_ENTRIES or n_kv > _GGUF_MAX_ENTRIES:
            raise _Invalid(f"GGUF counts are invalid ({n_tensors} tensors, {n_kv} metadata)")

        meta: dict[str, int | str] = {}
        keys: set[str] = set()
        for _ in range(n_kv):
            try:
                key = r.string(_GGUF_MAX_KEY_BYTES, "metadata key").decode("ascii")
            except UnicodeDecodeError:
                raise _Invalid("GGUF metadata key is not ASCII") from None
            if not key or key in keys:
                raise _Invalid(f"GGUF metadata key {_quoted(key)} is empty or repeated")
            keys.add(key)
            value_type = r.u32()
            wanted = _GGUF_KEYS.get(key)
            if wanted is not None and value_type != wanted:
                raise _Invalid(f"GGUF metadata {key} must be a {_GGUF_TYPE_NAMES[wanted]}")
            value = _read_gguf_value(r, value_type, key)
            if wanted is not None and value is not None:
                meta[key] = value

        alignment = int(meta.get("general.alignment", _GGUF_DEFAULT_ALIGNMENT))
        if alignment == 0 or alignment & (alignment - 1):
            raise _Invalid("GGUF general.alignment must be a power of two")

        names: list[str] = []
        unique: set[str] = set()
        layout: list[tuple[str, int, int]] = []  # name, offset, bytes
        for _ in range(n_tensors):
            try:
                name = r.string(_GGML_MAX_NAME_BYTES, "tensor name").decode("utf-8")
            except UnicodeDecodeError:
                raise _Invalid("GGUF tensor name is not UTF-8") from None
            if not name or name in unique:
                raise _Invalid(f"GGUF tensor name {_quoted(name)} is empty or repeated")
            names.append(name)
            unique.add(name)
            n_dims = r.u32()
            if n_dims > _GGML_MAX_DIMS:
                raise _Invalid(f"tensor {_quoted(name)} has {n_dims} dimensions; at most 4")
            dims = [r.u64() for _ in range(n_dims)]
            type_id, offset = r.u32(), r.u64()
            layout.append((name, offset, _tensor_bytes(name, dims, type_id)))

        # Tensor data starts at the next aligned offset. llama.cpp requires each tensor to start
        # exactly where the one before it ends, padded to the alignment: no gaps, no overlaps.
        data_start = _pad(r.pos, alignment)
        expected = end = 0
        for name, offset, nbytes in layout:
            if offset != expected:
                raise _Invalid(f"tensor {_quoted(name)} is at offset {offset}, not {expected}")
            end = offset + nbytes
            expected = _pad(end, alignment)
        if layout and data_start + end > size:
            raise _Invalid("GGUF tensor data runs past the end of the file")
        if size > data_start + expected:
            raise _Invalid(f"GGUF file has {size - data_start - expected} bytes after its data")

    def number(key: str) -> int | None:
        value = meta.get(key)
        return value if isinstance(value, int) else None

    architecture = meta.get("general.architecture")
    return _GGUFFile(
        architecture=architecture if isinstance(architecture, str) and architecture else None,
        split_no=number("split.no"),
        split_count=number("split.count"),
        split_tensors=number("split.tensors.count"),
        tensors=tuple(names),
    )


def _read_gguf_value(r: _GGUFReader, value_type: int, key: str) -> int | str | None:
    """Read one metadata value, returning it only for the keys this validator checks."""
    keep = key in _GGUF_KEYS
    if value_type == _GGUF_STRING:
        if not keep:
            r.skip_string()
            return None
        try:
            return r.string(_GGUF_MAX_VALUE_BYTES, key).decode("utf-8")
        except UnicodeDecodeError:
            raise _Invalid(f"GGUF metadata {key} is not UTF-8") from None
    if value_type == _GGUF_ARRAY:
        item_type, count = r.u32(), r.u64()
        if item_type == _GGUF_STRING:
            if count > MAX_GGUF_HEADER_BYTES // 8:  # each string has an 8-byte length
                raise _Invalid("GGUF array is too long")
            for _ in range(count):
                r.skip_string()
        elif item_type in _GGUF_SCALARS:
            r.skip(count * struct.calcsize(_GGUF_SCALARS[item_type]))
        elif item_type == _GGUF_ARRAY:
            raise _Invalid("nested GGUF arrays aren't supported")
        else:
            raise _Invalid(f"GGUF metadata has unknown value type {item_type}")
        return None
    if value_type in _GGUF_SCALARS:
        value = r.scalar(_GGUF_SCALARS[value_type])
        return int(value) if keep else None
    raise _Invalid(f"GGUF metadata has unknown value type {value_type}")


def _tensor_bytes(name: str, dims: list[int], type_id: int) -> int:
    if type_id not in _GGML_TYPES:
        raise _Invalid(f"tensor {_quoted(name)} uses ggml type {type_id}, which isn't accepted")
    type_name, block, block_bytes = _GGML_TYPES[type_id]
    values = math.prod(dims)
    if any(d == 0 or d > _INT64_MAX for d in dims) or values > _INT64_MAX:
        raise _Invalid(f"tensor {_quoted(name)} has an invalid shape {dims}")
    row = dims[0] if dims else 1
    if row % block:
        raise _Invalid(
            f"tensor {_quoted(name)} has {row} values per row, not a multiple of"
            f" {type_name}'s block of {block}"
        )
    return values // block * block_bytes


def _check_gguf_model(files: dict[str, _GGUFFile]) -> tuple[str | None, list[str]]:
    """Accept exactly one model: one file, or every part of one split model. Returns the file
    llama.cpp loads (a split model's first part) and any problems."""
    if not files:
        return None, ["no .gguf weights found"]
    one_model = "keep exactly one GGUF model: a single .gguf file, or every part of one split model"
    names = sorted(files)
    if len(names) == 1:
        name, gguf = names[0], files[names[0]]
        if (gguf.split_count or 1) > 1:
            return None, [
                f"{name} is one of {gguf.split_count} parts of a split model; {one_model}"
            ]
        if gguf.split_no not in (None, 0):
            return None, [f"{name}: split.no is {gguf.split_no}, but it's the only part"]
        if gguf.architecture is None:
            return None, [f"{name}: general.architecture is missing"]
        if not gguf.tensors:
            return None, [f"{name}: contains no tensors"]
        return name, []

    parts = [_SPLIT_NAME.fullmatch(n) for n in names]
    if not all(parts) or len({(m["prefix"], m["count"]) for m in parts if m}) != 1:
        return None, [f"found more than one GGUF model ({', '.join(names)}); {one_model}"]
    count = int(parts[0]["count"]) if parts[0] else 0
    numbers = [int(m["no"]) for m in parts if m]
    if numbers != list(range(1, count + 1)):
        return None, [f"split model has {len(names)} of its {count} parts; {one_model}"]

    problems = []
    seen: dict[str, str] = {}
    for number, name in zip(numbers, names, strict=True):
        gguf = files[name]
        if gguf.split_no != number - 1 or gguf.split_count != count:
            problems.append(f"{name}: split.no and split.count don't match its file name")
        for tensor in gguf.tensors:
            if tensor in seen:
                problems.append(f"{name}: tensor {_quoted(tensor)} is also in {seen[tensor]}")
            seen.setdefault(tensor, name)
    first = files[names[0]]
    if first.architecture is None:
        problems.append(f"{names[0]}: general.architecture is missing")
    if not seen:
        problems.append("split model contains no tensors")
    elif first.split_tensors is not None and first.split_tensors != len(seen):
        problems.append(
            f"{names[0]}: split.tensors.count is {first.split_tensors},"
            f" but the parts hold {len(seen)} tensors"
        )
    return (None, problems) if problems else (names[0], [])
