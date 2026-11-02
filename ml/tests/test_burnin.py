from __future__ import annotations

from types import SimpleNamespace

import pytest

from einsora_ml import burnin


def fake_torch(*, cuda: int = 0, mps: bool | None = False) -> SimpleNamespace:
    backends = (
        SimpleNamespace()
        if mps is None
        else SimpleNamespace(mps=SimpleNamespace(is_available=lambda: mps))
    )
    return SimpleNamespace(
        cuda=SimpleNamespace(is_available=lambda: cuda > 0, device_count=lambda: cuda),
        backends=backends,
    )


def test_select_backend() -> None:
    assert burnin.select_backend(fake_torch(cuda=4)) == ("cuda", 4)
    assert burnin.select_backend(fake_torch(cuda=2, mps=True)) == ("cuda", 2)
    assert burnin.select_backend(fake_torch(mps=True)) == ("mps", 1)
    for torch in (fake_torch(), fake_torch(mps=None)):  # mps=None: a build without MPS at all
        with pytest.raises(RuntimeError, match=r"CUDA\) or Apple Silicon \(Metal"):
            burnin.select_backend(torch)


@pytest.mark.parametrize("fraction", [0.0, -0.1, 0.96, 2.0])
def test_memory_fraction_is_bounded(fraction: float) -> None:
    with pytest.raises(ValueError, match="memory fraction"):
        burnin.run(seconds=0, memory_fraction=fraction)
