"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { LogOut, Search } from "lucide-react";
import { useAuth } from "@/lib/auth";
import { Button } from "./ui";
import { cn } from "@/lib/utils";

export function Nav() {
  const { username, ready, logout } = useAuth();
  const path = usePathname();
  const link = (href: string, label: string) => (
    <Link
      href={href}
      className={cn("text-sm hover:text-foreground", path === href ? "text-foreground font-medium" : "text-muted-foreground")}
    >
      {label}
    </Link>
  );
  return (
    <header className="sticky top-0 z-10 border-b border-border bg-background/90 backdrop-blur">
      <nav className="mx-auto flex h-14 max-w-2xl items-center gap-4 px-4">
        <Link href="/" className="font-semibold tracking-tight">
          NetworkAI
        </Link>
        {link("/", "Feed")}
        <Link href="/search" aria-label="Search" className="text-muted-foreground hover:text-foreground">
          <Search className="h-4 w-4" />
        </Link>
        <div className="ml-auto flex items-center gap-3">
          {!ready ? null : username ? (
            <>
              {link(`/u/${username}`, `@${username}`)}
              <Button variant="ghost" size="icon" onClick={logout} aria-label="Log out">
                <LogOut className="h-4 w-4" />
              </Button>
            </>
          ) : (
            <>
              {link("/login", "Log in")}
              <Button asChild size="sm">
                <Link href="/signup">Sign up</Link>
              </Button>
            </>
          )}
        </div>
      </nav>
    </header>
  );
}
