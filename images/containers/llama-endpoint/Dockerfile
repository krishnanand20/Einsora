# syntax=docker/dockerfile:1
# The GGUF endpoint image (ADR-0022): llama.cpp's own server image at v0.5.0 (build b11146), pinned
# by digest, plus the Python worker's dependencies, pinned by hash. Build from the repository root,
# once per variant:
#   docker build -f images/containers/llama-endpoint/Dockerfile -t ghcr.io/krishnanand20/einsora-llama-endpoint:0.1.0 .
#   docker build -f images/containers/llama-endpoint/Dockerfile --build-arg VARIANT=cuda \
#     -t ghcr.io/krishnanand20/einsora-llama-endpoint:0.1.0-cuda .
# The driver supplies its bundled ML policy read-only and starts einsora_ml.llama_server, which
# validates a GGUF model before execing /app/llama-server offline on a Unix socket.
ARG VARIANT=cpu
FROM ghcr.io/ggml-org/llama.cpp:server-v0.5.0@sha256:a94b642b3e2620749bf2ff5672df922c23a6202710e54a8f7667e58df70aba5a AS base-cpu
FROM ghcr.io/ggml-org/llama.cpp:server-cuda-v0.5.0@sha256:52192a23c7b258044463aebba78826cdff39a7393b713edc313324b343a7b836 AS base-cuda
FROM base-${VARIANT}

# Both bases are Ubuntu 24.04 (amd64 and arm64), whose Python is 3.12, the version the lock and the
# worker are for; a base that changes it fails here. Wheels only, checked against their hashes: no
# build code runs.
COPY images/containers/llama-endpoint/requirements.txt /tmp/requirements.txt
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3 python3-venv ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && python3 -c 'import sys; assert sys.version_info[:2] == (3, 12), sys.version' \
 && python3 -m venv /opt/einsora/venv \
 && /opt/einsora/venv/bin/pip install --no-cache-dir --require-hashes --only-binary :all: -r /tmp/requirements.txt \
 && rm /tmp/requirements.txt
ENV PATH=/opt/einsora/venv/bin:$PATH

# The server listens on a Unix socket only, so the base image's TCP health check can't pass; the
# agent checks /health on the socket instead.
HEALTHCHECK NONE
USER 1000:1000
WORKDIR /work
# The driver overrides the entrypoint, sets HOME, and mounts only this workload's data and socket.
ENTRYPOINT ["python3", "-m", "einsora_ml.llama_server"]
