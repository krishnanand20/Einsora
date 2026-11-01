import { describe, expect, it } from "vitest";

import type { Machine } from "./api/types";
import { gpusShown, machineName, memoryShown, seenAgo, systemShown } from "./machines";

const now = new Date("2026-11-01T12:00:00Z");
const ago = (seconds: number) => new Date(now.getTime() - seconds * 1000).toISOString();

describe("seenAgo", () => {
  it("says how long ago, in words", () => {
    expect(seenAgo(undefined, now)).toBe("never");
    expect(seenAgo(ago(3), now)).toBe("just now");
    expect(seenAgo(ago(40), now)).toBe("40 s ago");
    expect(seenAgo(ago(5 * 60), now)).toBe("5 min ago");
    expect(seenAgo(ago(3 * 3600), now)).toBe("3 h ago");
    expect(seenAgo(ago(3 * 86400), now)).toBe("3 days ago");
    expect(seenAgo(ago(-30), now)).toBe("just now"); // a clock a little ahead never reads as negative
  });
});

describe("machine facts", () => {
  it("shows memory, GPUs, and the system as people say them", () => {
    expect(memoryShown(36 * 1024)).toBe("36 GiB");
    expect(memoryShown(0)).toBe("unknown");
    expect(gpusShown({ accelerators: [] })).toBe("none");
    expect(gpusShown({ accelerators: [{ kind: "apple", model: "Apple M3 Pro", count: 1, memory_mib: 36864 }] })).toBe(
      "1 × Apple M3 Pro",
    );
    expect(gpusShown({ accelerators: [{ kind: "nvidia", model: "NVIDIA L40S", count: 2, memory_mib: 46068 }] })).toBe(
      "2 × NVIDIA L40S (45 GiB each)",
    );
    expect(systemShown({ os: "darwin", arch: "arm64" })).toBe("macOS, Apple Silicon");
    expect(systemShown({ os: "linux", arch: "amd64" })).toBe("Linux, x86-64");
    expect(systemShown({ os: "linux", arch: "arm64" })).toBe("Linux, Arm64");
  });

  it("finds a workload's machine by ID", () => {
    const machines = [{ id: "m1", name: "studio" }] as Machine[];
    expect(machineName(machines, "m1")).toBe("studio");
    expect(machineName(machines, "m2")).toBeUndefined();
    expect(machineName(machines, undefined)).toBeUndefined();
    expect(machineName(undefined, "m1")).toBeUndefined();
  });
});
