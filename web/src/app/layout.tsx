import type { Metadata } from "next";
import "./globals.css";
import { Providers } from "@/components/providers";
import { Nav } from "@/components/nav";

export const metadata: Metadata = {
  title: "NetworkAI",
  description: "Share posts and discover what people are talking about.",
};

export default function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="en">
      <body className="antialiased">
        <Providers>
          <Nav />
          <main className="mx-auto max-w-2xl px-4 py-6">{children}</main>
        </Providers>
      </body>
    </html>
  );
}
