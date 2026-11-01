"""A single validated MLX model, served on a private Unix socket for the local access proxy.

The HTTP surface deliberately has no model loading, adapter, file, or administration endpoints.
Heavy libraries are imported only after fetching and validating the local model.
"""

from __future__ import annotations

import argparse
import hmac
import json
import math
import os
import socket
import socketserver
import sys
import threading
import time
import uuid
from collections.abc import Generator
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler
from pathlib import Path
from typing import Any, Protocol

from tensorafi_ml.ingest import ModelRejectedError
from tensorafi_ml.local import fetch_model

MAX_BODY_BYTES = 1024**2
MAX_CONTEXT_TOKENS = 8192
MAX_OUTPUT_TOKENS = 4096


@dataclass(frozen=True)
class Request:
    chat: bool
    prompt: str | list[dict[str, str]]
    max_tokens: int
    temperature: float
    top_p: float
    stream: bool
    include_usage: bool


def parse_request(body: Any, model: str, chat: bool) -> Request:
    allowed = {"model", "max_tokens", "temperature", "top_p", "stream", "stream_options"}
    allowed.add("messages" if chat else "prompt")
    if not isinstance(body, dict) or body.keys() - allowed:
        raise ValueError("request contains unsupported fields")
    if body.get("model") != model:
        raise ValueError("model must name the model listed by this endpoint's /v1/models")
    limit = body.get("max_tokens", 512)
    if type(limit) is not int or not 1 <= limit <= MAX_OUTPUT_TOKENS:
        raise ValueError("max_tokens must be between 1 and 4096")
    temperature, top_p = body.get("temperature", 0.0), body.get("top_p", 1.0)
    for value, low, high in [(temperature, 0, 2), (top_p, 0, 1)]:
        if type(value) not in (int, float) or not math.isfinite(value) or not low <= value <= high:
            raise ValueError("temperature must be between 0 and 2; top_p between 0 and 1")
    stream = body.get("stream", False)
    options = body.get("stream_options", {})
    if (
        type(stream) is not bool
        or not isinstance(options, dict)
        or options.keys() - {"include_usage"}
    ):
        raise ValueError("invalid streaming options")
    include_usage = options.get("include_usage", False)
    if type(include_usage) is not bool:
        raise ValueError("include_usage must be a boolean")
    prompt = body.get("messages" if chat else "prompt")
    if chat:
        if not isinstance(prompt, list) or not 1 <= len(prompt) <= 128:
            raise ValueError("messages must contain between 1 and 128 text messages")
        for message in prompt:
            if (
                not isinstance(message, dict)
                or message.keys() != {"role", "content"}
                or message["role"] not in ("system", "user", "assistant")
                or not isinstance(message["content"], str)
            ):
                raise ValueError(
                    "messages must have a system, user, or assistant role and text content"
                )
    elif not isinstance(prompt, str) or not prompt:
        raise ValueError("prompt must be nonempty text")
    return Request(chat, prompt, limit, float(temperature), float(top_p), stream, include_usage)


@dataclass(frozen=True)
class Piece:
    text: str
    prompt_tokens: int
    completion_tokens: int
    finish_reason: str | None


class Engine(Protocol):
    def generate(self, request: Request) -> Generator[Piece]: ...


class MLXEngine:
    def __init__(self, model: Path) -> None:
        import mlx.core as mx
        from mlx_lm import load

        if not mx.metal.is_available():
            raise RuntimeError("MLX Metal is required for native macOS endpoints")
        mx.set_default_device(mx.gpu)
        loaded = load(
            str(model),
            tokenizer_config={"trust_remote_code": False, "local_files_only": True},
            trust_remote_code=False,
        )
        self.model = loaded[0]
        self.tokenizer: Any = loaded[1]  # MLX's tokenizer wrapper exposes untyped methods.

    def generate(self, request: Request) -> Generator[Piece]:
        from mlx_lm import stream_generate
        from mlx_lm.sample_utils import make_sampler

        prompt = request.prompt
        if request.chat:
            prompt = self.tokenizer.apply_chat_template(
                prompt, tokenize=False, add_generation_prompt=True
            )
        tokens = self.tokenizer.encode(prompt)
        if len(tokens) + request.max_tokens > MAX_CONTEXT_TOKENS:
            raise ValueError("prompt and completion exceed the 8192-token context limit")
        for result in stream_generate(
            self.model,
            self.tokenizer,
            tokens,
            max_tokens=request.max_tokens,
            sampler=make_sampler(temp=request.temperature, top_p=request.top_p),
        ):
            yield Piece(
                result.text, result.prompt_tokens, result.generation_tokens, result.finish_reason
            )


class Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True
    request_timeout = 30.0

    def __init__(self, socket: str, model: str, token: str, engine: Engine) -> None:
        self.model, self.token, self.engine = model, token, engine
        self.generation = threading.Lock()
        self.requests = threading.BoundedSemaphore(16)
        super().__init__(socket, Handler)
        os.chmod(socket, 0o600)

    def process_request(
        self, request: socket.socket | tuple[bytes, socket.socket], client_address: Any
    ) -> None:
        if not self.requests.acquire(blocking=False):
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, client_address)
        except BaseException:
            self.requests.release()
            raise

    def process_request_thread(
        self, request: socket.socket | tuple[bytes, socket.socket], client_address: Any
    ) -> None:
        try:
            super().process_request_thread(request, client_address)
        finally:
            self.requests.release()


class Handler(BaseHTTPRequestHandler):
    server: Server

    def setup(self) -> None:
        super().setup()
        self.connection.settimeout(self.server.request_timeout)

    def _request_id(self) -> str:
        try:
            return str(uuid.UUID(self.headers.get("X-Request-ID", "")))
        except ValueError:
            if not hasattr(self, "generated_request_id"):
                self.generated_request_id = str(uuid.uuid4())
            return self.generated_request_id

    def log_message(self, format: str, *args: Any) -> None:
        pass  # Never log request bodies, query strings, or credentials.

    def _json(self, code: int, body: dict[str, Any]) -> None:
        data = json.dumps(body, allow_nan=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Request-ID", self._request_id())
        self.end_headers()
        self.wfile.write(data)

    def _error(self, code: int, message: str) -> None:
        self._json(code, self._error_body(code, message))

    def _error_body(self, status: int, message: str) -> dict[str, Any]:
        codes = {401: "unauthenticated", 404: "not_found", 500: "internal", 503: "unavailable"}
        return {
            "error": {
                "message": message,
                "code": codes.get(status, "invalid_request"),
                "type": "server_error" if status >= 500 else "invalid_request_error",
                "request_id": self._request_id(),
            }
        }

    def _authenticated(self) -> bool:
        values = self.headers.get_all("Authorization", [])
        if len(values) != 1 or not hmac.compare_digest(values[0], "Bearer " + self.server.token):
            self._error(401, "authentication required")
            return False
        return True

    def do_GET(self) -> None:
        if self.path == "/api":
            self._json(200, {"ready": True})
        elif self._authenticated():
            if self.path != "/v1/models":
                self._error(404, "not found")
            else:
                self._json(
                    200,
                    {
                        "object": "list",
                        "data": [{"id": self.server.model, "object": "model", "owned_by": "local"}],
                    },
                )

    def do_POST(self) -> None:
        if not self._authenticated():
            return
        if self.path not in {"/v1/completions", "/v1/chat/completions"}:
            self._error(404, "not found")
            return
        lengths = self.headers.get_all("Content-Length", [])
        if (
            len(lengths) != 1
            or not lengths[0].isascii()
            or not lengths[0].isdigit()
            or len(lengths[0]) > 10
            or self.headers.get("Transfer-Encoding")
        ):
            self._error(400, "a single Content-Length is required")
            return
        length = int(lengths[0])
        if not 0 < length <= MAX_BODY_BYTES:
            self._error(413, "request body exceeds the 1 MiB limit")
            return
        try:
            request = parse_request(
                json.loads(self.rfile.read(length)),
                self.server.model,
                self.path == "/v1/chat/completions",
            )
        except TimeoutError:
            self._error(408, "request body timed out")
            self.close_connection = True
            return
        except (ValueError, UnicodeDecodeError, RecursionError):
            self._error(
                400,
                "invalid request; use this endpoint's model, text input, and supported parameters",
            )
            return
        if not self.server.generation.acquire(blocking=False):
            self._error(503, "this endpoint is busy; retry after the current generation finishes")
            return
        started = False
        pieces = self.server.engine.generate(request)
        try:
            # Validate/tokenize before sending streaming headers so input failures remain HTTP 400.
            first = next(pieces)
            response_id = ("chatcmpl-" if request.chat else "cmpl-") + uuid.uuid4().hex
            base = {
                "id": response_id,
                "created": int(time.time()),
                "model": self.server.model,
                "object": "chat.completion" if request.chat else "text_completion",
            }
            if request.stream:
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Cache-Control", "no-store")
                self.send_header("Connection", "close")
                self.send_header("X-Request-ID", self._request_id())
                self.end_headers()
                started = True
                self.close_connection = True
                base["object"] = "chat.completion.chunk" if request.chat else "text_completion"
            text, last = "", first
            while True:
                text += last.text
                if request.stream:
                    choice: dict[str, Any] = {"index": 0, "finish_reason": last.finish_reason}
                    choice["delta" if request.chat else "text"] = (
                        {"role": "assistant", "content": last.text} if request.chat else last.text
                    )
                    self._sse({**base, "choices": [choice]})
                following = next(pieces, None)
                if following is None:
                    break
                last = following
            usage = {
                "prompt_tokens": last.prompt_tokens,
                "completion_tokens": last.completion_tokens,
                "total_tokens": last.prompt_tokens + last.completion_tokens,
            }
            if request.stream:
                if request.include_usage:
                    self._sse({**base, "choices": [], "usage": usage})
                self.wfile.write(b"data: [DONE]\n\n")
                self.wfile.flush()
            else:
                choice = {"index": 0, "finish_reason": last.finish_reason}
                choice["message" if request.chat else "text"] = (
                    {"role": "assistant", "content": text} if request.chat else text
                )
                self._json(200, {**base, "choices": [choice], "usage": usage})
        except (BrokenPipeError, ConnectionResetError):
            pass
        except ValueError:
            if started:
                self._sse(self._error_body(500, "generation failed"))
            else:
                self._error(
                    400, "input is not supported by this model or exceeds its context limit"
                )
        except Exception:
            if started:
                self._sse(self._error_body(500, "generation failed"))
            else:
                self._error(500, "generation failed")
        finally:
            pieces.close()
            self.server.generation.release()

    def _sse(self, body: dict[str, Any]) -> None:
        self.wfile.write(("data: " + json.dumps(body, allow_nan=False) + "\n\n").encode())
        self.wfile.flush()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", required=True)
    parser.add_argument("--model-dir", required=True, type=Path)
    parser.add_argument("--socket", required=True)
    parser.add_argument("--status", required=True, type=Path)
    parser.add_argument("--max-bytes", required=True, type=int)
    parser.add_argument("--prepared", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    os.umask(0o077)

    def stage(name: str, code: str = "") -> None:
        tmp = args.status.with_suffix(".tmp")
        tmp.write_text(json.dumps({"stage": name, "code": code}), encoding="utf-8")
        tmp.replace(args.status)

    phase = "fetching"
    try:
        if args.prepared and not args.model_dir.is_dir():
            raise ValueError("prepared model is missing")
        fetch_model(args.source, args.model_dir, args.max_bytes, stage=stage)
        # The model loader gets a local, validated directory and cannot fetch anything else.
        for name in list(os.environ):
            if name.startswith(("AWS_", "HF_")):
                os.environ.pop(name, None)
        os.environ.update(HF_HUB_OFFLINE="1", TRANSFORMERS_OFFLINE="1")
        phase = "loading"
        stage(phase)
        if not args.prepared:
            # Hub/Transformers cache offline flags at import. Start serving in a fresh process
            # image whose environment is offline and contains no model-download credentials.
            os.execvpe(  # noqa: S606 - trusted interpreter and driver-owned arguments
                sys.executable,
                [sys.executable, "-m", "tensorafi_ml.native", *sys.argv[1:], "--prepared"],
                dict(os.environ),
            )
        engine = MLXEngine(args.model_dir)
        token = os.environ.pop("TENSORAFI_NATIVE_TOKEN")
        with Server(args.socket, args.source, token, engine) as server:
            stage("ready")
            server.serve_forever()
    except Exception as exc:
        code = (
            "model-rejected"
            if isinstance(exc, ModelRejectedError)
            else "image-unavailable"
            if phase == "fetching"
            else "driver-problem"
        )
        stage("failed", code)
        print(json.dumps({"stage": phase, "error_type": type(exc).__name__}), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
