"use client";

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from "react";
import { useRouter } from "next/navigation";
import { useQueryClient } from "@tanstack/react-query";
import { api, setUnauthorizedHandler, tokenStore, usernameFromToken } from "./api";

type Auth = {
  username: string | null;
  ready: boolean;
  login: (u: string, p: string) => Promise<void>;
  logout: () => void;
};

const AuthContext = createContext<Auth | null>(null);

// Trade-off, stated honestly: the token lives in localStorage, which any XSS
// on this origin could read. An httpOnly cookie set by the API would avoid
// that but needs CSRF protection and same-site deployment. See docs/frontend.md.
export function AuthProvider({ children }: { children: React.ReactNode }) {
  const [username, setUsername] = useState<string | null>(null);
  const [ready, setReady] = useState(false);
  const router = useRouter();
  const qc = useQueryClient();

  useEffect(() => {
    const u = usernameFromToken(tokenStore.get());
    if (!u) tokenStore.clear();
    setUsername(u);
    setReady(true);
  }, []);

  const logout = useCallback(() => {
    tokenStore.clear();
    setUsername(null);
    qc.clear();
  }, [qc]);

  useEffect(() => {
    setUnauthorizedHandler(() => {
      logout();
      router.push(`/login?next=${encodeURIComponent(window.location.pathname)}`);
    });
  }, [logout, router]);

  const login = useCallback(async (u: string, p: string) => {
    const { token } = await api.login(u, p);
    tokenStore.set(token);
    setUsername(usernameFromToken(token));
  }, []);

  const value = useMemo(() => ({ username, ready, login, logout }), [username, ready, login, logout]);
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth() {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth outside AuthProvider");
  return ctx;
}
