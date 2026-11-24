"use client";

import { useEffect, useState } from "react";
import { ChevronDown } from "@/components/icons";
import type { ServerView } from "@/lib/servers";
import { envRank } from "@/lib/status";

export type Pick = { environment?: string; serverId: string };

type Target = { environment: string; serverId: string; source: string };
type RepoTargets = { clientId: string; clientName: string; permission: string; targets: Target[] };

// TargetSelect is the deploy bar's "where": the repo's environments as the
// admins set them up, each already pointing at its server. Admins can also
// pick any ready server directly; developers deploy where the rules say.
export function TargetSelect({
  repo,
  servers,
  isAdmin,
  value,
  onChange,
}: {
  repo: string; // owner/name
  servers: ServerView[];
  isAdmin: boolean;
  value: Pick | null;
  onChange: (p: Pick | null) => void;
}) {
  const [open, setOpen] = useState(false);
  const [state, setState] = useState<{ repo: string; data: RepoTargets | null } | null>(null);

  useEffect(() => {
    const ctrl = new AbortController();
    const [owner, name] = repo.split("/");
    fetch(`/api/repos/${encodeURIComponent(owner)}/${encodeURIComponent(name)}/targets`, {
      cache: "no-store",
      signal: ctrl.signal,
    })
      .then((r) => (r.ok ? r.json() : null))
      .then((data: RepoTargets | null) => {
        if (ctrl.signal.aborted) return;
        setState({ repo, data });
        // One environment means there is nothing to choose.
        const ts = data?.targets ?? [];
        if (ts.length === 1) onChange({ environment: ts[0].environment, serverId: ts[0].serverId });
      })
      .catch(() => {
        if (!ctrl.signal.aborted) setState({ repo, data: null });
      });
    return () => ctrl.abort();
    // onChange is a setter from the parent; re-running on it would refetch.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [repo]);

  const loading = state?.repo !== repo;
  const data = loading ? null : state?.data ?? null;
  const byId = new Map(servers.map((s) => [s.id, s]));
  const targets = [...(data?.targets ?? [])].sort(
    (a, b) => envRank(a.environment) - envRank(b.environment) || a.environment.localeCompare(b.environment),
  );
  // A client's server only runs that client's code, admins included, so only
  // shared servers and this project's client's own are offered.
  const readyServers = servers.filter(
    (s) => s.status === "ready" && (!s.clientId || s.clientId === data?.clientId),
  );
  const nameOf = (id: string) => byId.get(id)?.name ?? "unknown server";

  const label = loading
    ? "Loading environments…"
    : value
      ? value.environment
        ? `${value.environment} → ${nameOf(value.serverId)}`
        : `Server: ${nameOf(value.serverId)}`
      : targets.length > 0 || isAdmin
        ? "Choose an environment…"
        : "No environments set up";

  const nothing = !loading && targets.length === 0 && !isAdmin;

  return (
    <div className="relative w-full sm:w-auto">
      <button
        type="button"
        onClick={() => !loading && !nothing && setOpen((o) => !o)}
        disabled={loading || nothing}
        title={nothing ? "Ask an admin to set up an environment for this project under Access." : undefined}
        className="flex w-full items-center justify-between gap-2 rounded-md border border-line bg-surface px-3 py-2.5 text-[13px] transition-colors hover:border-line-strong focus:border-line-strong focus:outline-none disabled:opacity-60 sm:min-w-[17rem] sm:py-2"
      >
        <span className={value ? "text-cream" : "text-muted"}>{label}</span>
        <ChevronDown className={`text-muted transition-transform ${open ? "rotate-180" : ""}`} />
      </button>
      {nothing && (
        <p className="mt-1.5 text-[12px] text-muted sm:absolute sm:right-0 sm:top-full sm:w-72 sm:text-right">
          No environment is set up for this project yet. Ask an admin to add one.
        </p>
      )}

      {open && (
        <>
          <div className="fixed inset-0 z-10" onClick={() => setOpen(false)} />
          <div className="absolute bottom-full left-0 right-0 z-20 mb-1 max-h-80 overflow-y-auto rounded-lg border border-line bg-surface py-1 shadow-xl">
            {targets.length > 0 && (
              <p className="px-3 pb-1 pt-2 text-[11px] font-medium text-faint">
                {data?.clientName ? `${data.clientName} environments` : "Environments"}
              </p>
            )}
            {targets.map((t) => {
              const srv = byId.get(t.serverId);
              const usable = srv?.status === "ready";
              const active = value?.environment === t.environment && value.serverId === t.serverId;
              return (
                <button
                  key={t.environment}
                  type="button"
                  disabled={!usable}
                  onClick={() => {
                    onChange({ environment: t.environment, serverId: t.serverId });
                    setOpen(false);
                  }}
                  className={`flex w-full items-center justify-between gap-3 px-3 py-2 text-left text-[13px] transition-colors disabled:opacity-50 ${
                    active ? "bg-surface-hover text-cream" : "text-body hover:bg-surface-hover hover:text-cream"
                  }`}
                >
                  <span className="font-medium">{t.environment}</span>
                  <span className="text-[12px] text-muted">
                    {srv ? srv.name : "unknown server"}
                    {srv && !usable ? " · not ready" : ""}
                  </span>
                </button>
              );
            })}
            {isAdmin && (
              <>
                <p className="border-t border-line px-3 pb-1 pt-2 text-[11px] font-medium text-faint">
                  Any server (admin)
                </p>
                {readyServers.length === 0 && <p className="px-3 py-2 text-[12px] text-muted">No ready servers.</p>}
                {readyServers.map((s) => (
                  <button
                    key={s.id}
                    type="button"
                    onClick={() => {
                      onChange({ serverId: s.id });
                      setOpen(false);
                    }}
                    className={`flex w-full items-center justify-between gap-3 px-3 py-2 text-left text-[13px] transition-colors ${
                      value && !value.environment && value.serverId === s.id
                        ? "bg-surface-hover text-cream"
                        : "text-body hover:bg-surface-hover hover:text-cream"
                    }`}
                  >
                    <span>{s.name}</span>
                    <span className="data text-[12px] text-muted">{s.ip}</span>
                  </button>
                ))}
              </>
            )}
          </div>
        </>
      )}
    </div>
  );
}
