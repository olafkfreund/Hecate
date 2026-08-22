import { readFileSync } from "node:fs";
import { join } from "node:path";
import ts from "typescript";
import { describe, expect, it } from "vitest";

/**
 * Does the browser's copy of the API types agree with what the API sends?
 *
 * lib/api.ts mirrors Go structs by hand and says so: an early version invented
 * `reason` and `remedy` where the Go type has `kind` and `fix`, and TypeScript
 * was perfectly happy, because nothing checks a response against a schema at
 * runtime. The page read undefined and rendered nothing.
 *
 * Nothing stopped that happening again, and five more hand-written mirrors were
 * added in one week. This is what stops it.
 *
 * The direction is deliberate: **every field TypeScript declares must exist in
 * Go**, and not the reverse. A TypeScript type that omits fields is a view of
 * the response — normal, and often the point. A TypeScript type that declares a
 * field the API never sends is the bug, every time.
 *
 * Read with the TypeScript compiler rather than a regular expression, because
 * the interfaces nest and a regular expression that handles nesting is a
 * parser. There is already a real one in the dependencies.
 */

const goShape: Record<string, string[]> = JSON.parse(
  readFileSync(join(__dirname, "apishape.json"), "utf8"),
);

/** The top-level property names each exported interface declares. */
function tsShape(): Record<string, string[]> {
  const path = join(__dirname, "..", "lib", "api.ts");
  const source = ts.createSourceFile(
    path,
    readFileSync(path, "utf8"),
    ts.ScriptTarget.Latest,
    true,
  );

  const names = (members: ts.NodeArray<ts.TypeElement>) =>
    members
      .filter((m): m is ts.PropertySignature => ts.isPropertySignature(m))
      .map((m) => (ts.isIdentifier(m.name) ? m.name.text : ""))
      .filter(Boolean);

  const out: Record<string, string[]> = {};
  source.forEachChild((node) => {
    if (ts.isInterfaceDeclaration(node)) {
      out[node.name.text] = names(node.members);
      return;
    }
    // `export type X = { ... }` as well as `export interface X`. This file uses
    // both, and a checker that saw only one would report the other as having no
    // TypeScript type at all — which is how an allowlist quietly stops checking.
    if (ts.isTypeAliasDeclaration(node) && ts.isTypeLiteralNode(node.type)) {
      out[node.name.text] = names(node.type.members);
    }
  });
  return out;
}

describe("the browser's copy of the API types", () => {
  const shapes = tsShape();

  it("declares no field the API does not send", () => {
    const wrong: string[] = [];

    for (const [name, fields] of Object.entries(shapes)) {
      const go = goShape[name];
      // Not every interface mirrors a Go type — Grant and WaitingKind are the
      // browser's own. An unmapped name is skipped rather than failed, and the
      // test below is what stops that skip becoming a hole.
      if (!go) continue;
      for (const f of fields) {
        if (!go.includes(f)) {
          wrong.push(`${name}.${f} — Go sends [${go.join(", ")}]`);
        }
      }
    }

    expect(wrong, wrong.join("\n")).toEqual([]);
  });

  it("still covers the types it is supposed to", () => {
    // Without this, deleting an entry from cmd/apishape's map would make the
    // check above pass by checking nothing — the failure mode of every
    // allowlist, and the one that looks like success.
    const mapped = Object.keys(shapes).filter((n) => goShape[n]);
    expect(mapped.length).toBeGreaterThanOrEqual(28);
  });

  it("maps every Go shape to an interface that exists", () => {
    // The other direction: a Go entry naming an interface that has been renamed
    // or removed is checking nothing, and would sit there looking like coverage.
    const orphaned = Object.keys(goShape).filter((n) => !shapes[n]);
    expect(orphaned, `no TypeScript interface named: ${orphaned.join(", ")}`).toEqual([]);
  });
});
