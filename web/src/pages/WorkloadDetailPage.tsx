import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef } from "react";
import { Link, useParams } from "react-router";

import { api } from "../api/client";
import { machineName } from "../machines";
import { mergeOutput, type OutputView, outputText } from "../output";
import { canOpen, classShown, describeChange, isSettling } from "../workloads";

/** How many pages of output one refresh reads at most; the next refresh reads on. */
const OUTPUT_ROUNDS = 20;
const OUTPUT_PAGE = 500;

/**
 * One workload: its complete timeline, its output, and a download of everything recorded about it
 * (ADR-0015, ADR-0024). Deleted workloads open too, so their history is never out of reach.
 */
export function WorkloadDetailPage() {
  const { id = "" } = useParams();
  const workload = useQuery({
    queryKey: ["workload", id],
    queryFn: () => api.getWorkload(id, true),
    refetchInterval: (q) => (q.state.data && isSettling(q.state.data.state) ? 2000 : false),
  });
  const machines = useQuery({ queryKey: ["machines"], queryFn: api.listMachines });
  const events = useQuery({
    queryKey: ["workload-events", id],
    queryFn: () => api.workloadEvents(id),
    refetchInterval: workload.data && isSettling(workload.data.state) ? 2000 : false,
  });
  // Output reads on from where it got to, so each refresh fetches only what's new. It refreshes
  // while the workload runs, since that's when there's new output.
  const queryClient = useQueryClient();
  const output = useQuery({
    queryKey: ["workload-output", id],
    queryFn: async (): Promise<OutputView> => {
      let view = queryClient.getQueryData<OutputView>(["workload-output", id]);
      let page = await api.workloadOutput(id, view?.next ?? 0);
      view = mergeOutput(view, page);
      for (let round = 1; round < OUTPUT_ROUNDS && page.items.length >= OUTPUT_PAGE; round++) {
        page = await api.workloadOutput(id, view.next);
        view = mergeOutput(view, page);
      }
      return view;
    },
    refetchInterval: workload.data && (workload.data.state === "running" || isSettling(workload.data.state)) ? 2000 : false,
  });
  // Keep the newest output in sight, unless the reader has scrolled up.
  const outputBox = useRef<HTMLPreElement>(null);
  const followOutput = useRef(true);
  const shownChunks = output.data?.chunks.length ?? 0;
  useEffect(() => {
    const box = outputBox.current;
    if (box && followOutput.current) box.scrollTop = box.scrollHeight;
  }, [shownChunks]);
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
  const connection = useMutation({
    mutationFn: async () => ({ id, grant: await api.openWorkload(id) }),
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
          {w.kind === "endpoint" && w.state === "running" && w.machine_id && (
            <button disabled={connection.isPending} onClick={() => connection.mutate()}>
              {connection.isPending ? "Loading…" : "API connection"}
            </button>
          )}
          <button disabled={download.isPending} onClick={() => download.mutate()}>
            {download.isPending ? "Preparing…" : "Download full log"}
          </button>
        </span>
      </div>
      {download.isError && <p className="error">{download.error.message}</p>}
      {open.isError && <p className="error">{open.error.message}</p>}
      {connection.isError && <p className="error">{connection.error.message}</p>}
      {connection.data?.id === id && w.kind === "endpoint" && w.state === "running" && (
        <div>
          <p>OpenAI-compatible base URL: <code>{connection.data.grant.url}</code></p>
          <p>
            Send an API token from <Link to="/tokens">API tokens</Link> as{" "}
            <code>Authorization: Bearer &lt;token&gt;</code>. Use <code>/models</code> to see the model name.
          </p>
        </div>
      )}
      <p className="muted">
        {w.kind} · <span className={`state state-${w.state}`}>{w.state}</span> · {classShown(w)} · region {w.region}
        {w.machine_id && (
          <>
            {" "}
            · on <Link to="/machines">{machineName(machines.data?.items, w.machine_id) ?? "a machine"}</Link>
            {w.driver && <> with {w.driver}</>}
          </>
        )}{" "}
        · id {w.id}
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

      <h2>Output</h2>
      <p className="muted">
        What it printed (stdout and stderr), with anything that looks like a credential in a URL replaced by REDACTED.
        It's kept for a limited time (14 days by default); <strong>Download full log</strong> has all of it that's kept.
      </p>
      {output.isError && <p className="error">{output.error.message}</p>}
      {output.data?.notes.map((note) => (
        <p key={note} className="muted">
          {note}
        </p>
      ))}
      {output.data && output.data.hiddenChars > 0 && (
        <p className="muted">
          Showing the most recent output. Earlier output this page leaves out is in <strong>Download full log</strong>.
        </p>
      )}
      {output.data && (output.data.collected || output.data.chunks.length > 0) && (
        <pre
          className="output"
          ref={outputBox}
          onScroll={(e) => {
            const box = e.currentTarget;
            followOutput.current = box.scrollHeight - box.scrollTop - box.clientHeight < 24;
          }}
        >
          {output.data.chunks.length === 0
            ? "No output yet."
            : output.data.chunks.map((c) => (
                <span key={c.id} className={`output-${c.stream}`}>
                  {c.stream === "einsora" ? `[einsora] ${outputText(c.text)}` : outputText(c.text)}
                </span>
              ))}
        </pre>
      )}
    </section>
  );
}
