import Link from "next/link";
import { Suspense } from "react";
import { EnvBadge } from "@/components/env-badge";
import { ArrowRight, Plus } from "@/components/icons";
import { Page, PageHeader } from "@/components/page-header";
import type { Deployment } from "@/lib/data";
import { getDeployments } from "@/lib/deployments";
import { getServers, type ServerView } from "@/lib/servers";
import { getCurrentUser } from "@/lib/session";
import { STATE_LABEL, STATE_TEXT, StatusDot, deployState, envRank, minutesAgo, type DeployState } from "@/lib/status";

export const metadata = { title: "Overview" };

export default async function OverviewPage() {
  const user = await getCurrentUser();
  return (
    <Page>
      <PageHeader
        title="Overview"
        description={
          user?.isAdmin
            ? "Every project, environment and server Berth runs."
            : "The projects you can deploy, where they run, and how they're doing."
        }
      />
      {/* /api/deployments asks GitHub about every deployment, so it streams in. */}
      <Suspense fallback={<OverviewSkeleton />}>
        <Cockpit />
      </Suspense>
    </Page>
  );
}

async function Cockpit() {
  const [deployments, servers] = await Promise.all([getDeployments(), getServers()]);
  const rows = deployments
    .map((d) => ({ d, state: deployState(d) }))
    .sort(
      (a, b) =>
        rankState(a.state) - rankState(b.state) ||
        a.d.repo.localeCompare(b.d.repo) ||
        envRank(a.d.environment) - envRank(b.d.environment),
    );
  const count = (s: DeployState) => rows.filter((r) => r.state === s).length;
  const projects = new Set(deployments.map((d) => d.repo)).size;
  const readyServers = servers.filter((s) => s.status === "ready").length;
  const serverName = (id: string) => servers.find((s) => s.id === id)?.name ?? id;

  const activity = deployments
    .flatMap((d) => (d.timeline ?? []).map((e) => ({ ...e, repo: d.repo, app: d.app })))
    .sort((a, b) => minutesAgo(a.when) - minutesAgo(b.when))
    .slice(0, 8);

  if (deployments.length === 0) {
    return <EmptyState hasServers={servers.length > 0} />;
  }

  return (
    <div className="mt-6 space-y-6">
      <div className="grid grid-cols-2 gap-px overflow-hidden rounded-lg border border-line bg-line lg:grid-cols-4">
        <Stat label="Deployments" value={deployments.length} sub={`${projects} project${projects === 1 ? "" : "s"}`} />
        <Stat label="Ready" value={count("ready")} tone="text-ok" sub="latest release succeeded" />
        <Stat
          label="Needs attention"
          value={count("failing")}
          tone={count("failing") ? "text-err" : undefined}
          sub={count("deploying") ? `${count("deploying")} deploying now` : "nothing in flight"}
        />
        <Stat label="Servers" value={`${readyServers}/${servers.length}`} sub="ready" />
      </div>

      <div className="grid gap-6 xl:grid-cols-[minmax(0,1fr)_340px]">
        <section className="min-w-0 overflow-hidden rounded-lg border border-line bg-surface">
          <div className="flex items-center justify-between border-b border-line px-4 py-3">
            <h2 className="text-[13px] font-semibold text-cream">Deployments</h2>
            <Link href="/deployments" className="flex items-center gap-1 text-[12px] text-muted hover:text-cream">
              Manage <ArrowRight />
            </Link>
          </div>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[640px] text-left text-[13px]">
              <thead className="text-[11px] uppercase tracking-wider text-faint">
                <tr className="border-b border-line">
                  <th className="px-4 py-2 font-medium">Project</th>
                  <th className="px-4 py-2 font-medium">Environment</th>
                  <th className="px-4 py-2 font-medium">Server</th>
                  <th className="px-4 py-2 font-medium">Last release</th>
                  <th className="px-4 py-2 font-medium">Status</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line">
                {rows.map(({ d, state }) => {
                  const [owner, repo] = d.repo.split("/");
                  const last = d.timeline?.[0];
                  const href = `/deployments?owner=${encodeURIComponent(owner)}&repo=${encodeURIComponent(repo)}&environment=${encodeURIComponent(d.environment)}`;
                  return (
                    <tr key={`${d.repo}/${d.environment}`} className="group transition-colors hover:bg-surface-2">
                      <td className="px-4 py-3">
                        <Link href={href} className="block">
                          <span className="flex items-center gap-2 font-medium text-cream group-hover:underline">
                            <span className="sm:hidden">
                              <StatusDot state={state} />
                            </span>
                            {d.app}
                          </span>
                          <span className="block text-[12px] text-muted">{d.repo}</span>
                        </Link>
                      </td>
                      <td className="px-4 py-3">
                        <EnvBadge env={d.environment} />
                        <span className="data mt-1 block text-[11px] text-faint">{d.branch}</span>
                      </td>
                      <td className="px-4 py-3 text-body">{serverName(d.serverId)}</td>
                      <td className="px-4 py-3">
                        {last ? (
                          <>
                            <span className="data text-body">{last.sha || "—"}</span>
                            <span className="block text-[12px] text-muted">
                              {last.actor || "unknown"} · {last.when}
                            </span>
                          </>
                        ) : (
                          <span className="text-muted">Never deployed</span>
                        )}
                      </td>
                      <td className="px-4 py-3">
                        <span className={`flex items-center gap-2 font-medium ${STATE_TEXT[state]}`}>
                          <StatusDot state={state} />
                          {STATE_LABEL[state]}
                        </span>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </section>

        <div className="space-y-6">
          <section className="rounded-lg border border-line bg-surface">
            <h2 className="border-b border-line px-4 py-3 text-[13px] font-semibold text-cream">Recent activity</h2>
            {activity.length === 0 ? (
              <p className="px-4 py-6 text-[13px] text-muted">No releases recorded yet.</p>
            ) : (
              <ol className="divide-y divide-line">
                {activity.map((e, i) => (
                  <li key={i} className="flex gap-3 px-4 py-3 text-[13px]">
                    <StatusDot state={releaseState(e.status)} />
                    <div className="-mt-1 min-w-0 flex-1">
                      <p className="truncate text-body">
                        <span className="font-medium text-cream">{e.actor || "someone"}</span> deployed{" "}
                        <span className="font-medium text-cream">{e.app}</span> to {e.environment}
                      </p>
                      <p className="text-[12px] text-muted">
                        <span className="data">{e.sha || "—"}</span> · {e.when}
                      </p>
                    </div>
                  </li>
                ))}
              </ol>
            )}
          </section>

          <ServersCard servers={servers} deployments={deployments} />
        </div>
      </div>
    </div>
  );
}

function rankState(s: DeployState) {
  return { failing: 0, deploying: 1, pending: 2, ready: 3, idle: 4 }[s];
}

function releaseState(s: string): DeployState {
  return s === "success" ? "ready" : s === "failed" ? "failing" : s === "running" ? "deploying" : "pending";
}

function Stat({ label, value, sub, tone }: { label: string; value: string | number; sub?: string; tone?: string }) {
  return (
    <div className="bg-surface px-4 py-4">
      <div className="text-[12px] font-medium text-muted">{label}</div>
      <div className={`mt-1 text-[26px] font-semibold tabular-nums tracking-tight ${tone ?? "text-cream"}`}>{value}</div>
      {sub && <div className="text-[12px] text-faint">{sub}</div>}
    </div>
  );
}

const SERVER_DOT: Record<ServerView["status"], string> = {
  ready: "bg-ok",
  setting_up: "bg-warn animate-pulse",
  failed: "bg-err",
  probed: "bg-faint",
};

const SERVER_LABEL: Record<ServerView["status"], string> = {
  ready: "Ready",
  setting_up: "Setting up",
  failed: "Setup failed",
  probed: "Not set up",
};

function ServersCard({ servers, deployments }: { servers: ServerView[]; deployments: Deployment[] }) {
  return (
    <section className="rounded-lg border border-line bg-surface">
      <div className="flex items-center justify-between border-b border-line px-4 py-3">
        <h2 className="text-[13px] font-semibold text-cream">Servers</h2>
        <Link href="/servers" className="flex items-center gap-1 text-[12px] text-muted hover:text-cream">
          All <ArrowRight />
        </Link>
      </div>
      {servers.length === 0 ? (
        <p className="px-4 py-6 text-[13px] text-muted">No servers you can deploy to yet.</p>
      ) : (
        <ul className="divide-y divide-line">
          {servers.map((s) => {
            const apps = deployments.filter((d) => d.serverId === s.id).length;
            return (
              <li key={s.id} className="flex items-center gap-3 px-4 py-3 text-[13px]">
                <span className={`h-2 w-2 shrink-0 rounded-full ${SERVER_DOT[s.status]}`} title={SERVER_LABEL[s.status]} />
                <div className="min-w-0 flex-1">
                  <p className="truncate font-medium text-cream">{s.name}</p>
                  <p className="data truncate text-[12px] text-muted">{s.ip}</p>
                </div>
                <span className="shrink-0 text-[12px] text-muted">
                  {apps} app{apps === 1 ? "" : "s"}
                </span>
              </li>
            );
          })}
        </ul>
      )}
    </section>
  );
}

function EmptyState({ hasServers }: { hasServers: boolean }) {
  return (
    <div className="mt-6 rounded-lg border border-dashed border-line-strong px-6 py-16 text-center">
      <h2 className="text-[15px] font-semibold text-cream">Nothing deployed yet</h2>
      <p className="mx-auto mt-1 max-w-md text-[13px] text-muted">
        {hasServers
          ? "Pick a repository and an environment, and Berth sets up the workflow, secrets and server for it."
          : "There are no servers you can deploy to yet. Ask an admin to add one and set up your project's environments."}
      </p>
      <Link
        href="/new"
        className="mt-5 inline-flex items-center gap-2 rounded-md bg-cream px-3 py-2 text-[13px] font-medium text-bg hover:opacity-90"
      >
        <Plus />{" "}New deployment
      </Link>
    </div>
  );
}

function OverviewSkeleton() {
  return (
    <div className="mt-6 space-y-6" aria-busy="true">
      <div className="h-[98px] animate-pulse rounded-lg border border-line bg-surface" />
      <div className="h-80 animate-pulse rounded-lg border border-line bg-surface" />
    </div>
  );
}
