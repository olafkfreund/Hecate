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

export interface Gate {
  metadata: { name: string; namespace: string };
  spec: Record<string, unknown>;
  status?: {
    current?: GateOccupant;
    health?: HealthReport;
    eligible?: string[];
    activePassage?: string;
  };
}

export interface Bundle {
  metadata: { name: string; namespace: string; creationTimestamp?: string };
  spec: { beacon?: string; alias?: string; digest?: string };
  status?: { approvedFor?: string[] };
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

const base = (ns: string) => `/api/v1alpha1/namespaces/${encodeURIComponent(ns)}`;

export const api = {
  gates: (ns: string) => get<Gate[]>(`${base(ns)}/gates`),
  gate: (ns: string, name: string) => get<Gate>(`${base(ns)}/gates/${encodeURIComponent(name)}`),
  explain: (ns: string, name: string) =>
    get<Explanation>(`${base(ns)}/gates/${encodeURIComponent(name)}/explain`),
  bundles: (ns: string) => get<Bundle[]>(`${base(ns)}/bundles`),
  passages: (ns: string) => get<Passage[]>(`${base(ns)}/passages`),
  health: () => get<{ status: string; version: string }>("/healthz"),
};

export interface Blocker {
  reason: string;
  detail?: string;
  remedy?: string;
}

export interface Explanation {
  gate: string;
  state: string;
  summary?: string;
  health?: Health;
  blockers?: Blocker[];
  eligible?: string[];
  waiting?: { bundle: string; reason: string }[];
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
