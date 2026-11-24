"use client";

import { useState } from "react";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { api } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { errorText } from "./posts";
import { Alert, Button, Card, Input } from "./ui";

export function AuthForm({ mode }: { mode: "login" | "signup" }) {
  const { login } = useAuth();
  const router = useRouter();
  const next = useSearchParams().get("next");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      if (mode === "signup") await api.signup(username, password);
      await login(username, password);
      // Only follow same-site relative paths (avoid open redirects).
      router.push(next && next.startsWith("/") && !next.startsWith("//") ? next : "/");
    } catch (err) {
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card className="mx-auto max-w-sm space-y-4">
      <h1 className="text-lg font-semibold">{mode === "login" ? "Log in" : "Create an account"}</h1>
      <form onSubmit={submit} className="space-y-3">
        <Input aria-label="Username" placeholder="username" autoComplete="username" value={username}
          onChange={(e) => setUsername(e.target.value.toLowerCase())} />
        <Input aria-label="Password" type="password" placeholder="password"
          autoComplete={mode === "login" ? "current-password" : "new-password"} value={password}
          onChange={(e) => setPassword(e.target.value)} />
        {mode === "signup" && (
          <p className="text-xs text-muted-foreground">3–20 characters: a–z, 0–9, _. Password at least 8 characters.</p>
        )}
        {error && <Alert>{error}</Alert>}
        <Button type="submit" className="w-full" disabled={busy || !username || !password}>
          {mode === "login" ? "Log in" : "Sign up"}
        </Button>
      </form>
      <p className="text-center text-sm text-muted-foreground">
        {mode === "login" ? (
          <>No account? <Link href="/signup" className="text-primary hover:underline">Sign up</Link></>
        ) : (
          <>Have an account? <Link href="/login" className="text-primary hover:underline">Log in</Link></>
        )}
      </p>
    </Card>
  );
}
