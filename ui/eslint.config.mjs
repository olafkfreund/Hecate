// eslint-config-next ships flat configs directly in v16, so they are imported
// rather than wrapped in FlatCompat — the compat shim hits a circular reference
// in this config's plugin graph and cannot load it at all.
import coreWebVitals from "eslint-config-next/core-web-vitals";
import typescript from "eslint-config-next/typescript";

const config = [
  ...coreWebVitals,
  ...typescript,
  { ignores: [".next/**", "out/**", "node_modules/**"] },
];

export default config;
