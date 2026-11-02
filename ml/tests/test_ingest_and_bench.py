from __future__ import annotations

import json

import httpx
import pytest

from einsora_ml import bench
from einsora_ml.bench import (
    Sample,
    api_base,
    check_key_transport,
    only_model,
    parse_sse,
    percentile,
    summarize,
)
from einsora_ml.ingest import (
    HFSource,
    ModelSourceError,
    S3Dest,
    parse_dest,
    parse_source,
    select_gguf,
)


def test_parse_source() -> None:
    assert parse_source("hf://Qwen/Qwen2.5-0.5B-Instruct") == HFSource(
        "Qwen/Qwen2.5-0.5B-Instruct", None
    )
    assert parse_source("hf://org/model@3f2a1b9").revision == "3f2a1b9"
    assert parse_source("hf://org/model-GGUF:Q4_K_M") == HFSource("org/model-GGUF", None, "Q4_K_M")
    assert parse_source("hf://org/model-GGUF@3f2a1b9:UD-IQ1_S") == HFSource(
        "org/model-GGUF", "3f2a1b9", "UD-IQ1_S"
    )


@pytest.mark.parametrize(
    "uri",
    [
        "http://x/y",
        "hf://only-org",
        "hf://org/model/extra",
        "hf://../etc@x",
        "hf://org/m@a b",
        "hf://org/m:",
        "hf://org/m:-flag",
        "hf://org/m:Q4:Q8",
    ],
)
def test_parse_source_rejects(uri: str) -> None:
    with pytest.raises(ValueError, match="hf://"):
        parse_source(uri)


REPO = [
    "README.md",
    "model-Q4_K_M.gguf",
    "model-UD-Q4_K_M.gguf",
    "model-Q4_K_S.gguf",
    "model-Q8_0.gguf",
    "mmproj-model-F16.gguf",
    "BF16/model-BF16-00001-of-00002.gguf",
    "BF16/model-BF16-00002-of-00002.gguf",
]


@pytest.mark.parametrize(
    ("tag", "names"),
    [
        ("Q8_0", ["model-Q8_0.gguf"]),
        ("q8_0", ["model-Q8_0.gguf"]),  # tags ignore case, as Hugging Face's do
        ("Q4_K_S", ["model-Q4_K_S.gguf"]),
        ("UD-Q4_K_M", ["model-UD-Q4_K_M.gguf"]),
        ("BF16", ["BF16/model-BF16-00001-of-00002.gguf", "BF16/model-BF16-00002-of-00002.gguf"]),
    ],
)
def test_select_gguf_by_tag(tag: str, names: list[str]) -> None:
    assert select_gguf(REPO, tag, "hf://org/model-GGUF") == names


@pytest.mark.parametrize(
    ("names", "tag", "error"),
    [
        (REPO, "Q4_K_M", "has 2 GGUF models matching Q4_K_M"),  # never a guess between them
        (REPO, "Q4_K", "no GGUF model in hf://org/m matches Q4_K"),  # a tag is a whole token
        (REPO, "F16", "matches F16"),  # a projector isn't a model, and BF16 isn't F16
        (REPO, None, "has 5 GGUF models"),
        (["README.md", "mmproj-F16.gguf"], None, "has no GGUF model"),
    ],
)
def test_select_gguf_never_guesses(names: list[str], tag: str | None, error: str) -> None:
    with pytest.raises(ModelSourceError, match=error):
        select_gguf(names, tag, "hf://org/m")


def test_select_gguf_takes_the_only_model() -> None:
    names = ["README.md", "tiny-00001-of-00002.gguf", "tiny-00002-of-00002.gguf", "mmproj.gguf"]
    assert select_gguf(names, None, "s") == names[1:3]


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


def test_api_base_accepts_the_root_or_v1() -> None:
    # Einsora's dashboard shows endpoints' /v1 base URL; servers' docs give their root.
    for url in ["http://127.0.0.1:8081", "http://127.0.0.1:8081/", "http://127.0.0.1:8081/v1/"]:
        assert api_base(url) == "http://127.0.0.1:8081/v1"
    assert api_base("http://w-1.localhost:8090/v1") == "http://w-1.localhost:8090/v1"


def test_only_model() -> None:
    assert only_model({"object": "list", "data": [{"id": "qwen", "object": "model"}]}) == "qwen"
    for listing in [{"data": []}, {"data": [{"id": "a"}, {"id": "b"}]}, {"data": "x"}, []]:
        with pytest.raises(ValueError, match="--model"):
            only_model(listing)


async def test_run_finds_the_model_and_counts_usage() -> None:
    seen: list[str] = []

    def server(request: httpx.Request) -> httpx.Response:
        assert request.headers["authorization"] == "Bearer tsk_test"
        seen.append(f"{request.method} {request.url.path}")
        if request.url.path == "/v1/models":
            return httpx.Response(200, json={"data": [{"id": "hf://org/model"}]})
        assert json.loads(request.content)["model"] == "hf://org/model"
        events = [
            {"choices": [{"delta": {"content": "Hel"}}]},
            {"choices": [{"delta": {"content": "lo"}}]},
            {"choices": [], "usage": {"completion_tokens": 7}},
        ]
        body = "".join(f"data: {json.dumps(e)}\n\n" for e in events) + "data: [DONE]\n\n"
        return httpx.Response(200, text=body, headers={"content-type": "text/event-stream"})

    result = await bench.run(
        "http://w-1.localhost:8090/v1/",
        requests=3,
        concurrency=2,
        api_key="tsk_test",
        transport=httpx.MockTransport(server),
    )
    assert seen == ["GET /v1/models"] + ["POST /v1/chat/completions"] * 3
    assert result["model"] == "hf://org/model"
    assert result["errors"] == 0 and result["output_tokens"] == 21


def test_api_key_never_goes_over_plain_http_to_another_machine() -> None:
    for url in [
        "https://gpu.example.com/v1",
        "http://127.0.0.1:8081",
        "http://[::1]:8081/v1",
        "http://localhost:8080",
        "http://w-1.localhost:8090/v1",  # an Einsora endpoint on this machine
    ]:
        check_key_transport(url)
    for url in [
        "http://192.168.1.20:8081",
        "http://gpu.example.com/v1",
        "http://localhost.evil.com",
    ]:
        with pytest.raises(ValueError, match="plain HTTP"):
            check_key_transport(url)
    check_key_transport("http://192.168.1.20:8081", insecure_http=True)
