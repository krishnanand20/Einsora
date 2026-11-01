# 02: GPU smoke test, burn-in, first passthrough VM (Datacenter profile)

On a single node, you test container mode first and then switch the node to passthrough. Both
modes can't run on one node at the same time ([gpu-sharing.md](../architecture/gpu-sharing.md)).

## Part A: container mode

```bash
kubectl label node <node> --overwrite nvidia.com/gpu.workload.config=container
kubectl get node <node> -o jsonpath='{.status.allocatable}' | jq   # expect "nvidia.com/gpu": "4"
kubectl apply -f deploy/examples/gpu-smoke-test.yaml
kubectl logs -f pod/gpu-smoke-test                                  # nvidia-smi table, all GPUs
kubectl delete -f deploy/examples/gpu-smoke-test.yaml
```

### Burn-in (required before anyone else uses a GPU)

Run `einsora-ml burnin` in a PyTorch CUDA image with all GPUs for at least 30 minutes:

```bash
kubectl run burnin --rm -it --restart=Never \
  --image=pytorch/pytorch:2.7.1-cuda12.8-cudnn9-runtime \
  --overrides='{"spec":{"containers":[{"name":"burnin","image":"pytorch/pytorch:2.7.1-cuda12.8-cudnn9-runtime",
    "command":["bash","-c","pip install -q /src && einsora-ml burnin --seconds 1800"],
    "resources":{"limits":{"nvidia.com/gpu":"4"}}}]}}'
```

The command above is illustrative. In practice, build `einsora-ml` into the image, or mount `ml/`.

**Pass criteria:**

- every GPU reports `passed: true`
- bf16 TFLOPS are within about 10% across identical cards
- no XID errors in DCGM
- temperatures are stable below the throttle point

## Part B: passthrough mode

```bash
# 1. No GPU pods may be running on the node.
kubectl get pods -A -o wide --field-selector spec.nodeName=<node> | grep -i -e gpu -e cuda
# 2. Switch the mode; the GPU Operator unloads the driver and binds vfio-pci.
kubectl label node <node> --overwrite nvidia.com/gpu.workload.config=vm-passthrough
kubectl -n gpu-operator get pods -o wide -w        # vfio-manager and sandbox-device-plugin become Ready
# 3. Confirm the advertised device resource matches catalog vm_device_name.
kubectl get node <node> -o jsonpath='{.status.allocatable}' | jq   # e.g. "nvidia.com/AD102GL_L40S": "4"
```

If the resource name differs from `images/catalog.yaml` and `deploy/environments/*.yaml`, fix those files
to match reality, then re-apply `cluster-config` (it carries KubeVirt `permittedHostDevices`).

### First VM

```bash
$EDITOR deploy/examples/vm-gpu-passthrough.yaml   # your SSH key; deviceName from step 3
kubectl apply -f deploy/examples/vm-gpu-passthrough.yaml
kubectl get vmi gpu-test -w                        # Running
virtctl ssh ubuntu@vm/gpu-test -n default
lspci | grep -i nvidia                             # the GPU is visible inside the VM
sudo ubuntu-drivers install --gpgpu && sudo reboot
nvidia-smi                                         # after reboot
```

### Memory remanence test (record the result in tenancy-and-isolation.md)

1. In the VM, fill GPU memory with a known pattern (a small CUDA or PyTorch script).
2. Delete the VM. Create a new one on the same GPU, then allocate memory **without** initializing it and search for the pattern.
3. If the pattern is found, a scrub step between users is mandatory before the GPU is shared.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| VMI Pending: `Insufficient nvidia.com/...` | Wrong `deviceName`, a `permittedHostDevices` mismatch, or the node is still in container mode |
| vfio-manager CrashLoop | The NVIDIA driver is still loaded on the host (a pod is still using the GPU), or nouveau isn't blacklisted |
| A GPU shares an IOMMU group with other devices | Enable ACS in the BIOS, or move the card to another slot |
| `BAR ... can't assign` in dmesg | Above 4G Decoding is disabled |
| The GPU is unusable after the VM stops | A reset quirk, common on consumer cards; check `dmesg` for reset errors and try a node reboot |
