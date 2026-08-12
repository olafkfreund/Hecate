import type { Metadata } from "next";
import { Geist, Geist_Mono } from "next/font/google";
import "./globals.css";
import { Shell } from "@/components/shell";
import { Theme } from "@/components/theme";

// The same two faces the Fides portal loads, under the same variable names —
// typography is most of why two products read as one.
const geistSans = Geist({ variable: "--font-geist-sans", subsets: ["latin"] });
const geistMono = Geist_Mono({ variable: "--font-geist-mono", subsets: ["latin"] });

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
      <body className={`${geistSans.variable} ${geistMono.variable} min-h-screen font-sans antialiased`}>
        <Theme>
          <Shell>{children}</Shell>
        </Theme>
      </body>
    </html>
  );
}
