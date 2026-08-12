"use client";

/**
 * The last resort, for a crash in the layout itself — where app/error.tsx
 * cannot help, because the shell it renders inside is the thing that broke.
 * It has to bring its own <html> and its own styling for the same reason.
 */
export default function GlobalError({ error }: { error: Error & { digest?: string } }) {
  return (
    <html lang="en">
      <body style={{ font: "16px/1.6 system-ui", maxWidth: "40rem", margin: "4rem auto", padding: "0 1rem" }}>
        <h1 style={{ fontSize: "1.25rem" }}>Hecate could not start its interface</h1>
        <p>This is a bug in the UI, not in your cluster. Nothing was changed.</p>
        <pre style={{ background: "#f3f4f6", padding: "0.75rem", overflowX: "auto", fontSize: "0.8rem" }}>
          {error.message}
          {error.digest ? `\n\ndigest: ${error.digest}` : ""}
        </pre>
        <p>
          The API is unaffected — <code>/healthz</code> and <code>/api/v1alpha1/…</code>
          answer normally.
        </p>
        <button onClick={() => window.location.reload()}>Reload</button>
      </body>
    </html>
  );
}
