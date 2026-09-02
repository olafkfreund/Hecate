"use client";

import { useMemo, useState } from "react";
import { ArrowDown, ArrowUp, Plus, Trash2 } from "lucide-react";
import { api, type StepSchema } from "@/lib/api";
import { Panel, useApi } from "@/components/loader";
import { ObjectFields, inputClass } from "@/components/step-field";
import { stepsYAML, type ComposedStep } from "@/lib/yaml";

/**
 * Author a Passage: pick steps from the catalogue #114 serves, fill in each
 * one's `with:` block from its schema, order them, and read off the YAML.
 *
 * Stops there on purpose (hecate#172, stage 1). Nothing here writes anywhere
 * — not the cluster, not git. Turning this YAML into a pull request against a
 * fleet repo, through `pkg/provider`, is the next stage: applying it directly
 * would make the browser a second source of truth Flux would just revert.
 */
export default function NewPassage() {
  const schemas = useApi(() => api.stepSchemas(), []);

  return (
    <div>
      <h1 className="text-xl font-semibold tracking-tight">Author a Passage</h1>
      <p className="mt-1 text-sm text-[var(--muted-foreground)]">
        Build a Gate&apos;s step list and read off the YAML. Nothing here is
        applied — copy the result into your fleet repo, or wait for the pull
        request this becomes in a later stage.
      </p>

      <div className="mt-6">
        <Panel state={schemas}>{(catalogue) => <Composer catalogue={catalogue} />}</Panel>
      </div>
    </div>
  );
}

let nextID = 0;

function Composer({ catalogue }: { catalogue: Record<string, StepSchema> }) {
  const [steps, setSteps] = useState<(ComposedStep & { id: number })[]>([]);
  const names = useMemo(() => Object.keys(catalogue).sort(), [catalogue]);

  function add(uses: string) {
    setSteps((prev) => [...prev, { id: nextID++, uses, with: {} }]);
  }
  function remove(id: number) {
    setSteps((prev) => prev.filter((s) => s.id !== id));
  }
  function move(id: number, by: -1 | 1) {
    setSteps((prev) => {
      const i = prev.findIndex((s) => s.id === id);
      const j = i + by;
      if (i < 0 || j < 0 || j >= prev.length) return prev;
      const next = [...prev];
      [next[i], next[j]] = [next[j], next[i]];
      return next;
    });
  }
  function update(id: number, patch: Partial<ComposedStep>) {
    setSteps((prev) => prev.map((s) => (s.id === id ? { ...s, ...patch } : s)));
  }

  const yaml = stepsYAML(steps.map((s) => ({ uses: s.uses, as: s.as, with: s.with })));

  return (
    <div className="space-y-6">
      <section className="rounded-lg border border-[var(--border)] p-4">
        <h2 className="text-sm font-medium">Steps</h2>
        <p className="mb-3 text-xs text-[var(--muted-foreground)]">
          Every step {"`uses`"} can name, from the controller&apos;s own registry.
        </p>
        <div className="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-3">
          {names.map((name) => (
            <button
              key={name}
              type="button"
              onClick={() => add(name)}
              className="flex flex-col items-start gap-0.5 rounded-md border border-[var(--border)] p-2 text-left text-sm hover:bg-[var(--secondary)]"
            >
              <span className="flex w-full items-center justify-between font-medium">
                {name}
                <Plus size={14} aria-hidden />
              </span>
              <span className="text-xs text-[var(--muted-foreground)]">
                {catalogue[name].description?.split("\n\n")[0]}
              </span>
            </button>
          ))}
        </div>
      </section>

      <section className="rounded-lg border border-[var(--border)] p-4">
        <h2 className="text-sm font-medium">Passage</h2>
        {steps.length === 0 ? (
          <p className="mt-2 text-sm text-[var(--muted-foreground)]">
            No steps yet — add one from the catalogue above.
          </p>
        ) : (
          <ol className="mt-3 space-y-3">
            {steps.map((step, i) => (
              <li key={step.id} className="rounded-md border border-[var(--border)] p-3">
                <div className="mb-2 flex flex-wrap items-center gap-2">
                  <span className="text-xs text-[var(--muted-foreground)]">#{i + 1}</span>
                  <span className="font-medium">{step.uses}</span>
                  <input
                    className={`${inputClass} ml-auto max-w-48`}
                    placeholder="as (optional)"
                    aria-label={`Name step ${i + 1}'s output`}
                    value={step.as ?? ""}
                    onChange={(e) => update(step.id, { as: e.target.value || undefined })}
                  />
                  <button
                    type="button"
                    className={iconButton}
                    disabled={i === 0}
                    onClick={() => move(step.id, -1)}
                    aria-label={`Move step ${i + 1} up`}
                  >
                    <ArrowUp size={14} aria-hidden />
                  </button>
                  <button
                    type="button"
                    className={iconButton}
                    disabled={i === steps.length - 1}
                    onClick={() => move(step.id, 1)}
                    aria-label={`Move step ${i + 1} down`}
                  >
                    <ArrowDown size={14} aria-hidden />
                  </button>
                  <button
                    type="button"
                    className={`${iconButton} hover:text-[var(--destructive)]`}
                    onClick={() => remove(step.id)}
                    aria-label={`Remove step ${i + 1}`}
                  >
                    <Trash2 size={14} aria-hidden />
                  </button>
                </div>
                <ObjectFields
                  schema={catalogue[step.uses] ?? {}}
                  value={step.with ?? {}}
                  onChange={(v) => update(step.id, { with: v })}
                />
              </li>
            ))}
          </ol>
        )}
      </section>

      <section className="rounded-lg border border-[var(--border)] p-4">
        <h2 className="text-sm font-medium">YAML</h2>
        <p className="mb-2 text-xs text-[var(--muted-foreground)]">
          The {"`steps:`"} block of a Gate&apos;s {"`spec.passage`"}. Paste it into the Gate that
          should run this Passage.
        </p>
        <pre className="overflow-x-auto rounded-md border border-[var(--border)] bg-[var(--secondary)] p-3 text-xs">
          <code>{yaml}</code>
        </pre>
      </section>
    </div>
  );
}

const iconButton =
  "rounded-md border border-[var(--border)] p-1 text-[var(--muted-foreground)] hover:bg-[var(--secondary)] disabled:opacity-30";
