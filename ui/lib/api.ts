// The Hecate API, as the browser sees it.
//
// Same origin: the UI is a static export embedded in the hecate-api binary, so
// there is no base URL to configure, no CORS, and the session cookie set by
// /auth/callback is attached by the browser without anything here doing so.

export type Health = "Healthy" | "Progressing" | "Degraded" | "Unknown" | "NotApplicable";

export interface GateOccupant {
  bundle?: string;
  /** When it entered. Named for the Go field: `since` was invented here, was
   *  never sent, and silently rendered nothing for three releases. */
  enteredAt?: string;
  digest?: string;
  passage?: string;
  actor?: string;
  verified?: boolean;
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

/**
 * StepStatus is one step's outcome.
 *
 * `reason` is carried alongside `message` rather than instead of it: the CRD
 * defines message for a human reading one failure and reason as a stable code
 * for anything reasoning across many, and a detail page showing only the prose
 * cannot answer "is this the same problem as yesterday?".
 */
export interface StepStatus {
  uses: string;
  as?: string;
  phase: string;
  reason?: string;
  message?: string;
  startedAt?: string;
  finishedAt?: string;
  attempts?: number;
}

export interface Passage {
  metadata: { name: string; namespace: string };
  spec: { gate: string; bundle: string; actor?: string; abort?: boolean };
  status?: {
    phase?: string;
    message?: string;
    startedAt?: string;
    finishedAt?: string;
    traceID?: string;
    currentStep?: number;
    steps?: StepStatus[];
  };
}

/**
 * AuthorPassageRequest is a whole Passage plus where to open a pull request
 * for it (hecate#172 stage 2, D58). `steps` is the same `ComposedStep[]`
 * shape `stepsYAML` (ui/lib/yaml.ts) renders for the live preview — the
 * server renders the copy that actually reaches git, from the identical
 * `v1alpha1.Step` type (D59).
 */
export interface AuthorPassageRequest {
  name: string;
  gate: string;
  bundle: string;
  steps: { uses: string; as?: string; with?: Record<string, unknown> }[];

  repo: string;
  path: string;
  /** Refuse by default when `path` already exists on the target branch — set
   *  this only to deliberately replace it. */
  overwrite?: boolean;
  base?: string;
  head?: string;

  provider?: string;
  baseURL?: string;
  title?: string;
  body?: string;
  labels?: string[];

  credentialsRef: string;
}

/** AuthoredPullRequest is what opening one returns. */
export interface AuthoredPullRequest {
  number: number;
  url: string;
  state: string;
  branch: string;
  repo: string;
}

/**
 * StepProblem is one thing wrong with a step list, in the shape a form can
 * attach to the offending row: which step (`index`, `uses`), and why
 * (`message`). Mirrors `passage.StepProblem` (`pkg/passage/step.go`) as
 * `/passages/validate` and `/passages/author` both send it — the same
 * `Registry.Validate` runs behind both, so the browser and the admission
 * path refuse the same things for the same reasons (hecate#172 scope item 4).
 */
export interface StepProblem {
  index: number;
  uses?: string;
  message: string;
}

/**
 * Beacon is a source Hecate watches, and the thing that emits Bundles.
 *
 * `spec.watch` is kept as the discriminated union the CRD defines rather than
 * flattened to a label: a Beacon may watch several sources of different kinds,
 * and which kind it is decides what identifies it — a repo for an image, a repo
 * and chart name for a chart.
 */
export interface WatchSource {
  image?: { repo: string; constraint?: string; platform?: string };
  chart?: { repo: string; name?: string; constraint?: string };
  git?: { repo: string };
  provider?: { name: string };
}

export interface Beacon {
  metadata: { name: string; namespace: string };
  spec: {
    interval?: string;
    watch?: WatchSource[];
    emit?: string;
    suspend?: boolean;
  };
  status?: {
    lastPolled?: string;
    latestBundle?: string;
    lastHandledReconcileAt?: string;
    conditions?: { type: string; status: string; reason?: string; message?: string }[];
  };
}


/**
 * Overview is every Gate the caller can see, across every namespace.
 *
 * Mirrors pkg/ops/overview.go. Hand-written like the rest of this file, and
 * therefore able to drift — see the note below.
 */
export interface GateSummary {
  name: string;
  health: Health;
  issues?: string[];
  current?: string;
  eligible: number;
  running?: string;
  suspended: boolean;
}

export interface NamespaceOverview {
  namespace: string;
  gates: GateSummary[];
}

export interface Totals {
  gates: number;
  healthy: number;
  progressing: number;
  degraded: number;
  unknown: number;
  suspended: number;
  eligible: number;
  running: number;
  failed: number;
}

/** Day is one day's crossings and failures, for the trend. */
export interface Day {
  date: string;
  crossed: number;
  failed: number;
}

export interface Overview {
  namespaces: NamespaceOverview[];
  totals: Totals;
  activity: Day[];
}

/**
 * Preflight is what the evidence gate would say about a Bundle, before anyone
 * presses Cross.
 */
export interface Preflight {
  bundle: string;
  compliant: boolean;
  missing?: string[];
  policies?: string[];
  unknown?: string;
}

/** FluxResource is one Flux object a Gate depends on. */
export interface FluxResource {
  kind: string;
  name: string;
  namespace: string;
  suspended: boolean;
  health: Health;
  detail?: string;
  revision?: string;
  lastHandled?: string;
  missing: boolean;
}

/**
 * StepSchema is one step's `with:` block, as JSON Schema.
 *
 * Recursive rather than flattened, because the real schemas nest — an
 * `http` step's `secretHeaders` is an array of objects, one of whose fields
 * is itself an object (`secretRef`). Pinned to what `invopop/jsonschema`
 * actually emits (`pkg/passage/steps/schema.go`), not the wider spec: no
 * `enum`, no `oneOf`, no `$ref` — the generator does not produce them today,
 * and a type that accepted them would be a promise the form does not keep.
 */
export interface StepSchema {
  type?: "string" | "integer" | "number" | "boolean" | "array" | "object";
  description?: string;
  properties?: Record<string, StepSchema>;
  required?: string[];
  items?: StepSchema;
  /** `true`/`false` bars extra keys; an object schema means "a map of this
   *  shape" (only string-valued maps occur today, e.g. `http.headers`). */
  additionalProperties?: boolean | StepSchema;
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


/** AuditEntry is one thing that happened, in terms an auditor asks about. */
export type AuditEntry = {
  at: string;
  kind: "crossed" | "refused" | "running" | "approved";
  /** Which namespace it happened in. A Gate name is not unique across them. */
  namespace: string;
  gate: string;
  bundle?: string;
  digest?: string;
  actor?: string;
  passage?: string;
  detail?: string;
  verified?: boolean;
  evidence?: { trail?: string; verdict?: string; risk?: number; blockers?: string[]; url?: string };
};

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
  clusters: { secret: string; gates: string[]; reachable: boolean; detail?: string }[];
  telemetry: { endpoint?: string; configured: boolean };
  home: { inCluster: boolean; server?: string; namespace?: string };
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

  /**
   * stepSchemas is the catalogue a Passage author picks from: every step
   * `uses` can name, keyed the same way, each with its `with:` block as JSON
   * Schema (#114).
   *
   * Not namespaced — see the handler's own comment — and not live: the
   * catalogue is the controller's build, not cluster state, so there is
   * nothing for `useLiveApi` to watch for.
   */
  stepSchemas: () => get<Record<string, StepSchema>>("/api/v1alpha1/steps"),

  /**
   * overview is every Gate this person can see, in one call.
   *
   * Cluster-wide and not namespaced, because "how is everything" is not a
   * question about one namespace. The server filters to what the caller may
   * read rather than refusing the whole answer — a board that 403s because one
   * namespace is out of reach is useless to a team-scoped operator.
   */
  overview: () => get<Overview>("/api/v1alpha1/overview"),

  /**
   * audit is what happened, newest first.
   *
   * Assembled server-side from Gate history, Passages and approvals, because
   * those three age out differently and reconciling them in the browser would
   * mean the page quietly disagreeing with itself depending on what was still
   * in the cluster when it loaded.
   */
  /**
   * audit is what happened, newest first, across every namespace you can read.
   *
   * Not per-namespace: an audit answers "what happened", and making that a
   * question about one namespace at a time meant you could only ask it about a
   * place you had already guessed.
   */
  audit: () => get<AuditEntry[]>("/api/v1alpha1/audit"),
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

  /**
   * gates, bundles, beacons and passages are every one you can read, grouped by
   * namespace by the server.
   *
   * One call rather than a request per namespace: the cost of a cluster-wide
   * List is the same whether you can see one namespace or forty, while a loop
   * would make the page slower the more of the cluster you are trusted with.
   */
  gates: () => get<Gate[]>("/api/v1alpha1/gates"),
  gate: (ns: string, name: string) => get<Gate>(`${base(ns)}/gates/${encodeURIComponent(name)}`),
  explain: (ns: string, name: string) =>
    get<Explanation>(`${base(ns)}/gates/${encodeURIComponent(name)}/explain`),

  /**
   * preflight asks whether each eligible Bundle would actually cross.
   *
   * A separate call from explain, deliberately: explain loads on every page
   * view and again on every live update, and this is one round-trip to Fides
   * per eligible Bundle. Folding it in would make the page's refresh rate the
   * rate at which Hecate polls Fides.
   */
  preflight: (ns: string, gate: string) =>
    get<Preflight[]>(`${base(ns)}/gates/${encodeURIComponent(gate)}/preflight`),

  /** flux is what this Gate's crossings actually depend on. */
  flux: (ns: string, gate: string) =>
    get<FluxResource[]>(`${base(ns)}/gates/${encodeURIComponent(gate)}/flux`),

  /**
   * suspendFlux stops or restarts Flux reconciling one resource.
   *
   * A separate permission from promoting, and a bigger one: suspending stops
   * every future deploy of a resource and is cluster state git will not
   * restore, so it outlives whoever did it.
   */
  suspendFlux: (ns: string, gate: string, kind: string, name: string, suspend: boolean) =>
    post<{ kind: string; name: string; suspended: boolean; by: string }>(
      `${base(ns)}/gates/${encodeURIComponent(gate)}/flux/suspend`,
      { kind, name, suspend },
    ),

  /** reconcileFlux asks Flux to look at one resource now. */
  reconcileFlux: (ns: string, gate: string, kind: string, name: string) =>
    post<{ requestedAt: string }>(
      `${base(ns)}/gates/${encodeURIComponent(gate)}/flux/reconcile`,
      { kind, name },
    ),
  bundles: () => get<Bundle[]>("/api/v1alpha1/bundles"),
  bundle: (ns: string, name: string) =>
    get<Bundle>(`${base(ns)}/bundles/${encodeURIComponent(name)}`),
  evidence: (ns: string, name: string) =>
    get<Evidence>(`${base(ns)}/bundles/${encodeURIComponent(name)}/evidence`),
  passages: () => get<Passage[]>("/api/v1alpha1/passages"),
  passage: (ns: string, name: string) =>
    get<Passage>(`${base(ns)}/passages/${encodeURIComponent(name)}`),

  beacons: () => get<Beacon[]>("/api/v1alpha1/beacons"),
  beacon: (ns: string, name: string) =>
    get<Beacon>(`${base(ns)}/beacons/${encodeURIComponent(name)}`),

  /**
   * poll asks a Beacon to look at its sources now.
   *
   * The returned token is `status.lastHandledReconcileAt` once the controller
   * has acted, so a caller can tell its own request landed rather than watching
   * for any change and hoping.
   */
  poll: (ns: string, beacon: string) =>
    post<{ requestedAt: string }>(
      `${base(ns)}/beacons/${encodeURIComponent(beacon)}/poll`,
      {},
    ),

  /**
   * abort stops a Passage that is running.
   *
   * A separate permission from promoting, like approving is: being allowed to
   * start a crossing does not imply being allowed to stop one half-way, which
   * can leave the target in a state neither the old nor the new Bundle
   * describes.
   */
  abort: (ns: string, passage: string) =>
    post<{ passage: string; aborted: boolean; abortedBy: string }>(
      `${base(ns)}/passages/${encodeURIComponent(passage)}/abort`,
      {},
    ),

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

  /**
   * authorPassage renders a whole Passage manifest, commits it to a new
   * branch of `repo`, and opens a pull request through `pkg/provider`
   * (hecate#172 stage 2).
   *
   * Never applies anything: the manifest exists only in the pull request
   * until a human merges it, and Flux picks it up from there — see the
   * `/passages/new` page and docs/DECISIONS.md D58.
   */
  authorPassage: (ns: string, req: AuthorPassageRequest) =>
    post<AuthoredPullRequest>(`${base(ns)}/passages/author`, req),

  /**
   * validatePassage reports what is wrong with a step list, using the same
   * `Registry.Validate` `authorPassage` refuses on — this endpoint never
   * refuses the request itself (always 200): it is feedback for a form still
   * being edited, not a gate (hecate#172 scope item 4).
   *
   * Not namespaced: Validate reads no cluster state, only the controller's
   * own step catalogue, so the answer is the same everywhere — see the
   * handler's own comment (`pkg/api/validate.go`).
   */
  validatePassage: (steps: AuthorPassageRequest["steps"]) =>
    post<{ problems: StepProblem[] }>("/api/v1alpha1/passages/validate", { steps }),
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
/**
 * ChangeRequest is the ITSM change governing a crossing, as attested.
 *
 * Read from the trail's servicenow-change attestation rather than live from
 * ServiceNow: the attestation is what the gate judged, and a change closed
 * since would make the record claim a decision nobody made.
 */
export interface ChangeRequest {
  number: string;
  state: string;
  approval: string;
  risk?: string;
  on_hold: boolean;
  short_description?: string;
  found: boolean;
}

export interface Evidence {
  bundle: string;
  namespace: string;
  digest?: string;
  trail?: string;
  gate?: string;
  verdict?: ChangeVerdict;
  /** The ITSM change governing this crossing, when one was checked. */
  change?: ChangeRequest;
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
