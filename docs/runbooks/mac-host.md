# Running an Apple Silicon Mac as an Einsora host

> **Status: planned (Phases 1–2, sharing in Phase 4).** This describes how the agent will use a Mac
> ([ADR-0013](../adr/0013-node-agent-and-apple-silicon.md)). Verify each step once the agent ships.

## What a Mac can run

| Workload | How | GPU | Who may use it |
|---|---|---|---|
| Model endpoint | MLX or llama.cpp server (`macos-native`) | Metal | Owner. Invited and community users get it as **managed inference** only. |
| Notebook | Jupyter with PyTorch (MPS) and MLX (`macos-native`) | Metal | Owner only |
| Notebook (strong isolation) | Linux container in its own VM (`macos-container`) | CPU only | Owner, invited, community (opt-in) |
| VM | macOS guest (`macos-vz`) | Metal (paravirtualized) | Owner, invited, community (opt-in). Max 2 macOS VMs per Mac. |
| VM | Linux guest (`macos-vz`) | CPU only | Owner, invited, community (opt-in) |

The trust matrix in [tenancy-and-isolation.md](../architecture/tenancy-and-isolation.md) decides who may use what.

## Set up the Mac

1. **Install and join:** `curl -fsSL https://einsora.com/install.sh | sh`, or
   `brew install krishnanand20/tap/einsora`, which builds from source ([quickstart](00-quickstart.md#install)).
   Then either run `einsora up` (Solo), and `einsora service install` to keep it running at login, or
   (Phase 2) `einsora agent --join <enrolment token from your Lab's dashboard>`. The agent dials out,
   so no ports need opening.
2. **Power:**
   - Desktops (Mac mini, Mac Studio): set "Prevent automatic sleeping when the display is off" if you want it available overnight.
   - Laptops: the agent shares only while on AC power.
3. **GPU memory:** macOS caps how much unified memory the GPU may use. The agent reports what's usable and keeps headroom for you. Raising the cap is an expert setting (`sysctl iogpu.wired_limit_mb`); the agent never changes it for you.
4. **Disk:** keep enough free space for models (often 5–80 GB each). The agent caches each model once and shares it between workloads.

## Sharing this Mac (Phase 4)

- Default: **private**.
- Invite-only or community sharing is set per machine in the dashboard. You can also set:
  - an idle time (for example 15 minutes with no input)
  - a schedule
  - caps for GPU memory, CPU, and disk
- When you come back, guest workloads stop within seconds.
- Community users never run native code on your Mac. They get managed inference or a VM, no access to your files or home network, and no internet by default.
- **You can see guest data on your own machine.** Einsora tells guests this before they launch anything.
- You pay for the electricity. An M-series Mac uses far less power than a GPU server, but check it anyway ([power notes](../hardware/power-and-cooling.md#hosting-at-home)).

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Endpoint fails to start, "out of memory" | Model too large for the GPU's share of unified memory. Use a smaller or more quantized model. |
| Machine shows offline | It's asleep, or the network changed. The agent reconnects by itself. |
| Third macOS VM won't start | The macOS licence allows 2 macOS VMs per Mac |
