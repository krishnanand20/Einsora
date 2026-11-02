import { describe, expect, it } from "vitest";

import { filenameFromDisposition, safeNext, toApiError, workloadPath } from "./api/client";
import type { CatalogImage, GpuClass } from "./api/types";
import {
  availableActions,
  canOpen,
  classShown,
  describeChange,
  isSettling,
  launchableClasses,
  modelField,
  runtimeFor,
} from "./workloads";

describe("availableActions", () => {
  it("matches the control plane's lifecycle rules", () => {
    expect(availableActions("running")).toEqual(["stop", "delete"]);
    expect(availableActions("stopped")).toEqual(["start", "delete"]);
    expect(availableActions("failed")).toEqual(["start", "delete"]);
    expect(availableActions("pending")).toEqual(["delete"]);
    expect(availableActions("deleting")).toEqual([]);
  });
});

describe("isSettling", () => {
  it("polls only while the controller is working", () => {
    expect(isSettling("provisioning")).toBe(true);
    expect(isSettling("running")).toBe(false);
  });
});

describe("canOpen", () => {
  it("opens only running notebooks that are on a machine", () => {
    const m = "6f1c2d3e-0000-4000-8000-000000000001";
    expect(canOpen({ kind: "notebook", state: "running", machine_id: m })).toBe(true);
    expect(canOpen({ kind: "notebook", state: "provisioning", machine_id: m })).toBe(false);
    expect(canOpen({ kind: "notebook", state: "running" })).toBe(false);
    expect(canOpen({ kind: "endpoint", state: "running", machine_id: m })).toBe(false);
    expect(canOpen({ kind: "vm", state: "running", machine_id: m })).toBe(false);
  });
});

describe("toApiError", () => {
  it("reads the API error envelope", () => {
    const err = toApiError(409, { error: { code: "quota_exceeded", message: "l40s quota" } });
    expect(err.code).toBe("quota_exceeded");
    expect(err.message).toBe("l40s quota");
  });

  it("falls back for non-API responses", () => {
    expect(toApiError(502, undefined).message).toContain("502");
  });

  it("shows the request ID so users can find the exact log lines", () => {
    const err = toApiError(500, { error: { code: "internal", message: "internal error", request_id: "abc-123" } });
    expect(err.requestId).toBe("abc-123");
    expect(err.message).toContain("request ID abc-123");
    expect(toApiError(502, undefined, "from-header").requestId).toBe("from-header");
  });
});

describe("filenameFromDisposition", () => {
  it("uses the server's file name", () => {
    expect(filenameFromDisposition("attachment; filename=einsora-llm-1.json", "x.json")).toBe("einsora-llm-1.json");
    expect(filenameFromDisposition('attachment; filename="a b.json"', "x.json")).toBe("a b.json");
  });

  it("never lets the server choose a directory", () => {
    expect(filenameFromDisposition('attachment; filename="../../etc/evil.json"', "x.json")).toBe("evil.json");
  });

  it("falls back when there's no header", () => {
    expect(filenameFromDisposition(null, "fallback.json")).toBe("fallback.json");
  });
});

describe("workloadPath", () => {
  it("encodes the ID so it can't reach another API path", () => {
    expect(workloadPath("abc", "/events")).toBe("/v1/workloads/abc/events");
    expect(workloadPath("../../v1/catalog?", "/export")).toBe("/v1/workloads/..%2F..%2Fv1%2Fcatalog%3F/export");
  });
});

describe("describeChange", () => {
  it("shows state changes and notes", () => {
    expect(describeChange({ id: 1, to: "pending", reason: "created", at: "" })).toBe("∅ → pending");
    expect(describeChange({ id: 2, from: "pending", to: "queued", reason: "waiting", at: "" })).toBe("pending → queued");
    expect(describeChange({ id: 3, from: "pending", to: "pending", reason: "retrying", at: "" })).toBe("still pending");
  });
});

describe("filenameFromDisposition with bad encoding", () => {
  it("falls back instead of throwing", () => {
    expect(filenameFromDisposition("attachment; filename*=UTF-8''%E0%A4%A", "x.json")).toBe("x.json");
  });
});

describe("classShown", () => {
  it("shows what actually runs, and never hides a fallback", () => {
    expect(classShown({ gpu_class: "l40s-48gb", gpu_count: 2 })).toBe("2 × l40s-48gb");
    expect(classShown({ gpu_class: "l40s-48gb", gpu_count: 2, placed_class: "l40s-48gb" })).toBe("2 × l40s-48gb");
    expect(classShown({ gpu_class: "l40s-48gb", gpu_count: 1, placed_class: "cpu-16c-64g" })).toBe(
      "1 × cpu-16c-64g (requested l40s-48gb)",
    );
  });
});

describe("launchableClasses", () => {
  const cls = (id: string, accelerator: GpuClass["accelerator"], regions: string[], supports_vm = false): GpuClass => ({
    id,
    display_name: id,
    accelerator,
    vram_gb: 1,
    supports_vm,
    regions,
  });
  const classes = [cls("l40s", "nvidia", ["local"], true), cls("mac", "apple", ["local"]), cls("fra", "nvidia", ["eu-fra-1"])];
  const endpoint: CatalogImage = {
    id: "ep",
    display_name: "ep",
    kind: "endpoint",
    description: "",
    runtimes: [
      { accelerators: ["nvidia"], server: "vllm" },
      { accelerators: ["apple"], server: "mlx" },
    ],
  };
  const vm: CatalogImage = { ...endpoint, kind: "vm", runtimes: [{ accelerators: ["nvidia"] }] };

  it("offers only what can run the image in the region", () => {
    expect(launchableClasses(classes, endpoint, "local").map((g) => g.id)).toEqual(["l40s", "mac"]);
    expect(launchableClasses(classes, endpoint, "eu-fra-1").map((g) => g.id)).toEqual(["fra"]);
    expect(launchableClasses(classes, vm, "local").map((g) => g.id)).toEqual(["l40s"]);
  });

  it("names the server for each accelerator", () => {
    expect(runtimeFor(endpoint, "apple")?.server).toBe("mlx");
    expect(runtimeFor(endpoint, "cpu")).toBeUndefined();
  });

  it("asks for a model in the format the server loads", () => {
    expect(modelField("gguf").placeholder).toMatch(/^hf:\/\/[^:]+:Q8_0$/);
    expect(modelField("gguf").label).toContain("quantization");
    expect(modelField("safetensors").label).toBe("Model (safetensors)");
    expect(modelField(undefined).label).toBe("Model (safetensors)");
  });
});

describe("safeNext", () => {
  it("returns only paths inside the dashboard", () => {
    expect(safeNext("/workloads/abc?x=1")).toBe("/workloads/abc?x=1");
    expect(safeNext(null)).toBe("/");
    expect(safeNext("https://evil.example")).toBe("/");
    expect(safeNext("//evil.example")).toBe("/");
    expect(safeNext("/\\evil.example")).toBe("/");
  });
});
