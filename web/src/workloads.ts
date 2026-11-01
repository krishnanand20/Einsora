import type { Accelerator, CatalogImage, GpuClass, Workload, WorkloadEvent, WorkloadState } from "./api/types";

export type Action = "stop" | "start" | "delete";

/** Which buttons to show. Mirrors the checks in controlplane/internal/service/service.go. */
export function availableActions(state: WorkloadState): Action[] {
  const actions: Action[] = [];
  if (state === "provisioning" || state === "running") actions.push("stop");
  if (state === "stopped" || state === "failed") actions.push("start");
  if (state !== "deleting" && state !== "deleted") actions.push("delete");
  return actions;
}

/** Whether the dashboard can open a workload in the browser: a running notebook on a machine (ADR-0017). */
export function canOpen(w: Pick<Workload, "kind" | "state" | "machine_id">): boolean {
  return w.kind === "notebook" && w.state === "running" && !!w.machine_id;
}

/** States where the controller is still working, so the list should poll. */
export function isSettling(state: WorkloadState): boolean {
  return ["pending", "queued", "provisioning", "stopping", "deleting"].includes(state);
}

/** How a timeline entry reads: a state change, or a note that leaves the state as it was. */
export function describeChange(e: WorkloadEvent): string {
  if (e.from === e.to) return `still ${e.to}`;
  return `${e.from ?? "∅"} → ${e.to}`;
}

/**
 * What a workload runs on (ADR-0010): the placed class, saying so when it differs from the class
 * requested (an opt-in fallback), or the requested class before it's admitted. Never hide a fallback.
 */
export function classShown(w: Pick<Workload, "gpu_class" | "gpu_count" | "placed_class">): string {
  const placed = w.placed_class ?? w.gpu_class;
  const shown = `${w.gpu_count} × ${placed}`;
  return placed === w.gpu_class ? shown : `${shown} (requested ${w.gpu_class})`;
}

/** The runtime an image uses on an accelerator: the first that supports it. Mirrors catalog.Image.RuntimeFor. */
export function runtimeFor(image: CatalogImage | undefined, accelerator: Accelerator) {
  return image?.runtimes.find((r) => r.accelerators.includes(accelerator));
}

/**
 * The classes a launch can use: offered in the region, able to host VMs if it's a VM, and with a
 * runtime for the image. The API checks the same; this only keeps impossible choices off the form.
 */
export function launchableClasses(
  classes: GpuClass[],
  image: CatalogImage | undefined,
  region: string,
): GpuClass[] {
  return classes.filter(
    (g) =>
      g.regions.includes(region) &&
      (image?.kind !== "vm" || g.supports_vm) &&
      runtimeFor(image, g.accelerator) !== undefined,
  );
}
