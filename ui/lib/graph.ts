import type { Gate, Health } from "@/lib/api";

/**
 * The pipeline graph, derived rather than declared.
 *
 * There is no pipeline object in Hecate — the graph is implied by what each
 * Gate admits, so it cannot drift out of sync with the Gates it describes. That
 * design decision is why this file exists: the shape has to be *computed* from
 * `spec.admits`, and nothing stores it.
 *
 * An edge is `beacon → gate` where an admission has no `after`, and
 * `gate → gate` for every name in one.
 */

export interface Node {
  id: string;
  label: string;
  kind: "beacon" | "gate";
  health?: Health;
  current?: string;
  /** Column, from the sources. */
  rank: number;
  x: number;
  y: number;
}

export interface Edge {
  from: string;
  to: string;
  /** A crossing here needs a human. Worth showing: it is where the gate is. */
  approval?: boolean;
}

export interface Graph {
  nodes: Node[];
  edges: Edge[];
  width: number;
  height: number;
}

export const NODE_W = 176;
export const NODE_H = 58;
const COL_GAP = 88;
const ROW_GAP = 20;

export function build(gates: Gate[]): Graph {
  const nodes = new Map<string, Node>();
  const edges: Edge[] = [];

  const add = (id: string, label: string, kind: Node["kind"]) => {
    if (!nodes.has(id)) {
      nodes.set(id, { id, label, kind, rank: 0, x: 0, y: 0 });
    }
    return nodes.get(id)!;
  };

  for (const g of gates) {
    const node = add(`gate/${g.metadata.name}`, g.metadata.name, "gate");
    node.health = g.status?.health?.status;
    node.current = g.status?.current?.bundle;

    for (const a of g.spec.admits ?? []) {
      if (a.after?.length) {
        for (const up of a.after) {
          add(`gate/${up}`, up, "gate");
          edges.push({ from: `gate/${up}`, to: node.id, approval: a.requireApproval });
        }
      } else if (a.from?.beacon) {
        add(`beacon/${a.from.beacon}`, a.from.beacon, "beacon");
        edges.push({
          from: `beacon/${a.from.beacon}`,
          to: node.id,
          approval: a.requireApproval,
        });
      }
    }
  }

  rank(nodes, edges);
  return position(nodes, edges);
}

/**
 * rank puts each node one column past its furthest upstream.
 *
 * Longest path rather than shortest, so a Gate admitting from both a Beacon and
 * another Gate sits after the Gate — drawing it beside its own upstream would
 * suggest they are alternatives.
 *
 * `visiting` makes a cycle terminate rather than recurse for ever. Hecate does
 * not stop anyone writing one, and a UI that hangs on bad configuration is a
 * worse answer than a UI that draws it oddly.
 */
function rank(nodes: Map<string, Node>, edges: Edge[]): void {
  const upstream = new Map<string, string[]>();
  for (const e of edges) {
    upstream.set(e.to, [...(upstream.get(e.to) ?? []), e.from]);
  }

  const done = new Map<string, number>();
  const visiting = new Set<string>();

  const depth = (id: string): number => {
    if (done.has(id)) return done.get(id)!;
    if (visiting.has(id)) return 0;
    visiting.add(id);
    const ups = upstream.get(id) ?? [];
    const d = ups.length === 0 ? 0 : Math.max(...ups.map((u) => depth(u) + 1));
    visiting.delete(id);
    done.set(id, d);
    return d;
  };

  for (const node of nodes.values()) node.rank = depth(node.id);
}

/** position lays the ranks out in columns, each centred vertically. */
function position(nodes: Map<string, Node>, edges: Edge[]): Graph {
  const columns = new Map<number, Node[]>();
  for (const n of nodes.values()) {
    columns.set(n.rank, [...(columns.get(n.rank) ?? []), n]);
  }

  let tallest = 0;
  for (const column of columns.values()) {
    // Stable order, so the same cluster always draws the same way. An API
    // listing is not guaranteed to keep its order between calls, and a graph
    // that reshuffles on refresh is unreadable.
    column.sort((a, b) => a.label.localeCompare(b.label));
    tallest = Math.max(tallest, column.length);
  }

  const height = Math.max(1, tallest) * (NODE_H + ROW_GAP) - ROW_GAP;
  for (const [r, column] of columns) {
    const span = column.length * (NODE_H + ROW_GAP) - ROW_GAP;
    column.forEach((n, i) => {
      n.x = r * (NODE_W + COL_GAP);
      n.y = (height - span) / 2 + i * (NODE_H + ROW_GAP);
    });
  }

  const width = Math.max(1, columns.size) * (NODE_W + COL_GAP) - COL_GAP;
  return { nodes: [...nodes.values()], edges, width, height };
}
