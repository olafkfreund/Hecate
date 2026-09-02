import type { StepSchema } from "@/lib/api";

/**
 * FieldKind is how one schema node should be drawn, decided once so the form
 * component only has a switch to run rather than schema logic mixed into JSX.
 *
 * Covers exactly what `pkg/passage/steps/schemas.json` contains today
 * (checked by hand — see the PR description): string, integer/number,
 * boolean, arrays of those or of objects, nested objects, and the one map
 * shape that occurs (`http.headers`, a `string -> string` map). Anything a
 * step's config gains later that this cannot classify — `enum`, `oneOf`, a
 * `$ref`, a map of non-strings — falls out as "freeform" so the field still
 * renders, as raw text/JSON, rather than the step silently vanishing from the
 * form.
 */
export type FieldKind =
  | "string"
  | "integer"
  | "boolean"
  | "string-array"
  | "integer-array"
  | "object-array"
  | "object"
  | "map"
  | "freeform";

export function classify(node: StepSchema | undefined): FieldKind {
  if (!node || !node.type) return "freeform";

  switch (node.type) {
    case "string":
      return "string";
    case "integer":
    case "number":
      return "integer";
    case "boolean":
      return "boolean";
    case "array": {
      switch (classify(node.items)) {
        case "string":
          return "string-array";
        case "integer":
          return "integer-array";
        case "object":
          return "object-array";
        default:
          return "freeform";
      }
    }
    case "object": {
      if (node.properties && Object.keys(node.properties).length > 0) return "object";
      if (
        node.additionalProperties &&
        typeof node.additionalProperties === "object" &&
        node.additionalProperties.type === "string"
      ) {
        return "map";
      }
      // additionalProperties: false with no properties (e.g. `http.body`) is
      // a schema for "an empty object" in the strict reading, but every real
      // use of that shape is a stand-in for "whatever JSON you like" — the Go
      // field behind it is `any`. Freeform is the honest degrade either way:
      // nothing here tells the form what fields to draw.
      return "freeform";
    }
    default:
      return "freeform";
  }
}

/** required reports whether `name` is required on the object schema `node`
 *  describes — `undefined`-safe so callers can pass a step's top-level
 *  schema (or a nested one) without checking first. */
export function required(node: StepSchema | undefined, name: string): boolean {
  return node?.required?.includes(name) ?? false;
}
