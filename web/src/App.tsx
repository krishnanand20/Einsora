import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Navigate, NavLink, Route, Routes, useLocation, useNavigate } from "react-router";

import { api, ApiError } from "./api/client";
import { LaunchPage } from "./pages/LaunchPage";
import { LoginPage } from "./pages/LoginPage";
import { TokensPage } from "./pages/TokensPage";
import { WorkloadDetailPage } from "./pages/WorkloadDetailPage";
import { WorkloadsPage } from "./pages/WorkloadsPage";

export function App() {
  const location = useLocation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const me = useQuery({ queryKey: ["me"], queryFn: api.me, retry: false });
  const logout = useMutation({
    mutationFn: api.logout,
    onSettled: () => {
      queryClient.clear();
      navigate("/login");
    },
  });

  const signedOut = me.error instanceof ApiError && me.error.status === 401;
  if (location.pathname === "/login") {
    return (
      <div className="shell">
        <header className="topbar">
          <span className="brand">TensoraFi</span>
        </header>
        <main>
          <LoginPage />
        </main>
      </div>
    );
  }
  if (signedOut) {
    return <Navigate to={`/login?next=${encodeURIComponent(location.pathname + location.search)}`} replace />;
  }
  const local = me.data?.auth_mode === "local";

  return (
    <div className="shell">
      <header className="topbar">
        <span className="brand">TensoraFi</span>
        <nav>
          <NavLink to="/" end>
            Workloads
          </NavLink>
          <NavLink to="/launch">Launch</NavLink>
          {local && <NavLink to="/tokens">API tokens</NavLink>}
        </nav>
        {me.data && (
          <span className="who muted">
            {me.data.email ?? me.data.subject} · {me.data.tenant}
            {local && (
              <button className="link" onClick={() => logout.mutate()} disabled={logout.isPending}>
                Sign out
              </button>
            )}
          </span>
        )}
      </header>
      <main>
        <Routes>
          <Route path="/" element={<WorkloadsPage />} />
          <Route path="/launch" element={<LaunchPage />} />
          <Route path="/workloads/:id" element={<WorkloadDetailPage />} />
          <Route path="/tokens" element={<TokensPage />} />
        </Routes>
      </main>
    </div>
  );
}
