"use client";

import React from "react";

/**
 * NamespaceGroups lays a cluster-wide list out by namespace.
 *
 * **Why this exists.** The pages used to show one namespace, chosen from a
 * picker, which made "what is happening" a question you could only ask about a
 * place you had already guessed — and it hid every other answer behind a
 * control most people never touched. Everything is shown now, so the job is
 * placement rather than selection.
 *
 * **One namespace gets no heading.** A section header naming the only namespace
 * there is adds a line of chrome and no information. The heading appears when
 * there is something to tell apart, which is exactly when it earns its space.
 *
 * Order comes from the server, which groups by namespace already. Sorting again
 * here would be a second opinion about ordering, free to disagree with the one
 * the API tests pin.
 */
export function NamespaceGroups<T>({
  items,
  namespaceOf,
  children,
  empty,
}: {
  items: T[];
  namespaceOf: (item: T) => string;
  children: (items: T[], namespace: string) => React.ReactNode;
  empty: React.ReactNode;
}) {
  if (items.length === 0) return <>{empty}</>;

  const groups: { namespace: string; items: T[] }[] = [];
  for (const item of items) {
    const ns = namespaceOf(item);
    const last = groups[groups.length - 1];
    if (last && last.namespace === ns) last.items.push(item);
    else groups.push({ namespace: ns, items: [item] });
  }

  if (groups.length === 1) return <>{children(groups[0].items, groups[0].namespace)}</>;

  return (
    <div className="space-y-8">
      {groups.map((g) => (
        <section key={g.namespace} aria-labelledby={`ns-${g.namespace}`}>
          <h2
            id={`ns-${g.namespace}`}
            className="mb-3 flex items-baseline gap-2 border-b border-[var(--border)] pb-1.5"
          >
            <span className="text-sm font-medium">{g.namespace}</span>
            {/* The count is here because the whole point of showing every
                namespace is comparison, and "staging has 12, production has 1"
                is the comparison people actually make. */}
            <span className="text-xs text-[var(--muted-foreground)]">
              {g.items.length}
            </span>
          </h2>
          {children(g.items, g.namespace)}
        </section>
      ))}
    </div>
  );
}

/**
 * NamespaceTag names the namespace on something shown outside its group.
 *
 * For the places where grouping does not apply — a table sorted by time, a
 * single card in a mixed list — where the namespace is still the difference
 * between two identically named Gates.
 */
export function NamespaceTag({ namespace }: { namespace: string }) {
  return (
    <span className="rounded bg-[var(--secondary)] px-1.5 py-0.5 text-xs text-[var(--muted-foreground)]">
      {namespace}
    </span>
  );
}
