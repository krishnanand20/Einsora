# syntax=docker/dockerfile:1
# Build from the repository root. The driver supplies its bundled ML policy read-only and starts
# tensora_ml.vllm_server, which validates local safetensors before execing the offline model server.
# v0.11.0 supports a Unix socket (--uds); the Datacenter runtime is pinned independently.
FROM vllm/vllm-openai:v0.11.0

RUN python3 -m pip install --no-cache-dir boto3==1.43.117

USER 1000:1000
WORKDIR /work
# The driver overrides the entrypoint, sets HOME, and mounts only this workload's data and socket.
ENTRYPOINT ["python3", "-m", "tensora_ml.vllm_server"]
