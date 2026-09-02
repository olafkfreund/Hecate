"use client";

import { Plus, Trash2 } from "lucide-react";
import type { StepSchema } from "@/lib/api";
import { classify, required } from "@/lib/step-fields";

/**
 * The step-config form.
 *
 * One component recursing over the JSON Schema `pkg/passage/steps` publishes,
 * rather than one form per step: 14 hand-written forms drift the moment a
 * step gains a field, silently — nobody edits a form they do not know exists.
 * Reading the schema instead means a new field appears the next time
 * `make generate` runs, with no UI change required.
 */

export const inputClass =
  "w-full rounded-md border border-[var(--border)] bg-transparent px-2 py-1 text-sm text-[var(--foreground)]";
const removeButton =
  "rounded-md border border-[var(--border)] p-1 text-[var(--muted-foreground)] hover:bg-[var(--secondary)] hover:text-[var(--destructive)]";
const addButton =
  "flex items-center gap-1 rounded-md border border-dashed border-[var(--border)] px-2 py-1 text-xs text-[var(--muted-foreground)] hover:bg-[var(--secondary)]";

/** The first paragraph of a Go doc comment — the rest is detail a form label
 *  has no room for. */
function summary(description?: string): string | undefined {
  return description?.split("\n\n")[0]?.trim() || undefined;
}

/** setKey returns `obj` with `key` set to `value`, or removed when `value` is
 *  "empty" — undefined, an empty string, or an empty object/array. A step's
 *  `with:` should carry only what the author actually filled in; every
 *  untouched field defaults on the controller side, and writing it out as
 *  `""` or `null` would pin a value nobody chose. */
function setKey(obj: Record<string, unknown>, key: string, value: unknown): Record<string, unknown> {
  const empty =
    value === undefined ||
    value === "" ||
    (Array.isArray(value) && value.length === 0) ||
    (typeof value === "object" && value !== null && Object.keys(value).length === 0);
  const next = { ...obj };
  if (empty) delete next[key];
  else next[key] = value;
  return next;
}

/** Label is the field name and description every kind of field shares. */
function Label({ name, node, req }: { name: string; node: StepSchema; req: boolean }) {
  const help = summary(node.description);
  return (
    <div>
      <label className="block text-xs font-medium">
        {name}
        {req && (
          <span className="ml-1 text-[var(--destructive)]" title="required" aria-label="required">
            *
          </span>
        )}
      </label>
      {help && <p className="text-xs text-[var(--muted-foreground)]">{help}</p>}
    </div>
  );
}

/** ObjectFields renders one schema's `properties` — the top level of a step's
 *  `with:` block, and recursively, any nested object it contains. */
export function ObjectFields({
  schema,
  value,
  onChange,
}: {
  schema: StepSchema;
  value: Record<string, unknown>;
  onChange: (v: Record<string, unknown>) => void;
}) {
  const props = schema.properties ?? {};
  const names = Object.keys(props);

  if (names.length === 0) {
    return (
      <p className="text-xs text-[var(--muted-foreground)]">
        This step takes no configuration.
      </p>
    );
  }

  return (
    <div className="space-y-3">
      {names.map((name) => (
        <Field
          key={name}
          name={name}
          node={props[name]}
          req={required(schema, name)}
          value={value[name]}
          onChange={(v) => onChange(setKey(value, name, v))}
        />
      ))}
    </div>
  );
}

function Field({
  name,
  node,
  req,
  value,
  onChange,
}: {
  name: string;
  node: StepSchema;
  req: boolean;
  value: unknown;
  onChange: (v: unknown) => void;
}) {
  const kind = classify(node);

  switch (kind) {
    case "string":
      return (
        <div>
          <Label name={name} node={node} req={req} />
          <input
            className={inputClass}
            value={(value as string) ?? ""}
            onChange={(e) => onChange(e.target.value)}
          />
        </div>
      );

    case "integer":
      return (
        <div>
          <Label name={name} node={node} req={req} />
          <input
            type="number"
            className={inputClass}
            value={value === undefined ? "" : (value as number)}
            onChange={(e) => onChange(e.target.value === "" ? undefined : Number(e.target.value))}
          />
        </div>
      );

    case "boolean":
      return (
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={Boolean(value)}
            onChange={(e) => onChange(e.target.checked || undefined)}
          />
          {name}
          {summary(node.description) && (
            <span className="text-xs text-[var(--muted-foreground)]">— {summary(node.description)}</span>
          )}
        </label>
      );

    case "string-array":
    case "integer-array": {
      const text = Array.isArray(value) ? (value as unknown[]).join(", ") : "";
      return (
        <div>
          <Label name={name} node={node} req={req} />
          <input
            className={inputClass}
            placeholder="comma-separated"
            defaultValue={text}
            onBlur={(e) => {
              const items = e.target.value
                .split(",")
                .map((s) => s.trim())
                .filter((s) => s.length > 0);
              if (items.length === 0) {
                onChange(undefined);
                return;
              }
              onChange(kind === "integer-array" ? items.map(Number) : items);
            }}
          />
        </div>
      );
    }

    case "object":
      return (
        <fieldset className="rounded-md border border-[var(--border)] p-2">
          <legend className="px-1 text-xs font-medium">
            {name}
            {req && <span className="ml-1 text-[var(--destructive)]">*</span>}
          </legend>
          {summary(node.description) && (
            <p className="mb-2 text-xs text-[var(--muted-foreground)]">{summary(node.description)}</p>
          )}
          <ObjectFields
            schema={node}
            value={(value as Record<string, unknown>) ?? {}}
            onChange={(v) => onChange(v)}
          />
        </fieldset>
      );

    case "map":
      return <MapField name={name} node={node} value={(value as Record<string, string>) ?? {}} onChange={onChange} />;

    case "object-array":
      return <ObjectArrayField name={name} node={node} value={(value as Record<string, unknown>[]) ?? []} onChange={onChange} />;

    case "freeform":
    default:
      return (
        <div>
          <Label name={name} node={node} req={req} />
          <p className="mb-1 text-xs text-amber-500">
            This step&apos;s schema does not describe {kind === "freeform" ? "a shape for" : ""} this field
            — entered as plain text, or JSON for anything structured.
          </p>
          <textarea
            className={`${inputClass} h-16 font-mono`}
            defaultValue={typeof value === "string" ? value : value === undefined ? "" : JSON.stringify(value, null, 2)}
            onBlur={(e) => {
              const text = e.target.value;
              if (text.trim() === "") {
                onChange(undefined);
                return;
              }
              try {
                onChange(JSON.parse(text));
              } catch {
                onChange(text);
              }
            }}
          />
        </div>
      );
  }
}

/** MapField is `additionalProperties: {type: string}` — a `string -> string`
 *  map, which today means only `http.headers`. */
function MapField({
  name,
  node,
  value,
  onChange,
}: {
  name: string;
  node: StepSchema;
  value: Record<string, string>;
  onChange: (v: Record<string, string>) => void;
}) {
  const entries = Object.entries(value);

  return (
    <div>
      <Label name={name} node={node} req={false} />
      <div className="space-y-1">
        {entries.map(([k, v], i) => (
          <div key={i} className="flex gap-1">
            <input
              className={inputClass}
              placeholder="key"
              value={k}
              onChange={(e) => {
                const next = { ...value };
                delete next[k];
                next[e.target.value] = v;
                onChange(next);
              }}
            />
            <input
              className={inputClass}
              placeholder="value"
              value={v}
              onChange={(e) => onChange({ ...value, [k]: e.target.value })}
            />
            <button
              type="button"
              className={removeButton}
              onClick={() => {
                const next = { ...value };
                delete next[k];
                onChange(next);
              }}
              aria-label={`Remove ${k}`}
            >
              <Trash2 size={14} aria-hidden />
            </button>
          </div>
        ))}
      </div>
      <button
        type="button"
        className={`${addButton} mt-1`}
        onClick={() => onChange({ ...value, "": "" })}
      >
        <Plus size={12} aria-hidden /> Add header
      </button>
    </div>
  );
}

/** ObjectArrayField is an array of objects — `flux-wait.resources`,
 *  `http.secretHeaders`, `edit-yaml.edits` — rendered as a repeatable group
 *  of the item schema's own fields. */
function ObjectArrayField({
  name,
  node,
  value,
  onChange,
}: {
  name: string;
  node: StepSchema;
  value: Record<string, unknown>[];
  onChange: (v: Record<string, unknown>[]) => void;
}) {
  const itemSchema = node.items ?? {};

  return (
    <div>
      <Label name={name} node={node} req={false} />
      <div className="space-y-2">
        {value.map((item, i) => (
          <div key={i} className="rounded-md border border-[var(--border)] p-2">
            <div className="mb-1 flex items-center justify-between">
              <span className="text-xs text-[var(--muted-foreground)]">
                {name} #{i + 1}
              </span>
              <button
                type="button"
                className={removeButton}
                onClick={() => onChange(value.filter((_, j) => j !== i))}
                aria-label={`Remove ${name} #${i + 1}`}
              >
                <Trash2 size={14} aria-hidden />
              </button>
            </div>
            <ObjectFields
              schema={itemSchema}
              value={item}
              onChange={(v) => onChange(value.map((it, j) => (j === i ? v : it)))}
            />
          </div>
        ))}
      </div>
      <button
        type="button"
        className={`${addButton} mt-1`}
        onClick={() => onChange([...value, {}])}
      >
        <Plus size={12} aria-hidden /> Add {name}
      </button>
    </div>
  );
}
