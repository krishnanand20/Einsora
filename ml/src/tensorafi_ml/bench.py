"""Load-test an OpenAI-compatible endpoint (vLLM): time to first token, latency, throughput.

Use it to size endpoints before offering a GPU class, and as a post-deploy smoke test.
"""

from __future__ import annotations

import asyncio
import json
import math
import time
from dataclasses import dataclass
from typing import Any

import httpx


@dataclass(frozen=True)
class Sample:
    ok: bool
    ttft_s: float
    total_s: float
    output_tokens: int
    error: str = ""


def percentile(values: list[float], p: float) -> float | None:
    """Nearest-rank percentile, p in [0, 100]. None for no data."""
    if not values:
        return None
    ordered = sorted(values)
    rank = max(1, math.ceil(p / 100 * len(ordered)))
    return ordered[rank - 1]


def summarize(samples: list[Sample], wall_s: float) -> dict[str, Any]:
    ok = [s for s in samples if s.ok]
    tokens = sum(s.output_tokens for s in ok)
    return {
        "requests": len(samples),
        "errors": len(samples) - len(ok),
        "ttft_p50_s": percentile([s.ttft_s for s in ok], 50),
        "ttft_p95_s": percentile([s.ttft_s for s in ok], 95),
        "latency_p50_s": percentile([s.total_s for s in ok], 50),
        "latency_p95_s": percentile([s.total_s for s in ok], 95),
        "output_tokens": tokens,
        "output_tokens_per_s": tokens / wall_s if wall_s > 0 else 0.0,
        "sample_errors": sorted({s.error for s in samples if s.error})[:5],
    }


def parse_sse(line: str) -> dict[str, Any] | None:
    """Return the JSON payload of an SSE data line; None for keep-alives and [DONE]."""
    if not line.startswith("data:"):
        return None
    data = line[len("data:") :].strip()
    if not data or data == "[DONE]":
        return None
    parsed = json.loads(data)
    return parsed if isinstance(parsed, dict) else None


async def _one(client: httpx.AsyncClient, url: str, payload: dict[str, Any]) -> Sample:
    start = time.perf_counter()
    ttft: float | None = None
    chunks = 0
    usage_tokens: int | None = None
    try:
        async with client.stream("POST", url, json=payload) as resp:
            if resp.status_code != httpx.codes.OK:
                return Sample(
                    False, 0.0, time.perf_counter() - start, 0, f"HTTP {resp.status_code}"
                )
            async for line in resp.aiter_lines():
                event = parse_sse(line)
                if event is None:
                    continue
                usage = event.get("usage")
                if isinstance(usage, dict) and isinstance(usage.get("completion_tokens"), int):
                    usage_tokens = usage["completion_tokens"]
                for choice in event.get("choices") or []:
                    if (choice.get("delta") or {}).get("content"):
                        chunks += 1
                        if ttft is None:
                            ttft = time.perf_counter() - start
    except (httpx.HTTPError, json.JSONDecodeError) as exc:
        return Sample(False, 0.0, time.perf_counter() - start, 0, type(exc).__name__)
    total = time.perf_counter() - start
    tokens = usage_tokens if usage_tokens is not None else chunks
    return Sample(True, ttft if ttft is not None else total, total, tokens)


async def run(
    base_url: str,
    model: str,
    *,
    requests: int = 64,
    concurrency: int = 8,
    prompt: str = "Explain how a GPU executes a matrix multiplication, step by step.",
    max_tokens: int = 256,
    api_key: str | None = None,
    timeout_s: float = 300.0,
) -> dict[str, Any]:
    url = base_url.rstrip("/") + "/v1/chat/completions"
    payload = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "max_tokens": max_tokens,
        "stream": True,
        "stream_options": {"include_usage": True},
    }
    headers = {"Authorization": f"Bearer {api_key}"} if api_key else {}
    sem = asyncio.Semaphore(concurrency)

    async with httpx.AsyncClient(timeout=timeout_s, headers=headers) as client:

        async def bounded() -> Sample:
            async with sem:
                return await _one(client, url, payload)

        start = time.perf_counter()
        samples = await asyncio.gather(*(bounded() for _ in range(requests)))
        wall = time.perf_counter() - start
    return summarize(list(samples), wall)
