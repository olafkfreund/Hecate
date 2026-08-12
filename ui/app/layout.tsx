import type { Metadata } from "next";
import "./globals.css";
import { Shell } from "@/components/shell";
import { Theme } from "@/components/theme";

export const metadata: Metadata = {
  title: "Hecate",
  description: "The promotion and release-orchestration layer for FluxCD.",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    // suppressHydrationWarning because next-themes sets the class on <html>
    // before React hydrates, which is the whole point — otherwise the page
    // flashes the wrong theme.
    <html lang="en" suppressHydrationWarning>
      <body className="min-h-screen antialiased">
        <Theme>
          <Shell>{children}</Shell>
        </Theme>
      </body>
    </html>
  );
}
