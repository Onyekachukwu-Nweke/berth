import type { Deployment } from "@/lib/data";

// One state per deployment, in the words the cockpit uses. The latest Actions
// run wins; without one (aged out, or GitHub unreachable) the latest recorded
// release decides.
export type DeployState = "ready" | "failing" | "deploying" | "pending" | "idle";

export function deployState(d: Deployment): DeployState {
  if (d.locked) return "deploying";
  const run = d.runs[0]?.status;
  if (run === "running") return "deploying";
  if (run === "failed") return "failing";
  if (run === "success") return "ready";
  switch (d.timeline?.[0]?.status) {
    case "success":
      return "ready";
    case "failed":
      return "failing";
    case "running":
      return "deploying";
    case "pending":
      return "pending";
  }
  if (d.status === "live") return "ready";
  if (d.status === "failing") return "failing";
  return "idle";
}

export const STATE_LABEL: Record<DeployState, string> = {
  ready: "Ready",
  failing: "Failed",
  deploying: "Deploying",
  pending: "Pending",
  idle: "No runs",
};

export const STATE_DOT: Record<DeployState, string> = {
  ready: "bg-ok",
  failing: "bg-err",
  deploying: "bg-warn animate-pulse",
  pending: "bg-faint",
  idle: "bg-faint",
};

export const STATE_TEXT: Record<DeployState, string> = {
  ready: "text-ok",
  failing: "text-err",
  deploying: "text-warn",
  pending: "text-muted",
  idle: "text-muted",
};

// minutesAgo turns the API's relative times ("25m ago", "3d ago", "just now")
// into minutes, so mixed lists sort newest first. Unknown sorts last.
export function minutesAgo(when: string): number {
  if (/just now|now/i.test(when)) return 0;
  const m = when.match(/(\d+)\s*([smhdw])/i);
  if (!m) return Number.POSITIVE_INFINITY;
  const n = Number(m[1]);
  const unit = m[2].toLowerCase();
  return unit === "s" ? n / 60 : unit === "m" ? n : unit === "h" ? n * 60 : unit === "d" ? n * 1440 : n * 10080;
}

// Environment ordering: production first, then staging, then the rest.
export function envRank(env: string): number {
  const e = env.toLowerCase();
  return e === "production" || e === "prod" ? 0 : e === "staging" ? 1 : 2;
}

export function StatusDot({ state }: { state: DeployState }) {
  return <span className={`inline-block h-2 w-2 shrink-0 rounded-full ${STATE_DOT[state]}`} />;
}
