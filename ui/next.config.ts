import type { NextConfig } from "next";

const config: NextConfig = {
  // A static export, because the UI is embedded in the hecate-api binary and
  // served from the same origin as the API (see pkg/api/ui.go). That buys no
  // CORS, one Deployment, one URL, and a session cookie that simply works —
  // at the cost of server-side rendering, which a dashboard over an
  // authenticated API would not benefit from anyway.
  output: "export",
  // Every route becomes a directory with an index.html, so a Go file server
  // can resolve /gates without knowing anything about the routes.
  trailingSlash: true,
  images: { unoptimized: true },
  // Type errors fail the build by default and are left that way. Linting is
  // its own script in Next 16 rather than a build flag, and runs in CI.
};

export default config;
