# Power and cooling

## Hosting at home

When you run TensoraFi on a personal machine, or share it ([ADR-0014](../adr/0014-sharing-idle-compute.md)),
**you pay for its electricity**, and your room gets its heat.

| Machine | Typical draw under GPU load | Notes |
|---|---|---|
| Mac mini (Apple Silicon) | Tens of watts | Very efficient; fine to leave on |
| MacBook (Apple Silicon) | Up to the charger's rating | Shares only on AC power; heavy sustained load warms the battery |
| Mac Studio (Max or Ultra) | Up to roughly 300 W | Check Apple's published figures for your model |
| Desktop with an RTX 4090 or 5090 | 450–575 W for the GPU, ~600–800 W for the whole system | Loud and hot under sustained load |

```
monthly kWh ≈ average watts while busy × hours busy per month ÷ 1000
```

For example, a 600 W desktop shared 8 hours a night uses about 600 × 240 ÷ 1000 ≈ 144 kWh a month.
Multiply by your electricity rate to see what sharing costs you. Then set caps and schedules in the
dashboard to match (power caps are planned).

- **Heat and noise:** set a schedule (for example, overnight only) for machines in living spaces.
- **Laptops:** keep them on a hard surface, and let TensoraFi share only on AC power (the default).
- **Power cuts:** a small UPS on the control plane machine protects its store.

## Datacenter

Power is often the real bottleneck, before GPUs or money. Plan for grid instability, high ambient
temperatures, and slow utility upgrades.

### Rough draw per node

These are board-power (TDP) figures. Measure your real draw with the BMC under burn-in.

| Config | GPUs | GPU power | Rest of system | Plan for |
|---|---|---|---|---|
| 4× L40S server | 4 × 350 W | 1.4 kW | ~0.8 kW | **2.5–3 kW** |
| 8× RTX 4090/5090 server | 8 × 450–575 W | 3.6–4.6 kW | ~1 kW | **5–6 kW** |
| 4× A100 80GB PCIe | 4 × 300 W | 1.2 kW | ~0.8 kW | **2.5 kW** |
| HGX 8× H100 SXM | 8 × 700 W | 5.6 kW | ~3–4 kW | **~10.2 kW** max system |

GPUs spike above their TDP for milliseconds, so PSUs and UPSes need headroom.

### UPS sizing

```
UPS kVA ≥ (sum of planned kW) ÷ power factor (≈0.9) × 1.25 headroom
```

One 4× L40S node plus a switch and router comes to about 3.3 kW, which needs a UPS of about **4.6 kVA** or more.

- Use an **online double-conversion** UPS for GPU servers.
- **Runtime:** batteries cover the time until a generator takes over, plus a clean shutdown.
- **Generator:** needed for anything others depend on. Test it under load monthly.
- **Wiring:** three-phase supply above about 5 kW, with separate A and B feeds to dual PSUs.
- **Monitoring:** UPS network cards (SNMP) and BMC power readings, with a graceful drain at a battery threshold.

### Cooling

- Each kW of IT load is about 3,412 BTU/h of heat. A 3 kW node needs roughly 1 ton of cooling by itself.
- Keep server inlet air at or below 27 °C (the ASHRAE recommended range). GPUs throttle and fail early when inlet air is hot.
- Passive datacenter GPUs depend on server fans and front-to-back airflow, so use blanking panels and hot/cold aisle separation.
- Watch GPU temperature and clock-throttle reasons through DCGM.

### Colocation versus on-premises

For a datacenter deployment that others depend on, **colocation in a Tier III facility** is usually
easier than building reliable power and cooling yourself. You get dual utility feeds, a generator,
UPS, N+1 cooling, carrier-neutral connectivity, and remote hands. On-premises suits labs and internal
setups with modest power needs.

### Knowing your running cost

```
monthly kWh ≈ average kW × 24 × 30 × PUE (colocation ~1.5, office ~2+)
```

A node averaging 2.5 kW uses about 2.5 × 720 × 1.5 ≈ 2,700 kWh a month. This is a cost of running
the hardware, not something TensoraFi charges for: TensoraFi is free.
