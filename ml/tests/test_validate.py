from __future__ import annotations

import json
import math
import struct
from collections.abc import Callable
from pathlib import Path

import pytest

from einsora_ml import validate
from einsora_ml.cli import main
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


def test_case_does_not_skip_checks(tmp_path: Path) -> None:
    # On macOS, a loader opening "tokenizer_config.json" gets "TOKENIZER_CONFIG.JSON".
    (tmp_path / "config.json").write_text("{}")
    write_safetensors(tmp_path / "model.safetensors")
    write_safetensors(tmp_path / "BAD.SAFETENSORS", data_len=4, offsets=(0, 8))
    (tmp_path / "TOKENIZER_CONFIG.JSON").write_text(json.dumps({"auto_map": {}}))
    problems = validate_model_dir(tmp_path).problems
    assert any("BAD.SAFETENSORS" in p and "offsets" in p for p in problems), problems
    assert any("TOKENIZER_CONFIG.JSON" in p and "auto_map" in p for p in problems), problems


# GGUF: a minimal writer, so every check can be shown failing on exactly one defect.

F32, Q4_0, Q8_0 = 0, 2, 8
_BLOCKS = {F32: (1, 4), Q4_0: (32, 18), Q8_0: (32, 34)}


def u32(n: int) -> bytes:
    return struct.pack("<I", n)


def u64(n: int) -> bytes:
    return struct.pack("<Q", n)


def gstr(text: str | bytes) -> bytes:
    raw = text.encode() if isinstance(text, str) else text
    return u64(len(raw)) + raw


def kv(key: str | bytes, value_type: int, payload: bytes) -> bytes:
    return gstr(key) + u32(value_type) + payload


def gguf_bytes(
    *,
    tensors: list[tuple[str, list[int], int]] | None = None,
    architecture: str | None = "llama",
    split: tuple[int, int, int] | None = None,
    alignment: int | None = None,
    extra_kv: list[bytes] | None = None,
    version: int = 3,
    offsets: list[int] | None = None,
    data_bytes: int | None = None,
    tail: int = 0,
) -> bytes:
    """A GGUF file with tensors (name, dims, ggml type) laid out as llama.cpp writes them."""
    tensors = [("token_embd.weight", [64, 2], F32)] if tensors is None else tensors
    pairs = list(extra_kv or [])
    if architecture is not None:
        pairs.append(kv("general.architecture", 8, gstr(architecture)))
    if alignment is not None:
        pairs.append(kv("general.alignment", 4, u32(alignment)))
    if split is not None:
        no, count, total = split
        pairs += [
            kv("split.no", 2, struct.pack("<H", no)),
            kv("split.count", 2, struct.pack("<H", count)),
            kv("split.tensors.count", 5, struct.pack("<i", total)),
        ]
    align = alignment or 32
    infos, offset = b"", 0
    for i, (name, dims, ggml_type) in enumerate(tensors):
        block, size = _BLOCKS.get(ggml_type, (1, 0))
        infos += gstr(name) + u32(len(dims)) + b"".join(map(u64, dims)) + u32(ggml_type)
        infos += u64(offsets[i] if offsets else offset)
        offset += -(-(math.prod(dims) // block * size) // align) * align
    head = b"GGUF" + u32(version) + u64(len(tensors)) + u64(len(pairs)) + b"".join(pairs) + infos
    head += b"\0" * (-len(head) % align)
    body = head + b"\0" * (offset if data_bytes is None else data_bytes)
    return body[:tail] if tail < 0 else body + b"x" * tail


def write_gguf(path: Path, **kwargs: object) -> Path:
    path.write_bytes(gguf_bytes(**kwargs))  # type: ignore[arg-type]
    return path


def problems_for(tmp_path: Path) -> list[str]:
    return validate_model_dir(tmp_path, model_format="gguf").problems


def test_gguf_model_passes(tmp_path: Path) -> None:
    write_gguf(tmp_path / "model.gguf", tensors=[("a", [64], F32), ("b", [32, 4], Q4_0)])
    (tmp_path / "README.md").write_text("# a model")
    (tmp_path / "LICENSE").write_text("MIT")
    report = validate_model_dir(tmp_path, model_format="gguf")
    assert report.ok, report.problems
    assert report.files == ["LICENSE", "README.md", "model.gguf"]
    assert report.model_file == "model.gguf"
    assert report.total_bytes == sum(p.stat().st_size for p in tmp_path.iterdir())
    assert report.as_dict()["model_format"] == "gguf"
    assert validate_model_dir(tmp_path, model_format="gguf", max_total_bytes=10).problems


def test_formats_do_not_mix(model: Path, tmp_path_factory: pytest.TempPathFactory) -> None:
    write_gguf(model / "model.gguf")
    assert any(
        "model.gguf" in p and "not allowed in a safetensors model" in p
        for p in validate_model_dir(model).problems
    )
    # A GGUF model carries its own tokenizer and template: configs and safetensors don't belong.
    problems = validate_model_dir(model, model_format="gguf").problems
    for name in ("config.json", "model.safetensors", "tokenizer.json"):
        assert any(name in p and "not allowed in a gguf model" in p for p in problems), problems
    with pytest.raises(ValueError, match="format"):
        validate_model_dir(model, model_format="pickle")
    empty = tmp_path_factory.mktemp("empty")
    assert any("no .gguf weights" in p for p in problems_for(empty))


def _raw(data: bytes) -> Callable[[Path], None]:
    return lambda path: path.write_bytes(data) and None


def _with(**kwargs: object) -> Callable[[Path], None]:
    return lambda path: write_gguf(path, **kwargs) and None


@pytest.mark.parametrize(
    ("write", "expected"),
    [
        (_raw(b"GGML" + b"\0" * 60), "not a GGUF file"),
        (_with(version=1), "version 1 is no longer supported"),
        (_with(version=3 << 24), "isn't supported"),  # a big-endian file
        (_raw(gguf_bytes()[:30]), "header is truncated"),
        (_raw(b"GGUF" + u32(3) + u64(2**40) + u64(0)), "counts are invalid"),
        (_with(extra_kv=[kv(b"k" * 65536, 4, u32(1))]), "metadata key is too long"),
        (_with(extra_kv=[kv("ключ", 4, u32(1))]), "not ASCII"),
        (_with(extra_kv=[kv("a", 4, u32(1)), kv("a", 4, u32(1))]), "empty or repeated"),
        (_with(extra_kv=[kv("general.name", 8, u64(2**62))]), "header is truncated"),
        (_with(extra_kv=[kv("x", 9, u32(9) + u64(1))]), "nested GGUF arrays"),
        (_with(extra_kv=[kv("x", 9, u32(99) + u64(0))]), "unknown value type 99"),
        (_with(extra_kv=[kv("x", 13, b"")]), "unknown value type 13"),
        (_with(extra_kv=[kv("x", 9, u32(4) + u64(2**60))]), "header is truncated"),
        (_with(alignment=48), "power of two"),
        (_with(extra_kv=[kv("general.alignment", 10, u64(32))]), "must be a uint32"),
        (_with(architecture=None, extra_kv=[kv("general.architecture", 4, u32(1))]), "string"),
        (_with(tensors=[("t" * 64, [32], F32)]), "tensor name is too long"),
        (_with(tensors=[("a", [32], F32), ("a", [32], F32)]), "empty or repeated"),
        (_with(tensors=[("a", [32, 1, 1, 1, 1], F32)]), "5 dimensions"),
        (_with(tensors=[("a", [32, 0], F32)], data_bytes=0), "invalid shape"),
        (_with(tensors=[("a", [2**62, 4], F32)], data_bytes=0), "invalid shape"),
        (_with(tensors=[("a", [32], 4)], data_bytes=0), "ggml type 4"),  # removed (Q4_2)
        (_with(tensors=[("a", [32], 9)], data_bytes=0), "ggml type 9"),  # q8_1: compute only
        (_with(tensors=[("a", [32], 99)], data_bytes=0), "ggml type 99"),
        (_with(tensors=[("a", [48], Q4_0)], data_bytes=0), "not a multiple of Q4_0"),
        (_with(offsets=[32], data_bytes=544), "at offset 32, not 0"),
        (_with(tensors=[("a", [32], F32), ("b", [32], F32)], offsets=[0, 0]), "not 128"),
        (_with(tail=-1), "runs past the end"),
        (_with(tail=64), "64 bytes after its data"),
        (_with(tensors=[]), "contains no tensors"),
        (_with(architecture=None), "general.architecture is missing"),
        (_with(split=(1, 1, 1)), "only part"),
    ],
)
def test_malformed_gguf_is_rejected(
    tmp_path: Path, write: Callable[[Path], None], expected: str
) -> None:
    write(tmp_path / "model.gguf")
    report = validate_model_dir(tmp_path, model_format="gguf")
    assert any(expected in p for p in report.problems), report.problems
    assert not report.ok and report.model_file is None


def test_gguf_header_limit(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    write_gguf(tmp_path / "model.gguf")
    monkeypatch.setattr(validate, "MAX_GGUF_HEADER_BYTES", 64)
    assert any("over the 64-byte limit" in p for p in problems_for(tmp_path))


def write_split(
    directory: Path, prefix: str = "model", count: int = 3, parts: list[int] | None = None
) -> None:
    """A split model as gguf-split writes it: metadata in part 1, one tensor in each part."""
    for no in parts or range(1, count + 1):
        write_gguf(
            directory / f"{prefix}-{no:05d}-of-{count:05d}.gguf",
            tensors=[(f"blk.{no}.weight", [32], F32)],
            architecture="llama" if no == 1 else None,
            split=(no - 1, count, count),
        )


def test_split_gguf_model_passes(tmp_path: Path) -> None:
    write_split(tmp_path)
    report = validate_model_dir(tmp_path, model_format="gguf")
    assert report.ok, report.problems
    assert report.model_file == "model-00001-of-00003.gguf"
    assert len(report.files) == 3
    # gguf-split --no-tensor-first-split: part 1 holds only metadata.
    write_gguf(
        tmp_path / "model-00001-of-00003.gguf", tensors=[], split=(0, 3, 2), architecture="llama"
    )
    assert validate_model_dir(tmp_path, model_format="gguf").ok


@pytest.mark.parametrize(
    ("arrange", "expected"),
    [
        (lambda d: write_split(d, parts=[1, 3]), "has 2 of its 3 parts"),
        (lambda d: write_split(d, parts=[1]), "is one of 3 parts of a split model"),
        (lambda d: write_gguf(d / "model.gguf", split=(0, 3, 3)), "is one of 3 parts"),
        (lambda d: (write_gguf(d / "a.gguf"), write_gguf(d / "b.gguf")), "more than one GGUF"),
        (lambda d: (write_split(d, "q4", 2), write_split(d, "q8", 2)), "more than one GGUF"),
        (lambda d: (write_split(d), write_gguf(d / "mmproj.gguf")), "more than one GGUF"),
        (
            lambda d: (
                write_split(d, count=2, parts=[1]),
                (d / "sub").mkdir(),
                write_split(d / "sub", count=2, parts=[2]),
            ),
            "more than one GGUF",
        ),
        (
            lambda d: (
                write_split(d, count=2),
                write_gguf(d / "model-00002-of-00002.gguf", split=(0, 2, 2), architecture=None),
            ),
            "don't match its file name",
        ),
        (
            lambda d: (
                write_split(d, count=2),
                write_gguf(
                    d / "model-00002-of-00002.gguf",
                    tensors=[("blk.1.weight", [32], F32)],
                    split=(1, 2, 2),
                    architecture=None,
                ),
            ),
            "is also in model-00001-of-00002.gguf",
        ),
        (
            lambda d: (
                write_split(d, count=2),
                write_gguf(
                    d / "model-00001-of-00002.gguf",
                    tensors=[("blk.1.weight", [32], F32)],
                    split=(0, 2, 5),
                ),
            ),
            "split.tensors.count is 5",
        ),
        (
            lambda d: (
                write_split(d, count=2),
                write_gguf(
                    d / "model-00001-of-00002.gguf",
                    tensors=[("blk.1.weight", [32], F32)],
                    split=(0, 2, 2),
                    architecture=None,
                ),
            ),
            "general.architecture is missing",
        ),
    ],
)
def test_gguf_must_be_one_model(
    tmp_path: Path, arrange: Callable[[Path], object], expected: str
) -> None:
    arrange(tmp_path)
    report = validate_model_dir(tmp_path, model_format="gguf")
    assert any(expected in p for p in report.problems), report.problems
    assert report.model_file is None


def test_cli_validates_gguf(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    write_gguf(tmp_path / "model.gguf")
    assert main(["validate", str(tmp_path), "--format", "gguf"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["ok"] and out["model_format"] == "gguf" and out["model_file"] == "model.gguf"
    assert main(["validate", str(tmp_path)]) == 1  # as safetensors, it isn't


@pytest.mark.parametrize("name", ["-m.gguf", "a\nb.gguf", "-docs/README.md"])
def test_unsafe_names_are_rejected(tmp_path: Path, name: str) -> None:
    write_gguf(tmp_path / "model.gguf")
    (tmp_path / name).parent.mkdir(exist_ok=True)
    write_gguf(tmp_path / name)
    report = validate_model_dir(tmp_path, model_format="gguf")
    assert any("may not start with '-'" in p for p in report.problems), report.problems
    assert name not in report.files and report.model_file is None


def test_unreadable_file_is_a_problem(tmp_path: Path) -> None:
    write_gguf(tmp_path / "model.gguf").chmod(0)
    try:
        report = validate_model_dir(tmp_path, model_format="gguf")
    finally:
        (tmp_path / "model.gguf").chmod(0o600)
    assert any("can't be read" in p for p in report.problems), report.problems
