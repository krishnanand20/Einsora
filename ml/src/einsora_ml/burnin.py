"""GPU burn-in for new machines: sustained matmul load, NaN checks, and a memory pattern test.

Run it on every GPU before it runs anyone else's workloads (docs/runbooks/add-node.md). It needs
the `burnin` extra (PyTorch) and tests NVIDIA GPUs with CUDA, or an Apple Silicon GPU with Metal
(MPS). On a Datacenter node, run it in container mode: passthrough GPUs are bound to vfio-pci and
invisible to CUDA on the host. A Mac's GPU shares memory with macOS and every open app, so the
pattern test there covers a smaller share of it by default.
"""

from __future__ import annotations

import subprocess
import time
from typing import Any

_PATTERNS = (0x00, 0xFF, 0x55, 0xAA)
_CHUNK_BYTES = 256 * 1024**2  # Metal also caps the size of any one buffer
#: Share of the GPU's free memory the pattern test fills, by backend: a dedicated NVIDIA card's
#: memory, or the part of a Mac's unified memory that Metal recommends the GPU use.
DEFAULT_MEMORY_FRACTION = {"cuda": 0.85, "mps": 0.5}


def run(
    seconds: float = 300.0, matrix_size: int = 8192, memory_fraction: float | None = None
) -> list[dict[str, Any]]:
    if memory_fraction is not None and not 0 < memory_fraction <= 0.95:
        raise ValueError("memory fraction must be above 0 and at most 0.95")
    try:
        import torch
    except ImportError as exc:
        raise RuntimeError("burn-in needs PyTorch: uv sync --extra burnin") from exc
    backend, count = select_backend(torch)
    fraction = DEFAULT_MEMORY_FRACTION[backend] if memory_fraction is None else memory_fraction
    return [
        _burn_device(torch, torch.device(backend, i), seconds, matrix_size, fraction)
        for i in range(count)
    ]


def select_backend(torch: Any) -> tuple[str, int]:
    """CUDA's devices on NVIDIA machines, or the one Apple GPU through Metal (MPS)."""
    if torch.cuda.is_available():
        return "cuda", int(torch.cuda.device_count())
    mps = getattr(torch.backends, "mps", None)
    if mps is not None and mps.is_available():
        return "mps", 1
    raise RuntimeError(
        "no GPU visible: burn-in needs an NVIDIA GPU (CUDA) or Apple Silicon (Metal)"
    )


def _burn_device(
    torch: Any, dev: Any, seconds: float, n: int, memory_fraction: float
) -> dict[str, Any]:
    # bf16 on NVIDIA, as most training uses; fp16 on Apple GPUs, which all run it natively.
    dtype = torch.bfloat16 if dev.type == "cuda" else torch.float16
    a = torch.randn(n, n, device=dev, dtype=dtype)
    b = torch.randn(n, n, device=dev, dtype=dtype)
    _synchronize(torch, dev)

    iterations = nan_events = 0
    start = time.perf_counter()
    while time.perf_counter() - start < seconds:
        c = a @ b
        iterations += 1
        if iterations % 50 == 0:
            _synchronize(torch, dev)
            if not bool(torch.isfinite(c).all()):
                nan_events += 1
    _synchronize(torch, dev)
    elapsed = time.perf_counter() - start
    del a, b, c
    _empty_cache(torch, dev)

    memory_ok, tested = _memory_pattern_test(torch, dev, memory_fraction)
    return {
        "index": dev.index or 0,
        "name": _device_name(torch, dev),
        "backend": dev.type,
        "dtype": str(dtype).removeprefix("torch."),
        "seconds": round(elapsed, 1),
        "tflops": round(2 * n**3 * iterations / elapsed / 1e12, 1),
        "nan_events": nan_events,
        "memory_tested_gib": round(tested / 1024**3, 1),
        "memory_pattern_ok": memory_ok,
        "passed": nan_events == 0 and memory_ok,
    }


def _memory_pattern_test(torch: Any, dev: Any, fraction: float) -> tuple[bool, int]:
    """Fill a share of the GPU's free memory with bit patterns and read them back, chunk by chunk.
    Returns whether every pattern read back intact, and how many bytes were tested."""
    total = int(_free_bytes(torch, dev) * fraction)
    if total <= 0:
        return False, 0  # nothing tested is not a pass
    chunks = []
    try:
        remaining = total
        while remaining > 0:
            chunks.append(torch.empty(min(_CHUNK_BYTES, remaining), dtype=torch.uint8, device=dev))
            remaining -= _CHUNK_BYTES
        for pattern in _PATTERNS:
            for chunk in chunks:
                chunk.fill_(pattern)
            for chunk in chunks:
                if not bool((chunk == pattern).all()):
                    return False, total
        return True, total
    finally:
        chunks.clear()
        _empty_cache(torch, dev)


def _free_bytes(torch: Any, dev: Any) -> int:
    if dev.type == "cuda":
        free, _total = torch.cuda.mem_get_info(dev)
        return int(free)
    # Unified memory: Metal's recommended working set for the GPU, less what's allocated already.
    return int(torch.mps.recommended_max_memory() - torch.mps.driver_allocated_memory())


def _synchronize(torch: Any, dev: Any) -> None:
    if dev.type == "cuda":
        torch.cuda.synchronize(dev)
    else:
        torch.mps.synchronize()


def _empty_cache(torch: Any, dev: Any) -> None:
    if dev.type == "cuda":
        torch.cuda.empty_cache()
    else:
        torch.mps.empty_cache()


def _device_name(torch: Any, dev: Any) -> str:
    if dev.type == "cuda":
        return str(torch.cuda.get_device_name(dev))
    # PyTorch doesn't name Apple GPUs; the chip's name does ("Apple M3 Pro").
    try:
        result = subprocess.run(
            ["/usr/sbin/sysctl", "-n", "machdep.cpu.brand_string"],
            capture_output=True,
            text=True,
            check=False,
        )
    except OSError:
        return "Apple GPU"
    return result.stdout.strip() or "Apple GPU"
