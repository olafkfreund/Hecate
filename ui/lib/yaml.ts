/**
 * A hand-rolled YAML emitter for the one shape this UI ever renders: a
 * `steps:` list built from JSON-schema-shaped form values (strings, numbers,
 * booleans, nested maps and arrays — the same universe `JSON.parse` produces).
 *
 * No library: `ui/package.json` carries none, and the surface here — block
 * maps and sequences, no anchors, no multi-document, no flow style — is a
 * few small functions. Reach for `yaml` from npm if a step ever needs
 * something this does not cover.
 */

function pad(indent: number): string {
  return "  ".repeat(indent);
}

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/** Whether a string must be quoted to round-trip as YAML rather than as
 *  something else — a number, a bool, an empty scalar, or a string YAML
 *  would otherwise parse structurally (a colon, a leading `-`, ...). */
function needsQuoting(s: string): boolean {
  if (s === "") return true;
  if (/^\s|\s$/.test(s)) return true;
  // C0 controls (\n, \r, \t and the rest): a raw one emitted mid-scalar
  // breaks the line structure the whole format is built on. JSON.stringify
  // escapes them correctly, and YAML double-quoted style understands the
  // result.
  if (/[\x00-\x1f]/.test(s)) return true;
  if (/^[-?:,[\]{}#&*!|>'"%@`]/.test(s)) return true;
  if (s.includes(": ") || s.endsWith(":") || s.includes(" #")) return true;
  if (/^(true|false|null|yes|no|~)$/i.test(s)) return true;
  if (/^-?\d+(\.\d+)?$/.test(s)) return true;
  return false;
}

/** key is a map key, quoted by the same rule as a scalar value: it is
 *  author input wherever a schema's `additionalProperties` allows an
 *  arbitrary key (`http.headers`), and an unquoted colon, leading dash or
 *  `#` in a header name breaks the document exactly as it would in a
 *  value. */
function key(k: string): string {
  return needsQuoting(k) ? JSON.stringify(k) : k;
}

function scalar(v: unknown): string {
  if (v === null || v === undefined) return "null";
  if (typeof v === "boolean" || typeof v === "number") return String(v);
  const s = String(v);
  return needsQuoting(s) ? JSON.stringify(s) : s;
}

function dumpObject(obj: Record<string, unknown>, indent: number): string[] {
  const keys = Object.keys(obj).filter((k) => obj[k] !== undefined);
  if (keys.length === 0) return [`${pad(indent)}{}`];

  const lines: string[] = [];
  for (const k of keys) {
    const v = obj[k];
    const kq = key(k);
    if (isPlainObject(v)) {
      if (Object.keys(v).length === 0) {
        lines.push(`${pad(indent)}${kq}: {}`);
      } else {
        lines.push(`${pad(indent)}${kq}:`);
        lines.push(...dumpObject(v, indent + 1));
      }
    } else if (Array.isArray(v)) {
      if (v.length === 0) {
        lines.push(`${pad(indent)}${kq}: []`);
      } else {
        lines.push(`${pad(indent)}${kq}:`);
        lines.push(...dumpArray(v, indent));
      }
    } else {
      lines.push(`${pad(indent)}${kq}: ${scalar(v)}`);
    }
  }
  return lines;
}

function dumpArray(arr: unknown[], indent: number): string[] {
  const lines: string[] = [];
  for (const item of arr) {
    if (isPlainObject(item)) {
      const objLines = dumpObject(item, indent + 1);
      lines.push(`${pad(indent)}- ${objLines[0].trimStart()}`);
      lines.push(...objLines.slice(1));
    } else if (Array.isArray(item)) {
      if (item.length === 0) {
        lines.push(`${pad(indent)}- []`);
      } else {
        const subLines = dumpArray(item, indent + 1);
        lines.push(`${pad(indent)}- ${subLines[0].trimStart()}`);
        lines.push(...subLines.slice(1));
      }
    } else {
      lines.push(`${pad(indent)}- ${scalar(item)}`);
    }
  }
  return lines;
}

/** toYAML renders one value as a standalone YAML document. Exported mainly
 *  for testing — pages compose Passage steps through {@link stepsYAML}. */
export function toYAML(value: unknown): string {
  if (isPlainObject(value)) {
    if (Object.keys(value).length === 0) return "{}\n";
    return dumpObject(value, 0).join("\n") + "\n";
  }
  if (Array.isArray(value)) {
    if (value.length === 0) return "[]\n";
    return dumpArray(value, 0).join("\n") + "\n";
  }
  return scalar(value) + "\n";
}

/** ComposedStep is one row of the form: what step, named how, configured how. */
export interface ComposedStep {
  uses: string;
  as?: string;
  with?: Record<string, unknown>;
}

/**
 * stepsYAML renders the ordered step list as the `steps:` block a Gate's
 * `spec.passage` carries (see `api/v1alpha1/gate.go`'s `PassageTemplate`).
 *
 * A `with:` that ended up empty is omitted rather than emitted as `{}`: an
 * author who filled in nothing meant "use the defaults", and `with: {}` reads
 * as a deliberate empty block.
 */
export function stepsYAML(steps: ComposedStep[]): string {
  if (steps.length === 0) return "steps: []\n";

  const items = steps.map((s) => {
    const obj: Record<string, unknown> = { uses: s.uses };
    if (s.as) obj.as = s.as;
    if (s.with && Object.keys(s.with).length > 0) obj.with = s.with;
    return obj;
  });

  return `steps:\n${dumpArray(items, 1).join("\n")}\n`;
}
