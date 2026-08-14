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

/** One human's sign-off for one Gate. Carries the approver, because an
 * approval that does not say who gave it cannot satisfy four-eyes. */
export interface BundleApproval {
  gate: string;
  actor: string;
  at: string;
}

export interface Bundle {
  metadata: { name: string; namespace: string; creationTimestamp?: string };
  spec: { beacon?: string; alias?: string; digest?: string; artifacts?: Artifact[] };
  status?: {
    cleared?: GateCrossing[];
    blocked?: GateCrossing[];
    approvedFor?: BundleApproval[];
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

/**
 * put is post with a different verb.
 *
 * Written out rather than folded into post with a parameter: the two differ only in
 * one word, and a shared helper taking a method reads worse at every call site
 * than two named ones.
 */
async function put<T>(path: string, body: unknown): Promise<T> {
  const res = await fetch(path, {
    method: "PUT",
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


/** Settings is what the settings screen shows, derived from cluster state. */
export type Settings = {
  version: string;
  identity: { name: string; groups?: string[] };
  fides: {
    serverURL: string;
    gates: string[];
    environments?: string[];
    reachable: boolean;
    detail?: string;
  }[];
  clusters: { secret: string; gates: string[] }[];
  telemetry: { endpoint?: string; configured: boolean };
};

/** Grant is one person (or group) holding one Hecate role. */
export type Grant = {
  binding: string;
  role: string;
  kind: string;
  subject: string;
  grantedBy?: string;
};

export const api = {
  /**
   * namespaces is where this user can look.
   *
   * Server-side discovery rather than a list the UI keeps: only the API knows
   * which namespaces hold Gates or Beacons, and only it can check which of
   * those this caller may read. A picker built from anything else either offers
   * namespaces that 403 on click or misses the one you wanted.
   */
  namespaces: () => get<{ namespaces: string[] }>("/api/v1alpha1/namespaces"),

  settings: () => get<Settings>("/api/v1alpha1/settings"),
  grants: () => get<{ grants: Grant[] }>("/api/v1alpha1/rbac/grants"),

  /**
   * grant gives someone a Hecate role by creating a ClusterRoleBinding.
   *
   * Hecate has no user store, so this is what "add a user" means here: the
   * permission model stays Kubernetes RBAC and the result is visible to
   * `kubectl get clusterrolebinding`, rather than living in a second place that
   * eventually disagrees with the first.
   */
  grant: (subject: string, kind: string, role: string) =>
    post<{ binding: string; created: boolean }>("/api/v1alpha1/rbac/grants", {
      subject,
      kind,
      role,
    }),

  connectCluster: (ns: string, name: string, kubeconfig: string) =>
    post<{ secret: string; created: boolean }>(`${base(ns)}/clusters`, { name, kubeconfig }),

  /**
   * setEvidence points a Gate at an evidence server.
   *
   * Writes to the cluster. A Gate reconciled from git will be restored to its
   * committed value on the next Flux sync — the screen says so, because
   * otherwise this looks like the change being lost.
   */
  setEvidence: (ns: string, gate: string, body: { serverURL: string; fidesEnvironment: string; credentialsRef?: string }) =>
    put<{ gate: string; note: string }>(`${base(ns)}/gates/${encodeURIComponent(gate)}/evidence`, body),

  gates: (ns: string) => get<Gate[]>(`${base(ns)}/gates`),
  gate: (ns: string, name: string) => get<Gate>(`${base(ns)}/gates/${encodeURIComponent(name)}`),
  explain: (ns: string, name: string) =>
    get<Explanation>(`${base(ns)}/gates/${encodeURIComponent(name)}/explain`),
  bundles: (ns: string) => get<Bundle[]>(`${base(ns)}/bundles`),
  bundle: (ns: string, name: string) =>
    get<Bundle>(`${base(ns)}/bundles/${encodeURIComponent(name)}`),
  evidence: (ns: string, name: string) =>
    get<Evidence>(`${base(ns)}/bundles/${encodeURIComponent(name)}/evidence`),
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

/** The change gate's verdict for a crossing. Mirrors v1alpha1.EvidenceRef. */
export interface EvidenceRef {
  trail?: string;
  /** "approve" or "hold". */
  verdict?: string;
  /** 0-100, higher being worse. */
  risk?: number;
  blockers?: string[];
  url?: string;
}

/** One control the change gate judged. Mirrors pkg/fides.Control. */
export interface Control {
  control: string;
  name: string;
  reasons?: string[];
  waived_reasons?: string[];
  reason?: string;
  approved_by?: string;
  expires_at?: string;
}

/** The change gate's full answer for a trail. Mirrors pkg/fides.ChangeVerdict. */
export interface ChangeVerdict {
  recommendation: string;
  approved: boolean;
  risk_score: number;
  risk_level: string;
  passed?: string[];
  failed?: Control[];
  missing_evidence?: Control[];
  waived?: Control[];
  attestations: { total: number; non_compliant: number };
  approvals: {
    count: number;
    human_approvers: number;
    four_eyes: boolean;
    approvers?: string[];
    deployers?: string[];
  };
  segregation_of_duties?: {
    committer?: string;
    approvers?: string[];
    deployers?: string[];
    compliant: boolean;
    violations?: string[];
  };
  summary?: string;
}

/** Everything Fides holds about a Bundle. Mirrors pkg/ops.Evidence. */
export interface Evidence {
  bundle: string;
  namespace: string;
  digest?: string;
  trail?: string;
  gate?: string;
  verdict?: ChangeVerdict;
  approvedIn?: BundleApproval[];
  /** Why there is nothing to show. Empty is not the same as a clean bill. */
  unavailable?: string;
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
  /** The change gate's verdict for the crossing in progress, if any. */
  evidence?: EvidenceRef;
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
