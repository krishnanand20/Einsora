import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, useState } from "react";
import { useNavigate } from "react-router";

import { api } from "../api/client";
import type { CreateWorkload, WorkloadKind } from "../api/types";
import { launchableClasses, modelField, runtimeFor } from "../workloads";

const kinds: { value: WorkloadKind; label: string }[] = [
  { value: "endpoint", label: "Model endpoint" },
  { value: "notebook", label: "Notebook" },
  { value: "vm", label: "GPU VM" },
];

export function LaunchPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const catalog = useQuery({ queryKey: ["catalog"], queryFn: api.catalog });
  const [kind, setKind] = useState<WorkloadKind>("endpoint");
  const [imageId, setImageId] = useState("");
  const [region, setRegion] = useState("");
  const [classId, setClassId] = useState("");
  const create = useMutation({
    mutationFn: api.createWorkload,
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["workloads"] });
      navigate("/");
    },
  });

  if (catalog.isPending) return <p>Loading…</p>;
  if (catalog.isError) return <p className="error">{catalog.error.message}</p>;

  const { regions } = catalog.data;
  const images = catalog.data.images.filter((i) => i.kind === kind);
  const image = images.find((i) => i.id === imageId) ?? images[0];
  const regionId = region || regions[0]?.id || "";
  const gpus = launchableClasses(catalog.data.gpu_classes, image, regionId);
  const gpu = gpus.find((g) => g.id === classId) ?? gpus[0];
  const runtime = gpu && kind === "endpoint" ? runtimeFor(image, gpu.accelerator) : undefined;
  const server = runtime?.server;
  const model = modelField(runtime?.model_format);

  function onSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const f = new FormData(e.currentTarget);
    const input: CreateWorkload = {
      name: String(f.get("name")),
      kind,
      image_id: image?.id ?? "",
      gpu_class: gpu?.id ?? "",
      gpu_count: Number(f.get("gpu_count")),
      region: regionId,
    };
    if (kind === "vm") input.ssh_public_key = String(f.get("ssh_public_key")).trim();
    if (kind === "endpoint") input.model_uri = String(f.get("model_uri")).trim();
    create.mutate(input);
  }

  return (
    <section>
      <h1>Launch</h1>
      <form className="form" onSubmit={onSubmit}>
        <label>
          Type
          <select
            value={kind}
            onChange={(e) => {
              setKind(e.target.value as WorkloadKind);
              setImageId("");
              setClassId("");
            }}
          >
            {kinds.map((k) => (
              <option key={k.value} value={k.value}>
                {k.label}
              </option>
            ))}
          </select>
        </label>
        <label>
          Name
          <input name="name" required pattern="[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?" placeholder="my-llm" />
        </label>
        {regions.length > 1 && (
          <label>
            Region
            <select value={regionId} onChange={(e) => setRegion(e.target.value)}>
              {regions.map((r) => (
                <option key={r.id} value={r.id}>
                  {r.display_name} ({r.id})
                </option>
              ))}
            </select>
          </label>
        )}
        <label>
          Image
          <select value={image?.id ?? ""} onChange={(e) => setImageId(e.target.value)}>
            {images.map((i) => (
              <option key={i.id} value={i.id} title={i.description}>
                {i.display_name}
              </option>
            ))}
          </select>
        </label>
        <div className="row">
          <label>
            Runs on
            <select value={gpu?.id ?? ""} onChange={(e) => setClassId(e.target.value)}>
              {gpus.map((g) => (
                <option key={g.id} value={g.id}>
                  {g.display_name}
                </option>
              ))}
            </select>
          </label>
          <label>
            Count
            <input name="gpu_count" type="number" min={1} max={8} defaultValue={1} />
          </label>
        </div>
        {gpus.length === 0 && <p className="muted">Nothing in this region can run this image.</p>}
        {server && <p className="muted">Served by {server} on this hardware.</p>}
        {kind === "vm" && (
          <label>
            SSH public key
            <textarea name="ssh_public_key" required rows={3} placeholder="ssh-ed25519 AAAA… you@laptop" />
          </label>
        )}
        {kind === "endpoint" && (
          <label>
            {model.label}
            <input name="model_uri" required placeholder={model.placeholder} />
          </label>
        )}
        {create.isError && <p className="error">{create.error.message}</p>}
        <button type="submit" disabled={create.isPending || !gpu}>
          {create.isPending ? "Launching…" : "Launch"}
        </button>
      </form>
    </section>
  );
}
