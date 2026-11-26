import type { DetectedPort } from "@/lib/ports";

// ServerPorts is what a deploy form needs to choose a host port without a
// collision: ports in use on the server (listening now, or claimed by another
// berth deployment there), and this deployment's own current port, which a
// redeploy keeps.
export type ServerPorts = { taken: Set<number>; current: number | null };

const EMPTY: ServerPorts = { taken: new Set(), current: null };

// fetchServerPorts asks the backend (which checks the server over SSH) for its
// ports. It returns no ports on any failure or for a server that isn't set up,
// so the form simply skips collision checks rather than breaking.
export async function fetchServerPorts(
  serverId: string,
  target: { owner: string; repo: string; environment: string },
  signal?: AbortSignal,
): Promise<ServerPorts> {
  if (!serverId) return EMPTY;
  try {
    const qs = new URLSearchParams(target);
    const resp = await fetch(`/api/servers/${serverId}/ports?${qs}`, { cache: "no-store", signal });
    if (!resp.ok) return EMPTY;
    const data = (await resp.json()) as { listening?: number[]; reserved?: number[]; current?: number };
    return {
      taken: new Set([...(data.listening ?? []), ...(data.reserved ?? [])]),
      current: data.current && data.current > 0 ? data.current : null,
    };
  } catch {
    return EMPTY;
  }
}

// AUTO_PORT_RANGE is where berth picks host ports for apps.
export const AUTO_PORT_MIN = 3000;
export const AUTO_PORT_MAX = 9999;

// pickFreePort returns preferred when it is free, else the first free port in
// the auto range starting from preferred (wrapping around), or null when the
// whole range is taken.
export function pickFreePort(preferred: number, taken: Set<number>): number | null {
  const start = preferred >= AUTO_PORT_MIN && preferred <= AUTO_PORT_MAX ? preferred : AUTO_PORT_MIN;
  const span = AUTO_PORT_MAX - AUTO_PORT_MIN + 1;
  for (let i = 0; i < span; i++) {
    const p = AUTO_PORT_MIN + ((start - AUTO_PORT_MIN + i) % span);
    if (!taken.has(p)) return p;
  }
  return null;
}

export type AssignedPort = { value: string; movedFrom?: string; current?: boolean };

// assignPorts picks a host port for every editable port the operator hasn't
// typed: the deployment's current port on a redeploy (first port), else its
// default when free (and not a privileged port), else the first free port in
// 3000-9999. Fixed and typed ports count as used, so two ports never collide.
export function assignPorts(
  detected: DetectedPort[],
  server: ServerPorts,
  isDocker: boolean,
  touched: Set<string>,
  values: Record<string, string>,
): Record<string, AssignedPort> {
  const out: Record<string, AssignedPort> = {};
  const used = new Set(server.taken);
  for (const p of detected) {
    const key = p.envVar || p.container;
    if (!(isDocker || p.editable)) {
      if (p.host) used.add(Number(p.host));
    } else if (touched.has(key)) {
      used.add(Number(values[key]));
    }
  }
  let first = true;
  for (const p of detected) {
    if (!(isDocker || p.editable)) continue;
    const key = p.envVar || p.container;
    if (touched.has(key)) continue;
    const def = values[key] || p.host || p.container;
    let chosen: number | null;
    let current = false;
    if (first && server.current && !used.has(server.current)) {
      chosen = server.current;
      current = true;
    } else if (/^\d+$/.test(def) && Number(def) >= 1024 && !used.has(Number(def))) {
      chosen = Number(def);
    } else {
      chosen = pickFreePort(Number(def), used);
    }
    first = false;
    if (chosen === null) continue;
    used.add(chosen);
    out[key] = { value: String(chosen), movedFrom: !current && String(chosen) !== def ? def : undefined, current };
  }
  return out;
}
