"""GPU burn-in for new nodes: sustained bf16 matmul load, NaN checks, and a VRAM pattern test.

Run on every GPU before it runs anyone else's workloads (docs/runbooks/add-node.md). It needs
the `burnin` extra (PyTorch) and a node in container mode, because passthrough GPUs are
bound to vfio-pci and invisible to CUDA on the host.
"""

from __future__ import annotations

import time
from typing import Any

_PATTERNS = (0x00, 0xFF, 0x55, 0xAA)
_CHUNK_BYTES = 256 * 1024**2


def run(
    seconds: float = 300.0, matrix_size: int = 8192, vram_fraction: float = 0.85
) -> list[dict[str, Any]]:
    try:
        import torch
    except ImportError as exc:
        raise RuntimeError("burn-in needs PyTorch: uv sync --extra burnin") from exc
    if not torch.cuda.is_available():
        raise RuntimeError("no CUDA devices visible")
    return [
        _burn_device(torch, i, seconds, matrix_size, vram_fraction)
        for i in range(torch.cuda.device_count())
    ]


def _burn_device(
    torch: Any, index: int, seconds: float, n: int, vram_fraction: float
) -> dict[str, Any]:
    dev = torch.device("cuda", index)
    a = torch.randn(n, n, device=dev, dtype=torch.bfloat16)
    b = torch.randn(n, n, device=dev, dtype=torch.bfloat16)
    torch.cuda.synchronize(dev)

    iterations = nan_events = 0
    start = time.perf_counter()
    while time.perf_counter() - start < seconds:
        c = a @ b
        iterations += 1
        if iterations % 50 == 0:
            torch.cuda.synchronize(dev)
            if not bool(torch.isfinite(c).all()):
                nan_events += 1
    torch.cuda.synchronize(dev)
    elapsed = time.perf_counter() - start
    del a, b, c
    torch.cuda.empty_cache()

    vram_ok = _vram_pattern_test(torch, dev, vram_fraction)
    return {
        "index": index,
        "name": torch.cuda.get_device_name(dev),
        "seconds": round(elapsed, 1),
        "bf16_tflops": round(2 * n**3 * iterations / elapsed / 1e12, 1),
        "nan_events": nan_events,
        "vram_pattern_ok": vram_ok,
        "passed": nan_events == 0 and vram_ok,
    }


def _vram_pattern_test(torch: Any, dev: Any, fraction: float) -> bool:
    """Fill most of VRAM with bit patterns and read them back, chunk by chunk."""
    free, _total = torch.cuda.mem_get_info(dev)
    buf = torch.empty(int(free * fraction), dtype=torch.uint8, device=dev)
    try:
        for pattern in _PATTERNS:
            buf.fill_(pattern)
            for chunk in buf.split(_CHUNK_BYTES):
                if not bool((chunk == pattern).all()):
                    return False
        return True
    finally:
        del buf
        torch.cuda.empty_cache()
