# ubuntu-24.04-cuda (Datacenter VM base image)

In the Datacenter profile, catalog id `ubuntu-24.04-cuda` refers to the CDI **DataSource** of the same name
in the `tensorafi-images` namespace. User VMs clone their root disk from it.

Base images are **per region** (ADR-0009). Import the same versioned DataSource into every region's
`tensorafi-images` namespace, so a catalog image means the same thing everywhere.

Lab machines will use the same Ubuntu image as a qcow2 file with the `libvirt` driver. Macs will use macOS
guest images with the `macos-vz` driver (ADR-0013). Both are planned.

## Starting point: the stock cloud image

`datasource.yaml` imports the Ubuntu 24.04 cloud image. The NVIDIA driver is **not**
included, so users install it themselves (`sudo ubuntu-drivers install --gpgpu`).
That's fine for testing, but users will expect drivers preinstalled.

```bash
kubectl apply -f images/vm/ubuntu-24.04-cuda/datasource.yaml
kubectl -n tensorafi-images get dv ubuntu-24.04-cuda -w     # wait for Succeeded
```

## Later: a prebaked image

Bake the driver, CUDA, and qemu-guest-agent into the qcow2, so VMs are usable on first boot:

1. Start from a dated Ubuntu cloud image. Pin the date, never `current`.
2. Use `virt-customize` (libguestfs) or Packer to install `nvidia-driver-<branch>-server`,
   `cuda-toolkit`, and `qemu-guest-agent`, then run `cloud-init clean`.
3. Publish it, and point `datasource.yaml` at it (with an `http` or `s3` source).
4. Version the DataSource name (`ubuntu-24.04-cuda-2026-11`) and update `catalog.yaml`.
   Existing VMs keep their own disks.
