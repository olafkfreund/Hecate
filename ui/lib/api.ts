// The Hecate API, as the browser sees it.
//
// Same origin: the UI is a static export embedded in the hecate-api binary, so
// there is no base URL to configure, no CORS, and the session cookie set by
// /auth/callback is attached by the browser without anything here doing so.

export type Health = "Healthy" | "Progressing" | "Degraded" | "Unknown" | "NotApplicable";

export interface GateOccupant {
  bundle?: string;
  since?: string;
}

export interface HealthReport {
  status: Health;
  issues?: string[];
  observedAt?: string;
  since?: string;
}

export interface Admission {
  from: { beacon: string };
  /** Upstream Gates a Bundle must have cleared first. Empty is an entry point. */
  after?: string[];
  requireApproval?: boolean;
}

export interface Gate {
  metadata: { name: string; namespace: string };
  spec: { admits?: Admission[]; auto?: boolean; suspend?: boolean };
  status?: {
    current?: GateOccupant;
    health?: HealthReport;
    eligible?: string[];
    activePassage?: string;
  };
}

export interface ImageArtifact {
  repo: string;
  tag?: string;
  /** A tag can be moved; a digest cannot. This is what makes it auditable. */
  digest?: string;
}

export interface Artifact {
  image?: ImageArtifact;
  chart?: { repo?: string; name?: string; version?: string };
  commit?: { repo?: string; sha?: string; ref?: string };
}

/** One Bundle/Gate outcome, as recorded on the Bundle itself. */
export interface GateCrossing {
  gate: string;
  passage?: string;
  at: string;
  /** Who caused it — a person, or the controller for an automatic crossing. */
  actor?: string;
  /** Why it was blocked. */
  reason?: string;
}

export interface Bundle {
  metadata: { name: string; namespace: string; creationTimestamp?: string };
  spec: { beacon?: string; alias?: string; digest?: string; artifacts?: Artifact[] };
  status?: {
    cleared?: GateCrossing[];
    blocked?: GateCrossing[];
    approvedFor?: string[];
  };
}

export interface Passage {
  metadata: { name: string; namespace: string };
  spec: { gate: string; bundle: string; actor?: string };
  status?: {
    phase?: string;
    message?: string;
    startedAt?: string;
    finishedAt?: string;
    traceID?: string;
    steps?: { uses: string; phase: string; message?: string }[];
  };
}

/** Unauthenticated is thrown when the API says the caller is not signed in. */
export class Unauthenticated extends Error {
  constructor() {
    super("not signed in");
  }
}

/** ApiError carries the status so a page can distinguish forbidden from broken. */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

async function get<T>(path: string): Promise<T> {
  const res = await fetch(path, {
    headers: { Accept: "application/json" },
    // Belt and braces on a same-origin request, and correct if the UI is ever
    // served from somewhere else.
    credentials: "same-origin",
  });

  if (res.status === 401) throw new Unauthenticated();
  if (!res.ok) {
    // The API answers errors as JSON, but a proxy in front of it may not, so
    // falling back to the status text keeps the message useful either way.
    let detail = res.statusText;
    try {
      const body = (await res.json()) as { error?: string };
      if (body.error) detail = body.error;
    } catch {
      /* not JSON; the status text stands */
    }
    throw new ApiError(res.status, detail);
  }
  return (await res.json()) as T;
}

async function post<T>(path: string, body: unknown): Promise<T> {
  const res = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    credentials: "same-origin",
    body: JSON.stringify(body),
  });

  if (res.status === 401) throw new Unauthenticated();
  if (!res.ok) {
    let detail = res.statusText;
    try {
      const parsed = (await res.json()) as { error?: string };
      if (parsed.error) detail = parsed.error;
    } catch {
      /* not JSON; the status text stands */
    }
    throw new ApiError(res.status, detail);
  }
  return (await res.json()) as T;
}

const base = (ns: string) => `/api/v1alpha1/namespaces/${encodeURIComponent(ns)}`;

export const api = {
  gates: (ns: string) => get<Gate[]>(`${base(ns)}/gates`),
  gate: (ns: string, name: string) => get<Gate>(`${base(ns)}/gates/${encodeURIComponent(name)}`),
  explain: (ns: string, name: string) =>
    get<Explanation>(`${base(ns)}/gates/${encodeURIComponent(name)}/explain`),
  bundles: (ns: string) => get<Bundle[]>(`${base(ns)}/bundles`),
  bundle: (ns: string, name: string) =>
    get<Bundle>(`${base(ns)}/bundles/${encodeURIComponent(name)}`),
  passages: (ns: string) => get<Passage[]>(`${base(ns)}/passages`),
  health: () => get<{ status: string; version: string }>("/healthz"),

  /**
   * promote asks a Gate to cross a Bundle.
   *
   * No actor is sent: the API takes it from the authenticated caller and
   * ignores anything the body claims, because a promotion is a compliance
   * record and a self-declared identity is not one.
   */
  promote: (ns: string, gate: string, bundle: string) =>
    post<Passage>(`${base(ns)}/gates/${encodeURIComponent(gate)}/promote`, { bundle }),

  /**
   * approve records that a human has approved a Bundle for a Gate.
   *
   * A separate permission from promoting, and deliberately so: an approval the
   * promoter can grant themselves is not an approval. The API enforces that
   * with different Kubernetes verbs, so a 403 here is the cluster saying you
   * may cross but not approve — which is the control working, not a fault.
   */
  approve: (ns: string, bundle: string, gate: string) =>
    post<unknown>(`${base(ns)}/bundles/${encodeURIComponent(bundle)}/approve`, { gate }),
};

/**
 * These mirror pkg/ops exactly, field for field.
 *
 * Hand-written rather than generated, and therefore able to drift — the first
 * version of this file invented `reason` and `remedy` where the Go type has
 * `kind` and `fix`, and TypeScript was perfectly happy because nothing checks a
 * response against a schema at runtime. Worth generating from the Go types
 * eventually; until then, changing pkg/ops means changing this.
 */
export interface Blocker {
  /** A stable code, so the UI can branch without parsing prose. */
  kind: string;
  detail: string;
  /** What would unblock it, when there is a single obvious answer. */
  fix?: string;
}

/** Stable codes for why a Bundle may not cross. Mirrors pkg/gate's Code. */
export type WaitingKind = "AlreadyCurrent" | "UpstreamNotCleared" | "AwaitingApproval";

export interface Waiting {
  bundle: string;
  reason: string;
  /** The same verdict as an identifier, so this can be filtered on. */
  kind?: WaitingKind;
}

export interface Explanation {
  gate: string;
  namespace: string;
  state: string;
  summary: string;
  blockers?: Blocker[];
  current?: string;
  eligible?: string[];
  waiting?: Waiting[];
  health?: Health;
}

/**
 * signIn sends the browser to the API's OIDC flow.
 *
 * A full navigation rather than a fetch: the provider answers with redirects
 * and its own login page, neither of which can happen inside XHR.
 */
export function signIn(): void {
  window.location.href = "/auth/login";
}
