import { useMutation, useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router";

import { api } from "../api/client";
import { canOpen, classShown, describeChange, isSettling } from "../workloads";

/**
 * One workload: its complete timeline, and a download of everything recorded about it (ADR-0015).
 * Deleted workloads open too, so their history is never out of reach.
 */
export function WorkloadDetailPage() {
  const { id = "" } = useParams();
  const workload = useQuery({
    queryKey: ["workload", id],
    queryFn: () => api.getWorkload(id, true),
    refetchInterval: (q) => (q.state.data && isSettling(q.state.data.state) ? 2000 : false),
  });
  const events = useQuery({
    queryKey: ["workload-events", id],
    queryFn: () => api.workloadEvents(id),
    refetchInterval: workload.data && isSettling(workload.data.state) ? 2000 : false,
  });
  const download = useMutation({
    mutationFn: () => api.exportWorkload(id),
    onSuccess: ({ blob, filename }) => {
      const url = URL.createObjectURL(blob);
      const link = document.createElement("a");
      link.href = url;
      link.download = filename;
      link.click();
      URL.revokeObjectURL(url);
    },
  });

  // The tab opens on the click, so it isn't blocked as a popup, and is pointed at the one-time URL
  // once the API answers. It loses its link back here: the notebook can't steer the dashboard.
  const open = useMutation({
    mutationFn: async () => {
      const tab = window.open("about:blank", "_blank");
      try {
        const grant = await api.openWorkload(id);
        if (tab) {
          tab.opener = null;
          tab.location.href = grant.url;
        }
      } catch (err) {
        tab?.close();
        throw err;
      }
    },
  });

  if (workload.isPending) return <p>Loading…</p>;
  if (workload.isError) return <p className="error">{workload.error.message}</p>;
  const w = workload.data;

  return (
    <section>
      <p>
        <Link to="/">← Workloads</Link>
      </p>
      <div className="section-head">
        <h1>{w.name}</h1>
        <span className="actions">
          {canOpen(w) && (
            <button disabled={open.isPending} onClick={() => open.mutate()}>
              {open.isPending ? "Opening…" : "Open"}
            </button>
          )}
          <button disabled={download.isPending} onClick={() => download.mutate()}>
            {download.isPending ? "Preparing…" : "Download full log"}
          </button>
        </span>
      </div>
      {download.isError && <p className="error">{download.error.message}</p>}
      {open.isError && <p className="error">{open.error.message}</p>}
      <p className="muted">
        {w.kind} · <span className={`state state-${w.state}`}>{w.state}</span> · {classShown(w)} · region {w.region} · id{" "}
        {w.id}
      </p>
      {w.state === "deleted" && (
        <p className="muted">This workload is deleted. Its timeline and full log stay available here.</p>
      )}

      <h2>Timeline</h2>
      <p className="muted">Every state change and note, when, and why. Nothing is filtered out.</p>
      {events.isError && <p className="error">{events.error.message}</p>}
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Time</th>
              <th>Change</th>
              <th>Reason</th>
            </tr>
          </thead>
          <tbody>
            {(events.data?.items ?? []).map((e) => (
              <tr key={e.id}>
                <td>{new Date(e.at).toLocaleString()}</td>
                <td>{describeChange(e)}</td>
                <td className="reason">{e.reason}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}
