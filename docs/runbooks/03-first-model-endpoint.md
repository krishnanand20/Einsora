# 03: First model endpoint (KServe + vLLM, Datacenter profile)

Prerequisites: the node is in **container** mode ([02, part A](02-gpu-passthrough.md#part-a-container-mode)),
KServe is installed, and `cluster-config` has created the `einsora-vllm` ClusterServingRuntime.

```bash
kubectl get clusterservingruntime einsora-vllm
kubectl apply -f deploy/examples/vllm-endpoint.yaml
kubectl -n einsora-smoke get isvc qwen -w                         # READY=True in about 2–5 min
kubectl -n einsora-smoke logs deploy/qwen-predictor -c storage-initializer   # model download
kubectl -n einsora-smoke logs deploy/qwen-predictor -f            # vLLM startup
```

This runbook calls the endpoint directly, which is fine for a smoke test. Once it exists, user traffic goes
through the regional **inference gateway** (API keys, token counts, failover; ADR-0010). Repeat
the test through the gateway once it exists. To test CPU fallback, apply the same InferenceService with
`runtime: einsora-vllm-cpu`, no GPU resources, and a small model.

## Call it

```bash
kubectl -n einsora-smoke port-forward svc/qwen-predictor 8080:80 &
curl -s localhost:8080/v1/models | jq
curl -s localhost:8080/v1/chat/completions -H 'Content-Type: application/json' -d '{
  "model": "qwen",
  "messages": [{"role": "user", "content": "Say hello in Hindi and Tamil."}],
  "max_tokens": 64
}' | jq -r '.choices[0].message.content'
```

## Benchmark it

```bash
cd ml && uv run einsora-ml bench http://localhost:8080 --model qwen --requests 64 --concurrency 8
```

Record TTFT p50/p95 and tokens/s per GPU class in a table here. These numbers become the
basis for capacity planning and per-class concurrency limits.

| Date | GPU | Model | Concurrency | TTFT p50 | TTFT p95 | Output tokens/s |
|---|---|---|---|---|---|---|
| | | | | | | |

## Clean up

```bash
kubectl delete -f deploy/examples/vllm-endpoint.yaml
```

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `storage-initializer` fails | No internet egress, or a gated Hugging Face model (needs `HF_TOKEN`; prefer ungated models for smoke tests) |
| vLLM: `Cannot find any model weights with safetensors` | The model ships only `.bin` files, so it's correctly rejected by `--load-format=safetensors` |
| vLLM OOM | Model too large for the GPU; lower `--max-model-len` or use more GPUs (`gpu_count` sets `--tensor-parallel-size`) |
| Pending pod | The node is in passthrough mode, or the GPUs are taken |
