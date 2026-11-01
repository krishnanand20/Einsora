# syntax=docker/dockerfile:1
# Image for validate / ingest / bench. Burn-in runs from a CUDA PyTorch image instead
# (see docs/runbooks/add-node.md) because it needs torch and GPU drivers.
FROM python:3.12-slim AS build
COPY --from=ghcr.io/astral-sh/uv:0.8 /uv /bin/uv
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy UV_PYTHON_DOWNLOADS=never
WORKDIR /app
COPY pyproject.toml uv.lock README.md ./
RUN uv sync --frozen --no-dev --no-install-project
COPY src ./src
RUN uv sync --frozen --no-dev

FROM python:3.12-slim
RUN useradd --system --uid 10001 --home-dir /work einsora && mkdir /work && chown einsora /work
COPY --from=build /app /app
ENV PATH="/app/.venv/bin:$PATH" HF_HOME=/work/.hf
WORKDIR /work
USER 10001
ENTRYPOINT ["einsora-ml"]
CMD ["--help"]
