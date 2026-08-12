"use client";

import Link from "next/link";
import { build, NODE_H, NODE_W, type Node } from "@/lib/graph";
import type { Gate, Health } from "@/lib/api";

const stroke: Record<Health, string> = {
  Healthy: "var(--color-healthy)",
  Progressing: "var(--color-progressing)",
  Degraded: "var(--color-degraded)",
  Unknown: "var(--color-unknown)",
  NotApplicable: "var(--line)",
};

/**
 * The pipeline, drawn from what the Gates admit.
 *
 * Hand-rolled SVG rather than a graph library: the layout is computed in
 * lib/graph.ts, so every coordinate is known before anything renders and there
 * is nothing to measure, no ref, no effect, and no second render. A library
 * would bring a layout engine to arrange four boxes.
 */
export function PipelineGraph({ gates, namespace }: { gates: Gate[]; namespace: string }) {
  const g = build(gates);
  if (g.nodes.length === 0) return null;

  const pad = 8;
  return (
    <figure className="overflow-x-auto">
      <svg
        viewBox={`${-pad} ${-pad} ${g.width + pad * 2} ${g.height + pad * 2}`}
        width={g.width + pad * 2}
        height={g.height + pad * 2}
        role="img"
        aria-label={describe(g.nodes)}
        className="max-w-full"
      >
        <defs>
          <marker
            id="arrow"
            viewBox="0 0 8 8"
            refX="7"
            refY="4"
            markerWidth="7"
            markerHeight="7"
            orient="auto-start-reverse"
          >
            <path d="M0,0 L8,4 L0,8 z" fill="var(--muted)" />
          </marker>
        </defs>

        {g.edges.map((e, i) => {
          const from = g.nodes.find((n) => n.id === e.from);
          const to = g.nodes.find((n) => n.id === e.to);
          if (!from || !to) return null;
          const x1 = from.x + NODE_W;
          const y1 = from.y + NODE_H / 2;
          const x2 = to.x;
          const y2 = to.y + NODE_H / 2;
          const mid = (x1 + x2) / 2;
          return (
            <g key={`${e.from}-${e.to}-${i}`}>
              <path
                d={`M${x1},${y1} C${mid},${y1} ${mid},${y2} ${x2},${y2}`}
                fill="none"
                stroke="var(--line)"
                strokeWidth="1.5"
                markerEnd="url(#arrow)"
              />
              {/* Where a human has to say yes. Drawn because it is the whole
                  point of the Gate, and invisible in a plain arrow. */}
              {e.approval && (
                <text
                  x={mid}
                  y={(y1 + y2) / 2 - 6}
                  textAnchor="middle"
                  className="fill-[var(--muted)] text-[10px]"
                >
                  approval
                </text>
              )}
            </g>
          );
        })}

        {g.nodes.map((n) => (
          <GraphNode key={n.id} node={n} namespace={namespace} />
        ))}
      </svg>
    </figure>
  );
}

function GraphNode({ node, namespace }: { node: Node; namespace: string }) {
  const body = (
    <g>
      <rect
        x={node.x}
        y={node.y}
        width={NODE_W}
        height={NODE_H}
        rx="8"
        fill="var(--raised)"
        stroke={node.kind === "gate" ? stroke[node.health ?? "Unknown"] : "var(--line)"}
        strokeWidth="1.5"
        // A Beacon is a source, not an environment; dashing it says so without
        // needing a legend.
        strokeDasharray={node.kind === "beacon" ? "4 3" : undefined}
      />
      <text x={node.x + 12} y={node.y + 23} className="fill-[var(--fg)] text-[13px] font-medium">
        {node.label}
      </text>
      <text x={node.x + 12} y={node.y + 41} className="fill-[var(--muted)] text-[11px]">
        {node.kind === "beacon" ? "beacon" : (node.current ?? "nothing yet")}
      </text>
    </g>
  );

  // Beacons have no detail page to go to.
  if (node.kind !== "gate") return body;
  return (
    <Link href={{ pathname: "/gates/", query: { name: node.label, namespace } }}>{body}</Link>
  );
}

/**
 * describe is the graph in words, for anyone who is not looking at it.
 *
 * An SVG of boxes is invisible to a screen reader without this, and the Gates
 * table below carries the same information in a form that reads properly — so
 * this is a summary rather than a transcription.
 */
function describe(nodes: Node[]): string {
  const gates = nodes.filter((n) => n.kind === "gate");
  const beacons = nodes.filter((n) => n.kind === "beacon");
  return (
    `Pipeline: ${beacons.length} beacon${beacons.length === 1 ? "" : "s"} feeding ` +
    `${gates.length} gate${gates.length === 1 ? "" : "s"} — ` +
    gates.map((g) => `${g.label} (${g.health ?? "Unknown"})`).join(", ") +
    ". The table below lists the same Gates."
  );
}
