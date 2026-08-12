import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { fileURLToPath } from "node:url";

// NODE_ENV is forced to "test" by the npm scripts rather than left to vitest's
// default, because an ambient NODE_ENV=production wins — and React's production
// build omits `act`, so every test fails with "React.act is not a function"
// and nothing points at the environment. The same variable also makes `npm
// install` skip devDependencies, which is why the Makefile passes --include=dev.
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: { "@": fileURLToPath(new URL(".", import.meta.url)) },
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./test/setup.ts"],
  },
});
