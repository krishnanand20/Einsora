import { useMutation, useQueryClient } from "@tanstack/react-query";
import type { FormEvent } from "react";
import { useNavigate, useSearchParams } from "react-router";

import { api, safeNext } from "../api/client";

/** Sign in with a local account (ADR-0012). The session is an HttpOnly cookie; nothing is stored in the page. */
export function LoginPage() {
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const queryClient = useQueryClient();
  const login = useMutation({
    mutationFn: ({ email, password }: { email: string; password: string }) => api.login(email, password),
    onSuccess: async () => {
      await queryClient.invalidateQueries();
      navigate(safeNext(params.get("next")), { replace: true });
    },
  });

  function onSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const f = new FormData(e.currentTarget);
    login.mutate({ email: String(f.get("email")), password: String(f.get("password")) });
  }

  return (
    <section className="narrow">
      <h1>Sign in</h1>
      <form className="form" onSubmit={onSubmit}>
        <label>
          Email
          <input name="email" type="email" autoComplete="username" required autoFocus />
        </label>
        <label>
          Password
          <input name="password" type="password" autoComplete="current-password" required />
        </label>
        {login.isError && <p className="error">{login.error.message}</p>}
        <button type="submit" disabled={login.isPending}>
          {login.isPending ? "Signing in…" : "Sign in"}
        </button>
      </form>
      <p className="muted">
        No account yet? The machine's owner creates the first one with <code>tensorafi setup</code>.
      </p>
    </section>
  );
}
