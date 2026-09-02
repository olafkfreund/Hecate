"use client";

import { useEffect, useMemo, useState, useSyncExternalStore } from "react";
import { ArrowDown, ArrowUp, GitPullRequest, Plus, Trash2 } from "lucide-react";
import {
  api,
  ApiError,
  type AuthoredPullRequest,
  type Bundle,
  type Gate,
  type StepProblem,
  type StepSchema,
} from "@/lib/api";
import { Panel, useApi } from "@/components/loader";
import { ObjectFields, inputClass } from "@/components/step-field";
import { stepsYAML, type ComposedStep } from "@/lib/yaml";

/**
 * Author a Passage: pick steps from the catalogue #114 serves, fill in each
 * one's `with:` block from its schema, order them, and open a pull request
 * for the whole thing (hecate#172).
 *
 * Nothing here ever applies anything to this cluster — no Create, no Apply.
 * The button at the bottom asks the API to render a complete Passage
 * manifest, commit it to a branch of a repository named in the form below,
 * and open a pull request through `pkg/provider`. The cluster only sees this
 * Passage once a human merges that pull request and Flux picks it up — see
 * docs/DECISIONS.md D58/D59.
 */
export default function NewPassage() {
  const data = useApi(
    () => Promise.all([api.stepSchemas(), api.gates(), api.bundles()]),
    [],
  );

  return (
    <div>
      <h1 className="text-xl font-semibold tracking-tight">Author a Passage</h1>
      <p className="mt-1 text-sm text-[var(--muted-foreground)]">
        Build a Passage — the Gate it crosses, the Bundle it moves, and its step
        list — and open a pull request for it. Nothing here is applied; a human
        reviews the pull request before this Passage exists.
      </p>

      <div className="mt-6">
        <Panel state={data}>
          {([catalogue, gates, bundles]) => (
            <Composer catalogue={catalogue} gates={gates} bundles={bundles} />
          )}
        </Panel>
      </div>
    </div>
  );
}

let nextID = 0;

/**
 * usePersistedField remembers one form field per browser (D58), for the
 * fields an author sets once and reuses across many Passages targeting the
 * same fleet repo — repo, path, base, credentialsRef and the like.
 *
 * The same shape as ui/lib/browser.ts's useQueryParam/setQueryParam: the
 * server snapshot is empty (there is no localStorage during server
 * rendering), and a write dispatches a synthetic event the hook subscribes
 * to, rather than calling setState from inside an effect.
 */
function usePersistedField(key: string): [string, (v: string) => void] {
  const value = useSyncExternalStore(
    (onChange) => {
      window.addEventListener("hecate-author-field", onChange);
      return () => window.removeEventListener("hecate-author-field", onChange);
    },
    () => {
      try {
        return localStorage.getItem(key) ?? "";
      } catch {
        return ""; // private browsing, or storage disabled
      }
    },
    () => "",
  );

  function set(v: string) {
    try {
      localStorage.setItem(key, v);
    } catch {
      /* nothing to do — the field still works for this session */
    }
    window.dispatchEvent(new Event("hecate-author-field"));
  }
  return [value, set];
}

/**
 * useValidation asks `/passages/validate` what is wrong with the composed
 * step list, so the browser refuses the same things authorPassage will
 * (hecate#172 scope item 4) — before the click that opens a pull request,
 * not only after it.
 *
 * Debounced 500ms after the last edit, not on every keystroke: a step's
 * `with:` block is typed field by field, and validating each character would
 * mean a request per keystroke for no benefit — the answer to "is this
 * step valid yet" does not change usefully faster than someone can pause.
 */
function useValidation(steps: ComposedStep[]): StepProblem[] {
  const [problems, setProblems] = useState<StepProblem[]>([]);
  const key = JSON.stringify(steps);

  useEffect(() => {
    if (steps.length === 0) return;
    let live = true;
    const timer = setTimeout(() => {
      api.validatePassage(steps).then(
        (res) => live && setProblems(res.problems),
        // Best-effort: a validate call that fails (offline, a 5xx) must never
        // block editing or hide the submit button — authorPassage still runs
        // the real check server-side either way.
        () => live && setProblems([]),
      );
    }, 500);
    return () => {
      live = false;
      clearTimeout(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);

  // An empty step list has nothing to report, computed rather than set from
  // the effect above so clearing the list clears its problems on the same
  // render — no stale row-level messages left over from a step just removed.
  return steps.length === 0 ? [] : problems;
}

function Composer({
  catalogue,
  gates,
  bundles,
}: {
  catalogue: Record<string, StepSchema>;
  gates: Gate[];
  bundles: Bundle[];
}) {
  const [steps, setSteps] = useState<(ComposedStep & { id: number })[]>([]);
  const names = useMemo(() => Object.keys(catalogue).sort(), [catalogue]);

  const [name, setName] = useState("");
  const [gateKey, setGateKey] = useState("");
  const [bundleName, setBundleName] = useState("");

  const [repo, setRepo] = usePersistedField("hecate.author.repo");
  const [path, setPath] = usePersistedField("hecate.author.path");
  const [base, setBase] = usePersistedField("hecate.author.base");
  const [credentialsRef, setCredentialsRef] = usePersistedField("hecate.author.credentialsRef");
  const [overwrite, setOverwrite] = useState(false);

  const [submitting, setSubmitting] = useState(false);
  const [result, setResult] = useState<AuthoredPullRequest | null>(null);
  const [error, setError] = useState<string | null>(null);

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

  const composed = steps.map((s) => ({ uses: s.uses, as: s.as, with: s.with }));
  const yaml = stepsYAML(composed);
  const problems = useValidation(composed);

  // The Gate a Passage crosses decides which namespace it lives in — Hecate
  // has no separate namespace picker on this page, the same way promoting
  // takes its namespace from the Gate being crossed. Bundles are offered only
  // from that namespace, since a Passage's Gate and Bundle are always in one.
  const gate = gates.find((g) => gateID(g) === gateKey);
  const bundlesInNamespace = bundles.filter(
    (b) => gate !== undefined && b.metadata.namespace === gate.metadata.namespace,
  );

  const canSubmit =
    name.trim() !== "" &&
    gate !== undefined &&
    bundleName.trim() !== "" &&
    steps.length > 0 &&
    repo.trim() !== "" &&
    path.trim() !== "" &&
    credentialsRef.trim() !== "";

  async function submit() {
    if (!gate) return;
    setSubmitting(true);
    setResult(null);
    setError(null);
    try {
      const pr = await api.authorPassage(gate.metadata.namespace, {
        name,
        gate: gate.metadata.name,
        bundle: bundleName,
        steps: composed,
        repo,
        path,
        base: base || undefined,
        overwrite,
        credentialsRef,
      });
      setResult(pr);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "the request failed");
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div className="space-y-6">
      <section className="rounded-lg border border-[var(--border)] p-4">
        <h2 className="text-sm font-medium">Passage</h2>
        <div className="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-3">
          <label className="text-sm">
            <span className="mb-1 block text-xs text-[var(--muted-foreground)]">Name</span>
            <input
              className={inputClass}
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="promote-1.2.3"
            />
          </label>
          <label className="text-sm">
            <span className="mb-1 block text-xs text-[var(--muted-foreground)]">Gate</span>
            <select
              className={inputClass}
              value={gateKey}
              onChange={(e) => {
                setGateKey(e.target.value);
                setBundleName("");
              }}
            >
              <option value="">Choose a Gate…</option>
              {gates.map((g) => (
                <option key={gateID(g)} value={gateID(g)}>
                  {g.metadata.namespace}/{g.metadata.name}
                </option>
              ))}
            </select>
          </label>
          <label className="text-sm">
            <span className="mb-1 block text-xs text-[var(--muted-foreground)]">Bundle</span>
            <select
              className={inputClass}
              value={bundleName}
              disabled={!gateKey}
              onChange={(e) => setBundleName(e.target.value)}
            >
              <option value="">Choose a Bundle…</option>
              {bundlesInNamespace.map((b) => (
                <option key={b.metadata.name} value={b.metadata.name}>
                  {b.metadata.name}
                </option>
              ))}
            </select>
          </label>
        </div>
      </section>

      <section className="rounded-lg border border-[var(--border)] p-4">
        <h2 className="text-sm font-medium">Steps</h2>
        <p className="mb-3 text-xs text-[var(--muted-foreground)]">
          Every step {"`uses`"} can name, from the controller&apos;s own registry.
        </p>
        <div className="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-3">
          {names.map((n) => (
            <button
              key={n}
              type="button"
              onClick={() => add(n)}
              className="flex flex-col items-start gap-0.5 rounded-md border border-[var(--border)] p-2 text-left text-sm hover:bg-[var(--secondary)]"
            >
              <span className="flex w-full items-center justify-between font-medium">
                {n}
                <Plus size={14} aria-hidden />
              </span>
              <span className="text-xs text-[var(--muted-foreground)]">
                {catalogue[n].description?.split("\n\n")[0]}
              </span>
            </button>
          ))}
        </div>
      </section>

      <section className="rounded-lg border border-[var(--border)] p-4">
        <h2 className="text-sm font-medium">Step list</h2>
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
                {problems
                  .filter((p) => p.index === i)
                  .map((p, j) => (
                    <p
                      key={j}
                      role="alert"
                      className="mt-2 text-xs text-[var(--destructive)]"
                    >
                      {p.message}
                    </p>
                  ))}
              </li>
            ))}
          </ol>
        )}
      </section>

      <section className="rounded-lg border border-[var(--border)] p-4">
        <h2 className="text-sm font-medium">YAML preview</h2>
        <p className="mb-2 text-xs text-[var(--muted-foreground)]">
          {"`spec.steps`"} as the pull request below will render it. The server
          renders its own copy from the same fields when you open the pull
          request — this is a preview, not what gets committed (D59).
        </p>
        <pre className="overflow-x-auto rounded-md border border-[var(--border)] bg-[var(--secondary)] p-3 text-xs">
          <code>{yaml}</code>
        </pre>
      </section>

      <section className="rounded-lg border border-[var(--border)] p-4">
        <h2 className="text-sm font-medium">Open a pull request</h2>
        <p className="mb-3 text-xs text-[var(--muted-foreground)]">
          Where this Passage&apos;s manifest is committed. Hecate cannot derive
          this — a Gate&apos;s own YAML does not necessarily live in the fleet
          repo its steps write to (D58) — so it is remembered per browser
          instead.
        </p>
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <label className="text-sm">
            <span className="mb-1 block text-xs text-[var(--muted-foreground)]">
              Repository (clone URL)
            </span>
            <input
              className={inputClass}
              value={repo}
              onChange={(e) => setRepo(e.target.value)}
              placeholder="https://github.com/acme/fleet.git"
            />
          </label>
          <label className="text-sm">
            <span className="mb-1 block text-xs text-[var(--muted-foreground)]">
              Path in the repository
            </span>
            <input
              className={inputClass}
              value={path}
              onChange={(e) => setPath(e.target.value)}
              placeholder="passages/promote-1.2.3.yaml"
            />
          </label>
          <label className="text-sm">
            <span className="mb-1 block text-xs text-[var(--muted-foreground)]">
              Base branch (optional)
            </span>
            <input
              className={inputClass}
              value={base}
              onChange={(e) => setBase(e.target.value)}
              placeholder="main"
            />
          </label>
          <label className="text-sm">
            <span className="mb-1 block text-xs text-[var(--muted-foreground)]">
              Credentials Secret
            </span>
            <input
              className={inputClass}
              value={credentialsRef}
              onChange={(e) => setCredentialsRef(e.target.value)}
              placeholder="fleet-repo-token"
            />
          </label>
        </div>
        <label className="mt-3 flex items-center gap-2 text-xs text-[var(--muted-foreground)]">
          <input
            type="checkbox"
            checked={overwrite}
            onChange={(e) => setOverwrite(e.target.checked)}
          />
          Overwrite the file at this path if it already exists on the target
          branch
        </label>

        <button
          type="button"
          disabled={!canSubmit || submitting}
          onClick={submit}
          className="mt-4 flex items-center gap-2 rounded-md border border-[var(--border)] bg-[var(--secondary)] px-3 py-1.5 text-sm font-medium disabled:opacity-40"
        >
          <GitPullRequest size={14} aria-hidden />
          {submitting ? "Opening…" : "Open pull request"}
        </button>

        {error && (
          <p className="mt-3 text-sm text-[var(--destructive)]" role="alert">
            {error}
          </p>
        )}
        {result && (
          <p className="mt-3 text-sm">
            Opened{" "}
            <a
              href={result.url}
              target="_blank"
              rel="noreferrer"
              className="font-medium underline underline-offset-2"
            >
              {result.repo}#{result.number}
            </a>{" "}
            ({result.state})
          </p>
        )}
      </section>
    </div>
  );
}

function gateID(g: Gate): string {
  return `${g.metadata.namespace}/${g.metadata.name}`;
}

const iconButton =
  "rounded-md border border-[var(--border)] p-1 text-[var(--muted-foreground)] hover:bg-[var(--secondary)] disabled:opacity-30";
