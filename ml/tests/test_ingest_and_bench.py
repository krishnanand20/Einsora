from __future__ import annotations

import pytest

from tensorafi_ml.bench import Sample, parse_sse, percentile, summarize
from tensorafi_ml.ingest import HFSource, S3Dest, parse_dest, parse_source


def test_parse_source() -> None:
    assert parse_source("hf://Qwen/Qwen2.5-0.5B-Instruct") == HFSource(
        "Qwen/Qwen2.5-0.5B-Instruct", None
    )
    assert parse_source("hf://org/model@3f2a1b9").revision == "3f2a1b9"


@pytest.mark.parametrize(
    "uri",
    ["http://x/y", "hf://only-org", "hf://org/model/extra", "hf://../etc@x", "hf://org/m@a b"],
)
def test_parse_source_rejects(uri: str) -> None:
    with pytest.raises(ValueError, match="hf://"):
        parse_source(uri)


def test_parse_dest() -> None:
    assert parse_dest("s3://models/acme/llm/") == S3Dest("models", "acme/llm")
    assert parse_dest("s3://models").key("config.json") == "config.json"
    assert parse_dest("s3://models/t/w").key("config.json") == "t/w/config.json"
    with pytest.raises(ValueError):
        parse_dest("s3://models/../other")


def test_percentile() -> None:
    assert percentile([], 50) is None
    assert percentile([3.0, 1.0, 2.0], 50) == 2.0
    assert percentile([float(i) for i in range(1, 101)], 95) == 95.0


def test_summarize() -> None:
    samples = [
        Sample(True, 0.1, 1.0, 100),
        Sample(True, 0.3, 2.0, 50),
        Sample(False, 0, 0.5, 0, "HTTP 503"),
    ]
    s = summarize(samples, wall_s=2.0)
    assert s["requests"] == 3
    assert s["errors"] == 1
    assert s["output_tokens"] == 150
    assert s["output_tokens_per_s"] == 75.0
    assert s["sample_errors"] == ["HTTP 503"]


def test_parse_sse() -> None:
    assert parse_sse(": keep-alive") is None
    assert parse_sse("data: [DONE]") is None
    assert parse_sse('data: {"choices": []}') == {"choices": []}
