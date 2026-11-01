from __future__ import annotations

import json
import socket
import sys
import tempfile
import threading
from collections.abc import Generator, Iterator
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from typing import Any

import httpx
import pytest

from tensora_ml.native import Piece, Request, Server, parse_request


class FakeEngine:
    def generate(self, request: Request) -> Generator[Piece]:
        yield Piece("Hello", 3, 1, None)
        yield Piece(" world", 3, 2, "length")


@pytest.fixture
def client() -> Iterator[httpx.Client]:
    # Unix socket paths must fit in macOS's 104-byte limit.
    with tempfile.TemporaryDirectory(dir="/tmp", prefix="te") as tmp:
        socket = str(Path(tmp) / "model.sock")
        with Server(socket, "hf://org/model", "worker-token", FakeEngine()) as server:
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            with httpx.Client(
                base_url="http://localhost",
                transport=httpx.HTTPTransport(uds=socket),
                headers={"Authorization": "token worker-token"},
            ) as client:
                try:
                    yield client
                finally:
                    server.shutdown()
                    thread.join()


def payload(**fields: Any) -> dict[str, Any]:
    return {"model": "hf://org/model", "messages": [{"role": "user", "content": "hello"}], **fields}


def test_models_require_the_internal_token(client: httpx.Client) -> None:
    assert client.get("/api", headers={"Authorization": ""}).status_code == 200
    assert (
        client.get("/v1/models", headers={"Authorization": "Bearer user-api-token"}).status_code
        == 401
    )
    response = client.get("/v1/models")
    assert response.json()["data"][0]["id"] == "hf://org/model"


def test_chat_and_usage(client: httpx.Client) -> None:
    response = client.post("/v1/chat/completions", json=payload())
    assert response.status_code == 200
    body = response.json()
    assert body["choices"][0]["message"] == {"role": "assistant", "content": "Hello world"}
    assert body["choices"][0]["finish_reason"] == "length"
    assert body["model"] == "hf://org/model"
    assert body["usage"] == {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}


def test_stream_is_openai_sse_with_usage(client: httpx.Client) -> None:
    response = client.post(
        "/v1/chat/completions", json=payload(stream=True, stream_options={"include_usage": True})
    )
    assert response.headers["content-type"] == "text/event-stream"
    events = [line[6:] for line in response.text.splitlines() if line.startswith("data: ")]
    assert events[-1] == "[DONE]"
    chunks = [json.loads(event) for event in events[:-1]]
    assert (
        "".join(c["choices"][0]["delta"]["content"] for c in chunks if c["choices"])
        == "Hello world"
    )
    assert chunks[-1]["choices"] == []
    assert chunks[-1]["usage"]["completion_tokens"] == 2


def test_text_completion(client: httpx.Client) -> None:
    response = client.post("/v1/completions", json={"model": "hf://org/model", "prompt": "Hello"})
    assert response.status_code == 200
    assert response.json()["choices"][0]["text"] == "Hello world"


@pytest.mark.parametrize(
    "fields",
    [
        {"model": "hf://other/model"},
        {"adapter_path": "/other/unsafe"},
        {"draft_model": "other"},
        {"trust_remote_code": True},
        {"max_tokens": True},
        {"max_tokens": 999999},
        {"temperature": float("nan")},
        {"top_p": 1.1},
        {"stream": "true"},
        {"messages": [{"role": "user", "content": {"image": "x"}}]},
    ],
)
def test_requests_cannot_change_models_or_enable_unsafe_features(fields: dict[str, Any]) -> None:
    with pytest.raises(ValueError):
        parse_request(payload(**fields), "hf://org/model", True)


def test_invalid_input_and_hidden_routes(client: httpx.Client) -> None:
    assert (
        client.post("/v1/chat/completions", json=payload(model="/etc/other-model")).status_code
        == 400
    )
    assert client.post("/v1/chat/completions", content=b"{broken").status_code == 400
    assert client.post("/v1/load", json={}).status_code == 404
    assert client.get("/v1/models?path=/tmp").status_code == 404


def test_client_body_is_bounded(client: httpx.Client) -> None:
    assert client.post("/v1/completions", content=b"x" * (1024**2 + 1)).status_code == 413


def test_generation_is_bounded_and_health_stays_available(
    client: httpx.Client, monkeypatch: pytest.MonkeyPatch
) -> None:
    entered, release = threading.Event(), threading.Event()

    def slow(self: FakeEngine, request: Request) -> Generator[Piece]:
        entered.set()
        assert release.wait(5)
        yield Piece("done", 1, 1, "stop")

    monkeypatch.setattr(FakeEngine, "generate", slow)
    with ThreadPoolExecutor(max_workers=1) as pool:
        pending = pool.submit(client.post, "/v1/chat/completions", json=payload())
        try:
            assert entered.wait(5)
            assert client.post("/v1/chat/completions", json=payload()).status_code == 503
            assert client.get("/api").status_code == 200
        finally:
            release.set()
        assert pending.result(timeout=5).status_code == 200


def test_stalled_body_times_out() -> None:
    with tempfile.TemporaryDirectory(dir="/tmp", prefix="te") as tmp:
        path = str(Path(tmp) / "model.sock")
        with Server(path, "hf://org/model", "worker-token", FakeEngine()) as server:
            server.request_timeout = 0.1
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
                    conn.settimeout(2)
                    conn.connect(path)
                    conn.sendall(
                        b"POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\n"
                        b"Authorization: token worker-token\r\nContent-Length: 10\r\n\r\n"
                    )
                    assert b"408" in conn.recv(4096)
            finally:
                server.shutdown()
                thread.join()


def test_error_carries_the_proxy_request_id(client: httpx.Client) -> None:
    request_id = "9ad0b111-3322-4bcd-abcd-111111111111"
    response = client.post(
        "/v1/chat/completions", json=payload(model="other"), headers={"X-Request-ID": request_id}
    )
    assert response.json()["error"]["request_id"] == request_id
    assert response.headers["X-Request-ID"] == request_id


def test_serving_reexec_starts_offline_without_fetch_credentials(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from tensora_ml import native

    monkeypatch.setattr(
        sys,
        "argv",
        [
            "native",
            "--source",
            "hf://org/model",
            "--model-dir",
            str(tmp_path / "model"),
            "--socket",
            str(tmp_path / "model.sock"),
            "--status",
            str(tmp_path / "status.json"),
            "--max-bytes",
            "4096",
        ],
    )
    monkeypatch.setenv("HF_TOKEN", "not-a-real-token")
    monkeypatch.setenv("AWS_SECRET_ACCESS_KEY", "not-a-real-credential")
    monkeypatch.setattr(native, "fetch_model", lambda *args, **kwargs: {})

    def reexec(executable: str, args: list[str], env: dict[str, str]) -> None:
        assert args[-1] == "--prepared"
        assert env["HF_HUB_OFFLINE"] == "1"
        assert env["TRANSFORMERS_OFFLINE"] == "1"
        assert "HF_TOKEN" not in env
        assert "AWS_SECRET_ACCESS_KEY" not in env
        raise SystemExit(0)

    monkeypatch.setattr(native.os, "execvpe", reexec)
    with pytest.raises(SystemExit) as exited:
        native.main()
    assert exited.value.code == 0
