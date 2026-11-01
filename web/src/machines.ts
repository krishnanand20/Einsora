import type { Machine } from "./api/types";

/** How long ago a machine was seen, in words: "just now", "40 s ago", "5 min ago", "3 h ago", "2 days ago". */
export function seenAgo(lastSeen: string | undefined, now: Date): string {
  if (!lastSeen) return "never";
  const seconds = Math.max(0, Math.round((now.getTime() - new Date(lastSeen).getTime()) / 1000));
  if (seconds < 10) return "just now";
  if (seconds < 60) return `${seconds} s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} min ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return `${hours} h ago`;
  return `${Math.floor(hours / 24)} days ago`;
}

/** Memory in GiB, as people say it: "36 GiB", or "unknown" before the agent reports. */
export function memoryShown(mib: number): string {
  return mib > 0 ? `${Math.round(mib / 1024)} GiB` : "unknown";
}

/** A machine's GPUs: "1 × Apple M3 Pro", "2 × NVIDIA L40S (48 GiB each)", or "none". */
export function gpusShown(m: Pick<Machine, "accelerators">): string {
  if (m.accelerators.length === 0) return "none";
  return m.accelerators
    .map((a) => {
      const each = a.kind !== "apple" && a.memory_mib > 0 ? ` (${memoryShown(a.memory_mib)} each)` : "";
      return `${a.count} × ${a.model}${each}`;
    })
    .join(", ");
}

/** The system a machine runs: "macOS, Apple Silicon" or "Linux, x86-64". */
export function systemShown(m: Pick<Machine, "os" | "arch">): string {
  const os = m.os === "darwin" ? "macOS" : "Linux";
  const arch = m.os === "darwin" && m.arch === "arm64" ? "Apple Silicon" : m.arch === "amd64" ? "x86-64" : "Arm64";
  return `${os}, ${arch}`;
}

/** The name of the machine a workload runs on, if it's one of yours the dashboard knows. */
export function machineName(machines: Machine[] | undefined, id: string | undefined): string | undefined {
  return id ? machines?.find((m) => m.id === id)?.name : undefined;
}
