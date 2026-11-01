import { useQuery } from "@tanstack/react-query";

import { api } from "../api/client";
import { gpusShown, memoryShown, seenAgo, systemShown } from "../machines";

/**
 * Your machines: what each has, whether it's online, and how many of your workloads it holds. A
 * machine joins when Einsora runs on it; its status comes from when its agent was last seen.
 */
export function MachinesPage() {
  const machines = useQuery({
    queryKey: ["machines"],
    queryFn: api.listMachines,
    refetchInterval: 30_000, // statuses change as agents come and go
  });

  if (machines.isPending) return <p>Loading…</p>;
  if (machines.isError) return <p className="error">{machines.error.message}</p>;

  const now = new Date();
  const items = machines.data.items;
  return (
    <section>
      <div className="section-head">
        <h1>Machines</h1>
      </div>
      <p className="muted">
        The machines your workloads run on. A machine is online when its agent was seen in the last 3 minutes. Every
        machine is private to you; sharing with others comes later.
      </p>
      {items.length === 0 ? (
        <p className="muted">
          No machines yet. Running <code>einsora up</code> on a Mac or Linux PC registers it here.
        </p>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th>Status</th>
                <th>System</th>
                <th>CPUs</th>
                <th>Memory</th>
                <th>GPUs</th>
                <th>Region</th>
                <th>Your workloads</th>
                <th>Last seen</th>
              </tr>
            </thead>
            <tbody>
              {items.map((m) => (
                <tr key={m.id} id={`machine-${m.id}`}>
                  <td>{m.name}</td>
                  <td>
                    <span className={m.status === "online" ? "state state-running" : "state state-stopped"}>
                      {m.status}
                    </span>
                  </td>
                  <td>{systemShown(m)}</td>
                  <td>{m.cpus || "unknown"}</td>
                  <td>{memoryShown(m.memory_mib)}</td>
                  <td>{gpusShown(m)}</td>
                  <td>{m.region}</td>
                  <td>{m.workloads}</td>
                  <td title={m.last_seen_at ? new Date(m.last_seen_at).toLocaleString() : undefined}>
                    {seenAgo(m.last_seen_at, now)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
