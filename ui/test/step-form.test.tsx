import { describe, expect, it } from "vitest";

import { classify, required } from "@/lib/step-fields";
import { stepsYAML, toYAML } from "@/lib/yaml";
import type { StepSchema } from "@/lib/api";

describe("classify", () => {
  it("reads the primitive types straight off the schema", () => {
    expect(classify({ type: "string" })).toBe("string");
    expect(classify({ type: "integer" })).toBe("integer");
    expect(classify({ type: "number" })).toBe("integer");
    expect(classify({ type: "boolean" })).toBe("boolean");
  });

  it("tells an array of strings from an array of objects", () => {
    expect(classify({ type: "array", items: { type: "string" } })).toBe("string-array");
    expect(classify({ type: "array", items: { type: "integer" } })).toBe("integer-array");
    expect(
      classify({ type: "array", items: { type: "object", properties: { name: { type: "string" } } } }),
    ).toBe("object-array");
  });

  it("tells a real object from a string->string map", () => {
    expect(classify({ type: "object", properties: { name: { type: "string" } } })).toBe("object");
    expect(classify({ type: "object", additionalProperties: { type: "string" } })).toBe("map");
  });

  // The construct that matters most: a step's config that the schema does not
  // describe should render as something, not vanish from the form. This is
  // what makes that promise checkable rather than a comment.
  it("degrades an object with no properties to freeform, not silently", () => {
    expect(classify({ type: "object" })).toBe("freeform");
    expect(classify({ type: "object", additionalProperties: false })).toBe("freeform");
  });

  it("degrades a node with no declared type to freeform", () => {
    expect(classify({ description: "no type at all" })).toBe("freeform");
    expect(classify(undefined)).toBe("freeform");
  });

  it("degrades an array of freeform items to freeform, not a broken array kind", () => {
    expect(classify({ type: "array", items: { description: "untyped" } })).toBe("freeform");
  });
});

describe("required", () => {
  const schema: StepSchema = { type: "object", properties: {}, required: ["repo"] };

  it("finds a name in the schema's required list", () => {
    expect(required(schema, "repo")).toBe(true);
    expect(required(schema, "branch")).toBe(false);
  });

  it("is undefined-safe — every real step schema today has no required list", () => {
    expect(required({ type: "object" }, "repo")).toBe(false);
    expect(required(undefined, "repo")).toBe(false);
  });
});

describe("toYAML", () => {
  it("renders scalars plainly", () => {
    expect(toYAML("hello")).toBe("hello\n");
    expect(toYAML(42)).toBe("42\n");
    expect(toYAML(true)).toBe("true\n");
  });

  it("quotes a string that would otherwise parse as something else", () => {
    expect(toYAML("42")).toBe('"42"\n');
    expect(toYAML("true")).toBe('"true"\n');
    expect(toYAML("")).toBe('""\n');
    expect(toYAML("a: b")).toBe('"a: b"\n');
  });

  it("renders a nested map with 2-space indents", () => {
    expect(toYAML({ a: 1, b: { c: "x" } })).toBe("a: 1\nb:\n  c: x\n");
  });

  it("renders a list of scalars and a list of maps", () => {
    expect(toYAML(["a", "b"])).toBe("- a\n- b\n");
    expect(toYAML([{ k: "v" }])).toBe("- k: v\n");
  });
});

describe("stepsYAML", () => {
  it("renders an empty Passage as an empty list", () => {
    expect(stepsYAML([])).toBe("steps: []\n");
  });

  it("omits `as` and `with` when they were not given", () => {
    expect(stepsYAML([{ uses: "git-push" }])).toBe("steps:\n  - uses: git-push\n");
  });

  it("nests `with` under the step, matching a hand-written Gate's shape", () => {
    const yaml = stepsYAML([
      { uses: "git-clone", with: { repo: "https://example.com/deploy.git", depth: 1 } },
      { uses: "git-commit", as: "commit", with: { message: "bump" } },
    ]);
    expect(yaml).toBe(
      "steps:\n" +
        "  - uses: git-clone\n" +
        "    with:\n" +
        "      repo: https://example.com/deploy.git\n" +
        "      depth: 1\n" +
        "  - uses: git-commit\n" +
        "    as: commit\n" +
        "    with:\n" +
        "      message: bump\n",
    );
  });

  it("drops an empty `with` rather than writing `with: {}`", () => {
    expect(stepsYAML([{ uses: "git-push", with: {} }])).toBe("steps:\n  - uses: git-push\n");
  });
});
