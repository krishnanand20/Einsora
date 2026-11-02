# Security

## Reporting a vulnerability

Email **[krishnanand.dma@gmail.com](mailto:krishnanand.dma@gmail.com)** with the details and, if you can, steps
to reproduce. Please don't open public issues for vulnerabilities. We aim to acknowledge reports within 2
business days.

## Threat model in one paragraph

Einsora runs code on machines that may belong to someone other than the person launching it. Users may
be hostile to the machine's owner (the host), to the instance's operator, and to each other. Hosts can
see what runs on their own machines. We must prevent users from:

- escaping to a host or reaching its files or home network
- reading or affecting another user's data, GPUs, or traffic
- using someone's machine or IP address to attack third parties
- consuming more than their quota

We must also be **honest** that a host can, in principle, see guest data.

The full model, including the trust matrix for each runtime, is in
[docs/architecture/tenancy-and-isolation.md](docs/architecture/tenancy-and-isolation.md).

## Controls that must stay in place

- Invited and community users run only behind strong isolation: VMs, Kata, or Linux containers in their own VM. Native macOS execution and plain runc are for owners only.
- No GPU time-slicing between different users. On a Mac, an untrusted user gets the whole GPU or none of it.
- Guest workloads can't reach the host's files or LAN. Community workloads have no internet by default.
- Sharing a machine is opt-in by its owner, and the owner can reclaim it instantly.
- Model weights must be code-free: safetensors or GGUF, with their headers checked by `einsora-ml validate`. A model's chat template (in `tokenizer_config.json`, or inside a GGUF file) is a Jinja template the model server renders in a sandboxed engine; the validator doesn't examine it. `trust_remote_code` is never enabled.
- In a datacenter, tenant pods don't mount a ServiceAccount token, and users never get Kubernetes API access.
- The API has no runtime credentials. Agents use per-machine mTLS certificates that can be revoked.
- No secrets in git.

## Supported versions

Pre-release. Only `main` is supported.
