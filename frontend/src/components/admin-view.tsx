"use client";

import { useRouter } from "next/navigation";
import { useState, type ReactNode } from "react";
import { formatDay } from "@/lib/dates";

type Client = { id: string; name: string };
type RepoLink = { owner: string; repo: string; clientId: string };
type EnvRule = { clientId: string; environment: string; serverId: string };
type RepoRule = { owner: string; repo: string; environment: string; serverId: string };
type ServerLite = { id: string; name: string; ip: string; clientId?: string };
type UserRow = { id: number; login: string; name: string; isAdmin: boolean; lastLoginAt: string; revokedAt?: string };
type Source = { account: string; pattern: string };
type Installation = { id: number; account: string; type: string };

export type AdminData = {
  policy: {
    clients: Client[];
    links: RepoLink[];
    envRules: EnvRule[];
    repoRules: RepoRule[];
    sources: Source[];
    homeAccount: string;
  };
  servers: ServerLite[];
  users: UserRow[];
  installations: Installation[] | null;
  installUrl: string;
  me: string;
};

const input =
  "rounded-md border border-line bg-bg px-2.5 py-1.5 text-[13px] text-cream outline-none placeholder:text-faint focus:border-line-strong";
const button =
  "rounded-md border border-line px-3 py-1.5 text-[13px] text-cream transition-colors hover:bg-surface-hover disabled:opacity-50";
const primary = "rounded-md bg-cream px-3 py-1.5 text-[13px] font-bold text-bg transition-opacity hover:opacity-90 disabled:opacity-50";

function Card({ title, hint, children }: { title: string; hint?: string; children: ReactNode }) {
  return (
    <section className="rounded-lg border border-line bg-surface">
      <div className="border-b border-line px-5 py-3">
        <h2 className="text-[13px] font-semibold text-cream first-letter:uppercase">{title}</h2>
        {hint && <p className="mt-0.5 text-[12px] text-muted first-letter:uppercase">{hint}</p>}
      </div>
      <div className="px-5 py-4">{children}</div>
    </section>
  );
}

export function AdminView({ initial }: { initial: AdminData }) {
  const router = useRouter();
  const { policy, servers, users, installations, installUrl, me } = initial;
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const clientName = (id?: string) =>
    !id || id === "agency" ? "agency (shared)" : (policy.clients.find((c) => c.id === id)?.name ?? id);
  const serverName = (id: string) => servers.find((s) => s.id === id)?.name ?? id;

  async function call(method: string, path: string, body?: unknown) {
    setBusy(true);
    setError(null);
    try {
      const resp = await fetch(path, {
        method,
        headers: body ? { "content-type": "application/json" } : undefined,
        body: body ? JSON.stringify(body) : undefined,
      });
      if (!resp.ok) {
        const data = (await resp.json().catch(() => null)) as { error?: string } | null;
        setError(data?.error ?? `request failed (${resp.status})`);
        return false;
      }
      router.refresh();
      return true;
    } catch {
      setError("couldn't reach berth");
      return false;
    } finally {
      setBusy(false);
    }
  }

  // form state
  const [newClient, setNewClient] = useState({ id: "", name: "" });
  const [newLink, setNewLink] = useState({ owner: "", repo: "*", clientId: "" });
  const [newRule, setNewRule] = useState({ clientId: "", environment: "", serverId: "" });
  const [newOverride, setNewOverride] = useState({ owner: "", repo: "", environment: "", serverId: "" });
  const [newSource, setNewSource] = useState({ account: "", pattern: "" });

  // A rule may only point at a shared server or one the client owns.
  const serversFor = (clientId: string) =>
    servers.filter((s) => !s.clientId || s.clientId === clientId);

  return (
    <div className="mt-6 space-y-5">
      {error && (
        <p role="alert" className="rounded-md border border-err/40 bg-err/10 px-3 py-2 text-[13px] text-err">
          {error}
        </p>
      )}

      <Card
        title="github app"
        hint="people see the repos they can reach on GitHub, but only where the berth app is installed, and developers only those inside “deploy from” below."
      >
        <div className="flex flex-wrap items-center gap-2 text-[13px]">
          {installations === null ? (
            <span className="text-err">couldn&apos;t read installations (check GITHUB_APP_ID / private key)</span>
          ) : installations.length === 0 ? (
            <span className="text-muted">not installed anywhere yet</span>
          ) : (
            installations.map((i) => (
              <span key={i.id} className="rounded-md border border-line px-2 py-1 text-cream">
                {i.account} <span className="text-faint">· {i.type.toLowerCase()}</span>
              </span>
            ))
          )}
          {installUrl && (
            <a href={installUrl} target="_blank" rel="noreferrer" className="ml-auto text-cream hover:text-cream">
              install on another account →
            </a>
          )}
        </div>
      </Card>

      <Card
        title="deploy from"
        hint="where developers may deploy from: an account, and optionally a repo-name pattern with * as a wildcard. admins can deploy from any repo the app reaches, including their own."
      >
        {policy.sources.length === 0 ? (
          <p className="mb-4 text-[13px] text-muted">
            {policy.homeAccount ? (
              <>
                no rules yet, so developers deploy from every repo of <span className="text-cream">{policy.homeAccount}</span>, the
                account that owns the app. add a rule to change that.
              </>
            ) : (
              "no rules yet, and the app's owner couldn't be read, so developers can't deploy until you add one."
            )}
          </p>
        ) : (
          <ul className="mb-4 divide-y divide-line text-[13px]">
            {policy.sources.map((src) => (
              <li key={`${src.account}/${src.pattern}`} className="flex items-center gap-3 py-2">
                <span className="text-cream">
                  {src.account}/{src.pattern}
                </span>
                <span className="text-faint">{src.pattern === "*" ? "every repo" : "matching repos"}</span>
                <button
                  className="ml-auto text-muted hover:text-err"
                  disabled={busy}
                  onClick={() =>
                    call(
                      "DELETE",
                      `/api/admin/sources?account=${encodeURIComponent(src.account)}&pattern=${encodeURIComponent(src.pattern)}`,
                    )
                  }
                >
                  remove
                </button>
              </li>
            ))}
          </ul>
        )}
        <form
          className="flex flex-wrap gap-2"
          onSubmit={async (e) => {
            e.preventDefault();
            if (await call("PUT", "/api/admin/sources", { account: newSource.account, pattern: newSource.pattern || "*" }))
              setNewSource({ account: "", pattern: "" });
          }}
        >
          <input
            className={input}
            placeholder={policy.homeAccount || "account or org"}
            value={newSource.account}
            onChange={(e) => setNewSource({ ...newSource, account: e.target.value })}
          />
          <input
            className={input}
            placeholder="repos (* or e.g. meals-*)"
            value={newSource.pattern}
            onChange={(e) => setNewSource({ ...newSource, pattern: e.target.value })}
          />
          <button className={button} disabled={busy || !newSource.account}>
            add
          </button>
        </form>
      </Card>

      <Card title="clients" hint="each client's servers only ever run that client's repos.">
        <ul className="mb-4 divide-y divide-line text-[13px]">
          {policy.clients.map((c) => (
            <li key={c.id} className="flex items-center gap-3 py-2">
              <span className="text-cream">{c.name}</span>
              <span className="text-faint">{c.id}</span>
              {c.id !== "agency" && (
                <button
                  className="ml-auto text-muted hover:text-err"
                  disabled={busy}
                  onClick={() => confirm(`delete client ${c.name}? its repo links and rules go too.`) && call("DELETE", `/api/admin/clients/${c.id}`)}
                >
                  delete
                </button>
              )}
            </li>
          ))}
        </ul>
        <form
          className="flex flex-wrap gap-2"
          onSubmit={async (e) => {
            e.preventDefault();
            if (await call("PUT", `/api/admin/clients/${encodeURIComponent(newClient.id)}`, { name: newClient.name }))
              setNewClient({ id: "", name: "" });
          }}
        >
          <input className={input} placeholder="id (e.g. acme)" value={newClient.id} onChange={(e) => setNewClient({ ...newClient, id: e.target.value.toLowerCase() })} />
          <input className={input} placeholder="name (e.g. Acme Inc)" value={newClient.name} onChange={(e) => setNewClient({ ...newClient, name: e.target.value })} />
          <button className={primary} disabled={busy || !newClient.id || !newClient.name}>add client</button>
        </form>
      </Card>

      <Card title="servers" hint="shared servers take any client's code (e.g. your dev box). a client's own server takes only theirs.">
        <ul className="divide-y divide-line text-[13px]">
          {servers.map((s) => (
            <li key={s.id} className="flex flex-wrap items-center gap-3 py-2">
              <span className="text-cream">{s.name}</span>
              <span className="text-faint">{s.ip}</span>
              <select
                className={`${input} ml-auto`}
                value={s.clientId || "agency"}
                disabled={busy}
                onChange={(e) => call("PUT", `/api/admin/servers/${s.id}/client`, { clientId: e.target.value === "agency" ? "" : e.target.value })}
              >
                <option value="agency">agency (shared)</option>
                {policy.clients.filter((c) => c.id !== "agency").map((c) => (
                  <option key={c.id} value={c.id}>{c.name}&apos;s own</option>
                ))}
              </select>
            </li>
          ))}
          {servers.length === 0 && <li className="py-2 text-muted">no servers yet. add one from the deploy page.</li>}
        </ul>
      </Card>

      <Card title="repos → clients" hint="link a client's GitHub org with repo *, or single repos that live in your own org. unlinked repos are agency projects.">
        <ul className="mb-4 divide-y divide-line text-[13px]">
          {policy.links.map((l) => (
            <li key={`${l.owner}/${l.repo}`} className="flex items-center gap-3 py-2">
              <span className="text-cream">{l.owner}/{l.repo}</span>
              <span className="text-muted">→ {clientName(l.clientId)}</span>
              <button className="ml-auto text-muted hover:text-err" disabled={busy} onClick={() => call("DELETE", `/api/admin/links/${encodeURIComponent(l.owner)}/${encodeURIComponent(l.repo)}`)}>
                remove
              </button>
            </li>
          ))}
          {policy.links.length === 0 && <li className="py-2 text-muted">no links yet.</li>}
        </ul>
        <form
          className="flex flex-wrap gap-2"
          onSubmit={async (e) => {
            e.preventDefault();
            if (await call("PUT", "/api/admin/links", newLink)) setNewLink({ owner: "", repo: "*", clientId: newLink.clientId });
          }}
        >
          <input className={input} placeholder="owner (org or user)" value={newLink.owner} onChange={(e) => setNewLink({ ...newLink, owner: e.target.value })} />
          <input className={input} placeholder="repo or *" value={newLink.repo} onChange={(e) => setNewLink({ ...newLink, repo: e.target.value })} />
          <select className={input} value={newLink.clientId} onChange={(e) => setNewLink({ ...newLink, clientId: e.target.value })}>
            <option value="">client…</option>
            {policy.clients.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}
          </select>
          <button className={primary} disabled={busy || !newLink.owner || !newLink.repo || !newLink.clientId}>link</button>
        </form>
      </Card>

      <Card title="environments" hint="where each client's environments deploy. developers pick an environment; the server follows from here.">
        <ul className="mb-4 divide-y divide-line text-[13px]">
          {policy.envRules.map((r) => (
            <li key={`${r.clientId}/${r.environment}`} className="flex flex-wrap items-center gap-3 py-2">
              <span className="text-cream">{clientName(r.clientId)}</span>
              <span className="text-cream">{r.environment}</span>
              <span className="text-muted">→ {serverName(r.serverId)}</span>
              <button className="ml-auto text-muted hover:text-err" disabled={busy} onClick={() => call("DELETE", `/api/admin/clients/${r.clientId}/environments/${encodeURIComponent(r.environment)}`)}>
                remove
              </button>
            </li>
          ))}
          {policy.envRules.length === 0 && <li className="py-2 text-muted">no environments yet.</li>}
        </ul>
        <form
          className="flex flex-wrap items-center gap-2"
          onSubmit={async (e) => {
            e.preventDefault();
            const { clientId, environment, serverId } = newRule;
            if (await call("PUT", `/api/admin/clients/${clientId}/environments/${encodeURIComponent(environment)}`, { serverId }))
              setNewRule({ ...newRule, environment: "", serverId: "" });
          }}
        >
          <select className={input} value={newRule.clientId} onChange={(e) => setNewRule({ ...newRule, clientId: e.target.value, serverId: "" })}>
            <option value="">client…</option>
            {policy.clients.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}
          </select>
          <input className={input} placeholder="environment (e.g. production)" value={newRule.environment} onChange={(e) => setNewRule({ ...newRule, environment: e.target.value })} />
          <select className={input} value={newRule.serverId} disabled={!newRule.clientId} onChange={(e) => setNewRule({ ...newRule, serverId: e.target.value })}>
            <option value="">server…</option>
            {serversFor(newRule.clientId).map((s) => <option key={s.id} value={s.id}>{s.name}</option>)}
          </select>
          <button className={primary} disabled={busy || !newRule.clientId || !newRule.environment || !newRule.serverId}>save</button>
        </form>
      </Card>

      <Card title="repo overrides" hint="one repo that deploys somewhere other than its client's rule.">
        <ul className="mb-4 divide-y divide-line text-[13px]">
          {policy.repoRules.map((r) => (
            <li key={`${r.owner}/${r.repo}/${r.environment}`} className="flex flex-wrap items-center gap-3 py-2">
              <span className="text-cream">{r.owner}/{r.repo}</span>
              <span className="text-cream">{r.environment}</span>
              <span className="text-muted">→ {serverName(r.serverId)}</span>
              <button className="ml-auto text-muted hover:text-err" disabled={busy} onClick={() => call("DELETE", `/api/admin/repo-rules/${encodeURIComponent(r.owner)}/${encodeURIComponent(r.repo)}/${encodeURIComponent(r.environment)}`)}>
                remove
              </button>
            </li>
          ))}
          {policy.repoRules.length === 0 && <li className="py-2 text-muted">no overrides.</li>}
        </ul>
        <form
          className="flex flex-wrap items-center gap-2"
          onSubmit={async (e) => {
            e.preventDefault();
            if (await call("PUT", "/api/admin/repo-rules", newOverride)) setNewOverride({ owner: "", repo: "", environment: "", serverId: "" });
          }}
        >
          <input className={input} placeholder="owner" value={newOverride.owner} onChange={(e) => setNewOverride({ ...newOverride, owner: e.target.value })} />
          <input className={input} placeholder="repo" value={newOverride.repo} onChange={(e) => setNewOverride({ ...newOverride, repo: e.target.value })} />
          <input className={input} placeholder="environment" value={newOverride.environment} onChange={(e) => setNewOverride({ ...newOverride, environment: e.target.value })} />
          <select className={input} value={newOverride.serverId} onChange={(e) => setNewOverride({ ...newOverride, serverId: e.target.value })}>
            <option value="">server…</option>
            {servers.map((s) => <option key={s.id} value={s.id}>{s.name}</option>)}
          </select>
          <button className={button} disabled={busy || !newOverride.owner || !newOverride.repo || !newOverride.environment || !newOverride.serverId}>save override</button>
        </form>
      </Card>

      <Card
        title="people"
        hint="everyone who has signed in. admins come from BERTH_ADMINS; everyone else's access is their access on GitHub. remove signs someone out at once and keeps them out until you restore them."
      >
        <ul className="divide-y divide-line text-[13px]">
          {users.map((u) => (
            <li key={u.id} className="flex items-center gap-3 py-2">
              <span className={u.revokedAt ? "text-muted line-through" : "text-cream"}>{u.login}</span>
              {u.name && <span className="text-muted">{u.name}</span>}
              {u.isAdmin && <span className="rounded border border-line px-1.5 text-[11px] text-cream">admin</span>}
              {u.revokedAt && <span className="rounded border border-err/40 px-1.5 text-[11px] text-err">removed</span>}
              <span className="ml-auto text-faint">last in {formatDay(u.lastLoginAt)}</span>
              {!u.isAdmin && u.login !== me &&
                (u.revokedAt ? (
                  <button className="text-muted hover:text-cream" disabled={busy} onClick={() => call("POST", `/api/admin/users/${u.id}/restore`)}>
                    restore
                  </button>
                ) : (
                  <button
                    className="text-muted hover:text-err"
                    disabled={busy}
                    onClick={() =>
                      confirm(`remove ${u.login}? they're signed out now and can't sign in again until you restore them.`) &&
                      call("POST", `/api/admin/users/${u.id}/remove`)
                    }
                  >
                    remove
                  </button>
                ))}
            </li>
          ))}
        </ul>
      </Card>
    </div>
  );
}
