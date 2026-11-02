"""Load-test an OpenAI-compatible endpoint: time to first token, latency, throughput.

Works with vLLM, llama.cpp's llama-server, MLX servers, and Einsora's endpoints. Use it to size
endpoints before offering a GPU class, and as a post-deploy smoke test.
"""

from __future__ import annotations

import asyncio
import ipaddress
import json
import math
import time
from dataclasses import dataclass
from typing import Any
from urllib.parse import urlsplit

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


def api_base(base_url: str) -> str:
    """The server's /v1 URL, from its root or from its /v1 base URL (as Einsora shows it)."""
    base = base_url.rstrip("/")
    return base if base.endswith("/v1") else base + "/v1"


def check_key_transport(base_url: str, insecure_http: bool = False) -> None:
    """Refuse to send an API key over plain HTTP to another machine, as the einsora CLI does.
    Loopback addresses and *.localhost names (Einsora's endpoints on this machine) are fine."""
    url = urlsplit(base_url)
    host = (url.hostname or "").lower()
    if url.scheme == "https" or insecure_http or host == "localhost" or host.endswith(".localhost"):
        return
    try:
        if ipaddress.ip_address(host).is_loopback:
            return
    except ValueError:
        pass
    raise ValueError(
        f"refusing to send EINSORA_BENCH_API_KEY over plain HTTP to {url.netloc}: use https://,"
        " or set EINSORA_INSECURE_HTTP=true if you trust every network in between"
    )


def only_model(listing: Any) -> str:
    """The model a server lists at /v1/models, when it serves exactly one."""
    data = listing.get("data") if isinstance(listing, dict) else None
    ids = [m.get("id") for m in data if isinstance(m, dict)] if isinstance(data, list) else []
    names = [i for i in ids if isinstance(i, str) and i]
    if len(names) != 1:
        raise ValueError(f"the endpoint lists {len(names)} models; choose one with --model")
    return names[0]


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
    model: str | None = None,
    *,
    requests: int = 64,
    concurrency: int = 8,
    prompt: str = "Explain how a GPU executes a matrix multiplication, step by step.",
    max_tokens: int = 256,
    api_key: str | None = None,
    timeout_s: float = 300.0,
    insecure_http: bool = False,
    transport: httpx.AsyncBaseTransport | None = None,
) -> dict[str, Any]:
    """Send requests, concurrency at a time. Without a model, use the one the server lists."""
    base = api_base(base_url)
    if api_key:
        check_key_transport(base, insecure_http)
    headers = {"Authorization": f"Bearer {api_key}"} if api_key else {}
    sem = asyncio.Semaphore(concurrency)

    async with httpx.AsyncClient(timeout=timeout_s, headers=headers, transport=transport) as client:
        if model is None:
            listing = await client.get(f"{base}/models")
            listing.raise_for_status()
            model = only_model(listing.json())
        url = f"{base}/chat/completions"
        payload = {
            "model": model,
            "messages": [{"role": "user", "content": prompt}],
            "max_tokens": max_tokens,
            "stream": True,
            "stream_options": {"include_usage": True},
        }

        async def bounded() -> Sample:
            async with sem:
                return await _one(client, url, payload)

        start = time.perf_counter()
        samples = await asyncio.gather(*(bounded() for _ in range(requests)))
        wall = time.perf_counter() - start
    return {"model": model, **summarize(list(samples), wall)}
