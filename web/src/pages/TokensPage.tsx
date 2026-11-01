import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, useState } from "react";

import { api } from "../api/client";
import type { CreatedApiToken } from "../api/types";

const expiries = [
  { days: 30, label: "30 days" },
  { days: 90, label: "90 days" },
  { days: 365, label: "1 year" },
  { days: 0, label: "Never" },
];

function when(at?: string) {
  return at ? new Date(at).toLocaleString() : "—";
}

/** API tokens for the CLI (TENSORAFI_TOKEN) and scripts. A new token is shown once and never again. */
export function TokensPage() {
  const queryClient = useQueryClient();
  const tokens = useQuery({ queryKey: ["tokens"], queryFn: api.listTokens });
  const [created, setCreated] = useState<CreatedApiToken | null>(null);
  const create = useMutation({
    mutationFn: ({ name, days, password }: { name: string; days: number; password: string }) =>
      api.createToken(name, days, password),
    onSuccess: (t) => {
      setCreated(t);
      void queryClient.invalidateQueries({ queryKey: ["tokens"] });
    },
  });
  const revoke = useMutation({
    mutationFn: api.revokeToken,
    onSettled: () => queryClient.invalidateQueries({ queryKey: ["tokens"] }),
  });

  function onSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const f = new FormData(e.currentTarget);
    create.mutate({
      name: String(f.get("name")).trim(),
      days: Number(f.get("days")),
      password: String(f.get("password")),
    });
    e.currentTarget.reset();
  }

  return (
    <section>
      <h1>API tokens</h1>
      <p className="muted">
        For the <code>tensorafi</code> CLI (<code>TENSORAFI_TOKEN</code>) and scripts. A token acts as you, so treat it
        like a password, and revoke it when you no longer need it.
      </p>

      {created && (
        <div className="notice">
          <p>
            <strong>Copy your new token now.</strong> It won't be shown again.
          </p>
          <code className="secret">{created.token}</code>
          <p>
            <button onClick={() => void navigator.clipboard?.writeText(created.token)}>Copy</button>{" "}
            <button onClick={() => setCreated(null)}>Done</button>
          </p>
        </div>
      )}

      <form className="form" onSubmit={onSubmit}>
        <div className="row">
          <label>
            Name
            <input name="name" required maxLength={64} placeholder="laptop" />
          </label>
          <label>
            Expires
            <select name="days" defaultValue={90}>
              {expiries.map((e) => (
                <option key={e.days} value={e.days}>
                  {e.label}
                </option>
              ))}
            </select>
          </label>
          <label>
            Your password
            <input name="password" type="password" required autoComplete="current-password" />
          </label>
        </div>
        {create.isError && <p className="error">{create.error.message}</p>}
        <button type="submit" disabled={create.isPending}>
          Create token
        </button>
      </form>

      {tokens.isError && <p className="error">{tokens.error.message}</p>}
      {revoke.isError && <p className="error">{revoke.error.message}</p>}
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Token</th>
              <th>Created</th>
              <th>Expires</th>
              <th>Last used</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {(tokens.data?.items ?? []).map((t) => (
              <tr key={t.id}>
                <td>{t.name}</td>
                <td>
                  <code>{t.prefix}…</code>
                </td>
                <td>{when(t.created_at)}</td>
                <td>{t.expires_at ? when(t.expires_at) : "never"}</td>
                <td>{when(t.last_used_at)}</td>
                <td className="actions">
                  {t.revoked_at ? (
                    <span className="muted">revoked {when(t.revoked_at)}</span>
                  ) : (
                    <button className="danger" disabled={revoke.isPending} onClick={() => revoke.mutate(t.id)}>
                      revoke
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}
