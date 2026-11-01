import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Link } from "react-router";

import { api } from "../api/client";
import type { Workload } from "../api/types";
import { machineName } from "../machines";
import { type Action, availableActions, classShown, isSettling } from "../workloads";

const run: Record<Action, (id: string) => Promise<Workload>> = {
  stop: api.stopWorkload,
  start: api.startWorkload,
  delete: api.deleteWorkload,
};

export function WorkloadsPage() {
  const queryClient = useQueryClient();
  const [showDeleted, setShowDeleted] = useState(false);
  const workloads = useQuery({
    queryKey: ["workloads", showDeleted],
    queryFn: () => api.listWorkloads(showDeleted),
    // Poll while anything is mid-transition; the controller does the work asynchronously.
    refetchInterval: (q) => (q.state.data?.items.some((w) => isSettling(w.state)) ? 2000 : false),
  });
  const machines = useQuery({ queryKey: ["machines"], queryFn: api.listMachines });
  const action = useMutation({
    mutationFn: ({ id, act }: { id: string; act: Action }) => run[act](id),
    onSettled: () => queryClient.invalidateQueries({ queryKey: ["workloads"] }),
  });

  if (workloads.isPending) return <p>Loading…</p>;
  if (workloads.isError) return <p className="error">{workloads.error.message}</p>;

  const items = workloads.data.items;
  return (
    <section>
      <div className="section-head">
        <h1>Workloads</h1>
        <div className="head-actions">
          <label className="muted">
            <input type="checkbox" checked={showDeleted} onChange={(e) => setShowDeleted(e.target.checked)} /> Show
            deleted
          </label>
          <Link className="button" to="/launch">
            Launch
          </Link>
        </div>
      </div>
      {action.isError && <p className="error">{action.error.message}</p>}
      {items.length === 0 ? (
        <p className="muted">
          Nothing running yet. <Link to="/launch">Launch</Link> a GPU VM, a notebook, or a model endpoint.
        </p>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th>Kind</th>
                <th>State</th>
                <th>Runs on</th>
                <th>Region</th>
                <th>Created</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {items.map((w) => (
                <tr key={w.id}>
                  <td>
                    <Link to={`/workloads/${w.id}`}>{w.name}</Link>
                  </td>
                  <td>{w.kind}</td>
                  <td>
                    <span className={`state state-${w.state}`}>{w.state}</span>
                  </td>
                  <td>
                    {classShown(w)}
                    {machineName(machines.data?.items, w.machine_id) && (
                      <span className="muted"> on {machineName(machines.data?.items, w.machine_id)}</span>
                    )}
                  </td>
                  <td>{w.region}</td>
                  <td>{new Date(w.created_at).toLocaleString()}</td>
                  <td className="actions">
                    {availableActions(w.state).map((act) => (
                      <button
                        key={act}
                        className={act === "delete" ? "danger" : undefined}
                        disabled={action.isPending}
                        onClick={() => action.mutate({ id: w.id, act })}
                      >
                        {act}
                      </button>
                    ))}
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
