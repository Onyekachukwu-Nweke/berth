"use client";

import { useCallback, useEffect, useRef, useState, useTransition } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { Badge } from "@/components/badge";
import { ServerAvatar } from "@/components/server-avatar";
import { StreamLog } from "@/components/stream-log";
import { ConfirmAction } from "@/components/confirm-action";
import { ServerDomains, type DomainFormValue } from "@/components/server-domains";
import {
  ArrowRight,
  ChevronRight,
  CircleCheck,
  CircleDot,
  CircleX,
  ExternalLink,
  GithubMark,
  Refresh,
  Terminal,
  Trash,
} from "@/components/icons";
import type { Deployment, DeployRun, Release, ReleaseStatus, RunStatus } from "@/lib/data";
import type { Domain, ServerView } from "@/lib/servers";
import {
  type ServerMetrics,
  fetchServerMetrics,
  fmtDisk,
  fmtLoad,
  fmtMem,
  fmtUptime,
} from "@/lib/server-metrics";
import {
  type GithubConclusion,
  type GithubStatus,
  type RunJob,
  type RunSteps,
  fetchRunSteps,
  runActive,
} from "@/lib/run-steps";
import { isLogHeader, logHeaderName, splitLogTimestamp, formatLogTimestamp, fetchServerLogs } from "@/lib/server-logs";
import { classifyJobLogLine, fetchJobLogs } from "@/lib/job-logs";
import { type DomainPreview, fetchDomainPreview } from "@/lib/domain-preview";
import { type DashboardTool, installedDashboards, openDashboard } from "@/lib/dashboards";
import { type AppHealth, clearDeployLock, deleteDeployment, fetchAppHealth, setDeploymentPort } from "@/lib/app-health";
import { getRepoBranches } from "@/lib/branches";
import {
  type ApplyResult,
  type DeploymentSecret,
  type DeploymentSecrets,
  deleteDeploymentSecret,
  fetchDeploymentSecrets,
  setDeploymentSecrets,
} from "@/lib/secrets";
import { type UptimeKumaAdmin, fetchUptimeKumaAdmin, resetUptimeKumaAdmin } from "@/lib/uptime-kuma-admin";

const runColor: Record<RunStatus, string> = {
  success: "bg-blue",
  failed: "bg-red-400",
  running: "bg-lime",
};

// Release status extends run status with "pending" (recorded at deploy time, no
// completed run yet). Dot colours and Badge tones reuse the run palette.
const releaseColor: Record<ReleaseStatus, string> = {
  ...runColor,
  pending: "bg-muted",
};

const releaseTone: Record<ReleaseStatus, "blue" | "red" | "lime" | "gray"> = {
  success: "blue",
  failed: "red",
  running: "lime",
  pending: "gray",
};

const statusTone = { live: "blue", idle: "gray", failing: "red" } as const;

// deploymentKey identifies a deployment card by (app, environment) — one app can
// have several environments, so app alone is not unique. JSON.stringify gives a
// collision-free, text-safe key used for the open/health/deleted state.
function deploymentKey(d: Deployment | undefined): string {
  // repo is "owner/repo" — with environment it is the deployment's unique
  // identity (app names can collide across repos, so don't key on app).
  return d ? JSON.stringify([d.repo, d.environment]) : "";
}

// Environments sort production first, then staging, then the rest alphabetically,
// so a repo card lists its targets in operational priority.
const ENV_ORDER: Record<string, number> = { production: 0, staging: 1 };
function envRank(env: string): number {
  return env in ENV_ORDER ? ENV_ORDER[env] : 2;
}

// RepoGroup collects one repository's environments (each a Deployment) under a
// single card — the environment is the first-class unit, and a repo owns several.
type RepoGroup = { repo: string; owner: string; app: string; environments: Deployment[] };

// groupByRepo turns the flat per-environment list into one group per repository.
// The app label is deterministic: the production environment's app name, else the
// first environment's, so environments disagreeing never flickers the title.
function groupByRepo(deployments: Deployment[]): RepoGroup[] {
  const groups = new Map<string, Deployment[]>();
  for (const d of deployments) {
    const list = groups.get(d.repo);
    if (list) list.push(d);
    else groups.set(d.repo, [d]);
  }
  return Array.from(groups.entries()).map(([repo, envs]) => {
    const environments = [...envs].sort(
      (a, b) =>
        envRank(a.environment) - envRank(b.environment) || a.environment.localeCompare(b.environment),
    );
    const [owner] = splitRepo(repo);
    const headline = environments.find((e) => e.environment === "production") ?? environments[0];
    return { repo, owner, app: headline?.app ?? repo, environments };
  });
}

// defaultEnv picks the environment a repo card opens to: production if present,
// else the first by ordering.
function defaultEnv(group: RepoGroup): string {
  const prod = group.environments.find((e) => e.environment === "production");
  return (prod ?? group.environments[0])?.environment ?? "";
}

// How often to re-poll the GitHub run walkthrough while a run is in progress.
const STEP_POLL_MS = 4000;

export function MonitorView({
  deployments,
  servers,
  stamp,
}: {
  deployments: Deployment[];
  servers: ServerView[];
  stamp: string;
}) {
  const router = useRouter();
  // After a successful deploy the configure page sends the operator here with
  // ?owner=<o>&repo=<r>&environment=<env>, so open that deployment; otherwise
  // open the first one. Deployments are identified by (owner/repo, environment) —
  // an app name can collide across repos, so match on the repo, not the app.
  const params = useSearchParams();
  const ownerParam = params.get("owner");
  const repoParam = params.get("repo");
  const envParam = params.get("environment");
  // A card is one repository; `open` is the open repo ("owner/repo"), and
  // selectedEnvByRepo tracks which environment chip is active within each card.
  const [open, setOpen] = useState<string>(() => {
    const fullRepo = ownerParam && repoParam ? `${ownerParam}/${repoParam}` : "";
    const match = deployments.find((d) => d.repo === fullRepo);
    return (match ?? deployments[0])?.repo ?? "";
  });
  const [selectedEnvByRepo, setSelectedEnvByRepo] = useState<Record<string, string>>(() =>
    ownerParam && repoParam && envParam ? { [`${ownerParam}/${repoParam}`]: envParam } : {},
  );
  const [refreshing, startRefresh] = useTransition();
  const [metrics, setMetrics] = useState<Record<string, ServerMetrics | null>>({});
  const [serverList, setServerList] = useState<ServerView[]>(servers);
  const serverById = new Map(serverList.map((s) => [s.id, s]));

  // App health per deployment, keyed by (app, environment): undefined = not
  // probed yet, null = could not reach the backend (unknown), otherwise the
  // probe result.
  const [health, setHealth] = useState<Record<string, AppHealth | null>>({});
  // The delete confirmation gate (the open deployment to forget), and the set of
  // deployment keys already dropped locally so their cards disappear immediately
  // on delete.
  const [confirmDelete, setConfirmDelete] = useState<Deployment | null>(null);
  const [confirmClearLock, setConfirmClearLock] = useState<Deployment | null>(null);
  // Set when a domain was configured but recording its port on the deployment
  // failed, so the domain won't associate — surfaced instead of silently lost.
  const [portAssocError, setPortAssocError] = useState<string | null>(null);
  const [deletedKeys, setDeletedKeys] = useState<Set<string>>(new Set());
  const visibleDeployments = deployments.filter((d) => !deletedKeys.has(deploymentKey(d)));
  const repoGroups = groupByRepo(visibleDeployments);

  // The open repo group and, within it, the selected environment's deployment.
  // The selection falls back to the default when unset or no longer present (e.g.
  // after the selected environment was deleted).
  const openGroup = repoGroups.find((g) => g.repo === open);
  const selectedEnv =
    openGroup && openGroup.environments.some((e) => e.environment === selectedEnvByRepo[open])
      ? selectedEnvByRepo[open]
      : openGroup
        ? defaultEnv(openGroup)
        : "";
  const openDeployment = openGroup?.environments.find((e) => e.environment === selectedEnv);
  const openKey = deploymentKey(openDeployment);

  // Probe the open deployment's health on demand (no daemon), keyed by (repo,
  // environment) so switching the environment chip re-probes the newly selected
  // one. setState only runs in the resolved callback, never synchronously.
  useEffect(() => {
    if (!openDeployment || openKey in health) return;
    const ctrl = new AbortController();
    const [owner, repo] = splitRepo(openDeployment.repo);
    fetchAppHealth(owner, repo, openDeployment.environment, ctrl.signal).then((h) =>
      setHealth((prev) => ({ ...prev, [openKey]: h })),
    );
    return () => ctrl.abort();
  }, [openKey, health, openDeployment]);

  // confirmDeployDelete forgets a deployment's tracking, drops its card locally,
  // then refreshes to re-pull authoritative state. Runs from an event handler.
  async function confirmDeployDelete(d: Deployment) {
    setConfirmDelete(null);
    const [owner, repo] = splitRepo(d.repo);
    const ok = await deleteDeployment(owner, repo, d.environment);
    if (!ok) return;
    setDeletedKeys((prev) => new Set(prev).add(deploymentKey(d)));
    startRefresh(() => router.refresh());
  }

  // confirmLockClear force-releases a stuck deploy lock, then refreshes to
  // re-pull authoritative lock state. Runs from an event handler.
  async function confirmLockClear(d: Deployment) {
    setConfirmClearLock(null);
    const [owner, repo] = splitRepo(d.repo);
    const ok = await clearDeployLock(owner, repo, d.environment);
    if (!ok) return;
    startRefresh(() => router.refresh());
  }

  // Live streams (add domain / remove domain) for the open card. Each only
  // opens after its ConfirmAction gate is confirmed.
  // The add variant captures the deployment it was launched from, so a domain's
  // port is linked to that environment even if the operator switches the
  // environment chip while the (streamed) domain setup is still running.
  const [domainTarget, setDomainTarget] = useState<
    | { server: ServerView; mode: "add"; value: DomainFormValue; deployment: Deployment }
    | { server: ServerView; mode: "remove"; host: string }
    | null
  >(null);

  // Confirmation gates. Nothing touches a server until one is confirmed.
  const [confirmDomain, setConfirmDomain] = useState<
    | { server: ServerView; mode: "add"; value: DomainFormValue; deployment: Deployment }
    | { server: ServerView; mode: "remove"; host: string }
    | null
  >(null);
  // The add-domain preview (nginx config + script), keyed by the host it was
  // fetched for. Keying by host means a stale preview from a previous host is
  // never shown and the effect never resets state synchronously; data null means
  // the fetch was attempted but returned nothing.
  const [previewState, setPreviewState] = useState<{ host: string; data: DomainPreview | null } | null>(null);

  const repoCount = new Set(visibleDeployments.map((d) => d.repo)).size;
  const liveCount = visibleDeployments.filter((d) => d.status === "live").length;
  const failingCount = visibleDeployments.filter((d) => d.status === "failing").length;
  const openServerId = openDeployment?.serverId ?? "";

  // Read the open deployment's server metrics on demand (no daemon), keyed by
  // server id. Poll while the card is open so a transient SSH miss or a cold
  // backend recovers on its own, and only ever store a real reading: a later
  // miss never clobbers the last good one, so the cards stay populated and
  // never fall back to n/a once they have loaded.
  useEffect(() => {
    if (!openServerId) return;
    const ctrl = new AbortController();
    let active = true;
    const read = () => {
      fetchServerMetrics(openServerId, ctrl.signal).then((m) => {
        if (active && m) setMetrics((prev) => ({ ...prev, [openServerId]: m }));
      });
    };
    read();
    const timer = setInterval(read, 8000);
    return () => {
      active = false;
      clearInterval(timer);
      ctrl.abort();
    };
  }, [openServerId]);

  // When the add-domain gate opens, fetch the exact nginx config + script the
  // backend would write, so the operator sees precisely what runs before it
  // does. State is only set in the resolved callback (never synchronously in the
  // effect body); staleness is handled by keying the result to the host.
  useEffect(() => {
    if (!confirmDomain || confirmDomain.mode !== "add") return;
    const host = confirmDomain.value.host;
    const ctrl = new AbortController();
    fetchDomainPreview(confirmDomain.value, ctrl.signal)
      .then((res) => {
        if (!ctrl.signal.aborted) setPreviewState({ host, data: "error" in res ? null : res });
      })
      .catch(() => {
        if (!ctrl.signal.aborted) setPreviewState({ host, data: null });
      });
    return () => ctrl.abort();
  }, [confirmDomain]);

  // Derived preview for the open add-domain gate: the data when it is for the
  // current host, and a loading flag while this host's fetch is still pending.
  const addHost = confirmDomain?.mode === "add" ? confirmDomain.value.host : null;
  const domainPreview = addHost !== null && previewState?.host === addHost ? previewState.data : null;
  const domainPreviewLoading = addHost !== null && previewState?.host !== addHost;

  function refresh() {
    setMetrics({}); // drop cached metrics so the open server re-reads
    setHealth({}); // drop cached health so the open app is re-probed
    startRefresh(() => router.refresh()); // re-pull deployments + their runs
  }

  // After a successful domain change, reflect it locally so the panels stay
  // current without a refetch, then refresh to re-pull authoritative state.
  function recordDomainAdd(serverId: string, value: DomainFormValue) {
    const entry: Domain = {
      host: value.host,
      aliases: value.aliases,
      upstream: value.upstream,
      email: value.email || undefined,
      createdAt: new Date().toISOString(),
    };
    setServerList((list) =>
      list.map((s) =>
        s.id === serverId
          ? { ...s, domains: [...(s.domains ?? []).filter((d) => d.host !== entry.host), entry] }
          : s,
      ),
    );
  }
  function recordDomainRemove(serverId: string, host: string) {
    setServerList((list) =>
      list.map((s) =>
        s.id === serverId ? { ...s, domains: (s.domains ?? []).filter((d) => d.host !== host) } : s,
      ),
    );
  }

  return (
    <main className="mx-auto flex w-full max-w-[1100px] flex-1 flex-col px-4 pb-16 pt-8 sm:px-6 sm:pt-10 lg:px-8">
      {/* Dashboard-first header: state and actions in the first viewport, no hero.
          The summary line is operational — repositories and environments are
          distinct counts, and idle is neither live nor failing. */}
      <div className="rise flex items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold tracking-tight text-cream sm:text-3xl">deployments</h1>
          <p className="mt-1.5 text-[13px] text-muted">
            <span className="font-semibold text-cream">{repoCount}</span>{" "}
            {repoCount === 1 ? "repository" : "repositories"}
            {" · "}
            <span className="font-semibold text-cream">{visibleDeployments.length}</span>{" "}
            {visibleDeployments.length === 1 ? "environment" : "environments"}
            {" · "}
            <span className="font-semibold text-blue">{liveCount}</span> live
            {" · "}
            <span className={`font-semibold ${failingCount > 0 ? "text-red-400" : "text-muted"}`}>
              {failingCount}
            </span>{" "}
            failing
            <span className="text-faint"> · {stamp}</span>
          </p>
        </div>
        <button
          onClick={refresh}
          disabled={refreshing}
          className="flex shrink-0 items-center gap-2 rounded-md border border-line px-3 py-2 text-[12px] text-lime transition-colors hover:bg-lime/10 disabled:opacity-60"
        >
          <Refresh className={refreshing ? "animate-spin" : ""} /> {refreshing ? "refreshing…" : "refresh"}
        </button>
      </div>

      {portAssocError && (
        <div className="mt-6 flex items-start justify-between gap-4 rounded-xl border border-red-400/30 bg-red-400/[0.05] px-4 py-3 text-[13px] leading-6 text-red-200">
          <span>{portAssocError}</span>
          <button
            onClick={() => setPortAssocError(null)}
            className="shrink-0 text-red-300 transition-colors hover:text-cream"
            aria-label="dismiss"
          >
            dismiss
          </button>
        </div>
      )}

      <div className="rise mt-6 flex flex-col gap-5" style={{ animationDelay: "80ms" }}>
        {repoGroups.length === 0 && (
          <p className="rounded-2xl border border-dashed border-line px-6 py-16 text-center text-[14px] text-muted">
            nothing deployed yet. connect a repository to a server and your live status shows up here.
          </p>
        )}
        {repoGroups.map((group) => {
          const isOpen = open === group.repo;
          // Headline environment (production, else first): its server avatars the
          // card and titles it.
          const headline =
            group.environments.find((e) => e.environment === "production") ?? group.environments[0];
          const headlineServer = serverById.get(headline?.serverId ?? "");
          const anyLocked = group.environments.some((e) => e.locked);
          // Collapsed activity is repo-wide, not just production's: the deploy
          // count sums every environment, and the run strip / "last deploy" track
          // the most operationally-notable environment — a failing one first, then
          // one mid-deploy, else the headline — so a broken or fresher staging is
          // never hidden behind a healthy production. (No absolute timestamps reach
          // the client, so "notable" stands in for a true most-recent sort.)
          const totalDeploys = group.environments.reduce((n, e) => n + (e.deploys ?? 0), 0);
          const activityEnv =
            group.environments.find((e) => e.status === "failing") ??
            group.environments.find((e) => e.locked) ??
            headline;
          return (
            <section key={group.repo} className="relative overflow-hidden rounded-2xl border border-line bg-surface">
              <button
                onClick={() => setOpen(isOpen ? "" : group.repo)}
                className="flex w-full items-center gap-4 px-6 py-5 pr-16 text-left"
                aria-expanded={isOpen}
              >
                <ChevronRight className={`text-muted transition-transform ${isOpen ? "rotate-90" : ""}`} />
                {headlineServer && <ServerAvatar seed={headlineServer.name} />}
                <div className="min-w-0 flex-1">
                  <span className="flex flex-wrap items-center gap-2.5">
                    <span className="text-[18px] font-semibold text-cream">{group.app}</span>
                    {/* Per-environment status pills — the source of truth for which
                        environments exist and which is failing (red dot). */}
                    {group.environments.map((e) => (
                      <Badge key={e.environment} tone={statusTone[e.status]} dot>
                        {e.environment}
                      </Badge>
                    ))}
                    {anyLocked && <LockPill />}
                  </span>
                  <span className="mt-1.5 block truncate text-[13px] text-muted">
                    {group.repo} · {group.environments.length}{" "}
                    {group.environments.length === 1 ? "environment" : "environments"}
                  </span>
                </div>
                {activityEnv && <RunStrip runs={activityEnv.runs} />}
                {activityEnv && (
                  <div className="hidden text-right sm:block">
                    <span className="block text-[18px] font-semibold text-cream">{totalDeploys}</span>
                    <span className="block text-[11px] text-muted">deploys · last {activityEnv.lastDeploy}</span>
                  </div>
                )}
              </button>

              {isOpen && openDeployment && (
                <>
                  <EnvironmentSwitcher
                    group={group}
                    selectedEnv={openDeployment.environment}
                    onSelect={(env) =>
                      setSelectedEnvByRepo((prev) => ({ ...prev, [group.repo]: env }))
                    }
                  />
                  {/* Keyed by (repo, environment) so switching the environment chip
                      remounts the card — resetting its env-scoped state (loaded
                      secrets, active tab, logs) instead of showing the previous
                      environment's data against the new one. */}
                  <ExpandedCard
                    key={openKey}
                    deployment={openDeployment}
                    server={serverById.get(openDeployment.serverId)}
                    metrics={metrics[openDeployment.serverId]}
                    health={health[openKey]}
                    siblings={group.environments.filter(
                      (e) => e.environment !== openDeployment.environment,
                    )}
                    onAddDomain={(server, value) =>
                      setConfirmDomain({ server, mode: "add", value, deployment: openDeployment })
                    }
                    onRemoveDomain={(server, host) => setConfirmDomain({ server, mode: "remove", host })}
                    onDelete={() => setConfirmDelete(openDeployment)}
                    onClearLock={() => setConfirmClearLock(openDeployment)}
                  />
                </>
              )}
            </section>
          );
        })}
      </div>

      {/* Live streams: each opens only after its gate is confirmed. */}
      {domainTarget && (
        <StreamLog
          title={
            domainTarget.mode === "add"
              ? `adding ${domainTarget.value.host}`
              : `removing ${domainTarget.host}`
          }
          subtitle={domainTarget.server.ip}
          timezone={domainTarget.server.timezone}
          url={
            domainTarget.mode === "add"
              ? addDomainUrl(domainTarget.server.id, domainTarget.value)
              : removeDomainUrl(domainTarget.server.id, domainTarget.host)
          }
          onDone={async (ok) => {
            if (!ok) return;
            if (domainTarget.mode === "add") {
              recordDomainAdd(domainTarget.server.id, domainTarget.value);
              // Record the domain's port as the port of the deployment this flow
              // was launched from (captured in domainTarget, so switching the
              // environment chip mid-stream can't retarget it). The card matches
              // domain→deployment by port; if this link fails, surface it —
              // otherwise the domain silently won't show on that environment.
              const dep = domainTarget.deployment;
              const [owner, repo] = splitRepo(dep.repo);
              const linked = await setDeploymentPort(owner, repo, dep.environment, Number(domainTarget.value.upstream));
              if (!linked) {
                setPortAssocError(
                  `${domainTarget.value.host} is live on the server, but linking it to ${dep.app} (${dep.environment}) failed. re-add the domain, or set the app port, or it won't show on this card.`,
                );
              }
            } else recordDomainRemove(domainTarget.server.id, domainTarget.host);
            startRefresh(() => router.refresh());
          }}
          onClose={() => setDomainTarget(null)}
        />
      )}

      {/* Confirmation gates. */}
      {confirmDomain && confirmDomain.mode === "add" && (
        <ConfirmAction
          title={`add ${confirmDomain.value.host}`}
          subtitle={confirmDomain.server.ip}
          summary={
            <>
              mountabo will configure <span className="text-cream">{confirmDomain.server.name}</span> to
              front <span className="text-cream">{confirmDomain.value.host}</span> on https, proxying to
              your app on port <span className="text-cream">{confirmDomain.value.upstream}</span>. it
              installs nginx if needed, writes the vhost below, and obtains a Let&apos;s Encrypt
              certificate over http, so the domain must already point at{" "}
              <span className="text-cream">{confirmDomain.server.ip}</span>. nothing runs until you
              confirm.
            </>
          }
          loading={domainPreviewLoading}
          steps={
            domainPreview
              ? `# ${domainPreview.sitePath} (http)\n${domainPreview.httpConfig}\n\n# ${domainPreview.sitePath} (https)\n${domainPreview.tlsConfig}\n\n# setup script\n${domainPreview.script}`
              : domainPreviewLoading
                ? undefined
                : [
                    "connect to the server as the mountabo user over SSH (with sudo)",
                    "install nginx if it is not already present",
                    `write the nginx vhost for ${confirmDomain.value.host}`,
                    "obtain a Let's Encrypt certificate over http and enable https",
                    "reload nginx so the domain serves over https",
                  ]
          }
          confirmLabel={`add ${confirmDomain.value.host}`}
          onConfirm={() => {
            setDomainTarget(confirmDomain);
            setConfirmDomain(null);
          }}
          onCancel={() => setConfirmDomain(null)}
        />
      )}

      {confirmDomain && confirmDomain.mode === "remove" && (
        <ConfirmAction
          title={`remove ${confirmDomain.host}`}
          subtitle={confirmDomain.server.ip}
          destructive
          summary={
            <>
              mountabo will stop fronting <span className="text-cream">{confirmDomain.host}</span> on{" "}
              <span className="text-cream">{confirmDomain.server.name}</span>: it removes the nginx vhost
              and its certificate, then reloads nginx. your app keeps running on its local port. nothing
              runs until you confirm.
            </>
          }
          steps={[
            "connect to the server as the mountabo user over SSH (with sudo)",
            `remove the nginx vhost for ${confirmDomain.host}`,
            "delete the Let's Encrypt certificate for the domain",
            "reload nginx so it stops serving the domain",
          ]}
          confirmLabel="remove domain"
          onConfirm={() => {
            setDomainTarget(confirmDomain);
            setConfirmDomain(null);
          }}
          onCancel={() => setConfirmDomain(null)}
        />
      )}

      {/* Delete a deployment: type-to-confirm, honest about what it does and
          does not do. It only removes mountabo's local tracking. */}
      {confirmDelete && (
        <ConfirmAction
          title={`delete ${confirmDelete.app}`}
          subtitle="tear this deployment down"
          destructive
          requireTyping={confirmDelete.app}
          summary={
            <>
              this tears down <span className="text-cream">{confirmDelete.app}</span>. mountabo stops and
              removes its running container on{" "}
              <span className="text-cream">{serverById.get(confirmDelete.serverId)?.name ?? confirmDelete.serverId}</span>,
              deletes the deploy workflow from{" "}
              <span className="text-cream">{confirmDelete.repo}</span> so a future push no longer deploys it,
              and forgets the deployment record and its history. this cannot be undone. type the app name to
              confirm.
            </>
          }
          stepsLabel="what deleting does"
          steps={[
            `stop and remove the running container for ${confirmDelete.app} on ${serverById.get(confirmDelete.serverId)?.name ?? confirmDelete.serverId}`,
            `delete the deploy workflow and deploy.sh from ${confirmDelete.repo} so it no longer deploys`,
            "remove the deployment record and its append-only history from mountabo",
          ]}
          confirmLabel="delete deployment"
          onConfirm={() => void confirmDeployDelete(confirmDelete)}
          onCancel={() => setConfirmDelete(null)}
        />
      )}

      {/* Clear a stuck deploy lock: a recovery escape hatch, not destructive. */}
      {confirmClearLock && (
        <ConfirmAction
          title={`clear lock on ${confirmClearLock.app}`}
          subtitle="allow a new deploy to start"
          summary={
            <>
              this force-releases the deploy lock on <span className="text-cream">{confirmClearLock.app}</span>{" "}
              so a new deploy can start. only do this if a previous deploy wedged and left the target
              locked — clearing it while a deploy is genuinely running could let two deploys race.
            </>
          }
          stepsLabel="what clearing does"
          steps={[
            `release mountabo's in-process deploy lock for ${confirmClearLock.app}`,
            "does not touch the server, the running app, or GitHub — it only frees the local lock",
          ]}
          confirmLabel="clear stuck lock"
          onConfirm={() => void confirmLockClear(confirmClearLock)}
          onCancel={() => setConfirmClearLock(null)}
        />
      )}
    </main>
  );
}

// LockPill shows on a card while a deploy to this target is in flight (the
// backend's per-target deploy lock is held), with how long it has been held.
function LockPill({ since }: { since?: string }) {
  return (
    <Badge tone="lime" dot>
      deploying{since ? ` · ${since}` : ""}
    </Badge>
  );
}

// HealthBanner is the prominent app-health read inside the open card: a clear
// healthy/unhealthy state, what was probed, and an honest "unknown" when the
// probe could not reach the backend. It is the SSH up/down indicator; the
// embedded Uptime Kuma dashboard is the detailed view.
function HealthBanner({ health }: { health: AppHealth | null | undefined }) {
  if (health === undefined) {
    return (
      <div className="mt-6 flex items-center gap-3 rounded-xl border border-line bg-surface/40 px-5 py-4">
        <CircleDot className="animate-pulse text-muted" />
        <span className="text-[14px] text-muted">checking whether the app is responding, over ssh…</span>
      </div>
    );
  }
  if (health === null) {
    return (
      <div className="mt-6 flex items-center gap-3 rounded-xl border border-line bg-surface/40 px-5 py-4">
        <CircleDot className="text-muted" />
        <span className="text-[14px] text-muted">
          app health is unknown. mountabo could not probe the app (the server may not be set up, or is
          unreachable).
        </span>
      </div>
    );
  }
  if (health.reachable) {
    return (
      <div className="mt-6 flex items-center gap-3 rounded-xl border border-blue/30 bg-blue/[0.06] px-5 py-4">
        <CircleCheck className="text-blue" />
        <span className="text-[14px] text-cream">
          app is healthy{health.status ? ` (http ${health.status})` : ""}
          {health.target && <span className="text-muted"> · {health.target}</span>}
        </span>
      </div>
    );
  }
  return (
    <div className="mt-6 flex items-center gap-3 rounded-xl border border-red-500/30 bg-red-500/[0.06] px-5 py-4">
      <CircleX className="text-red-400" />
      <span className="text-[14px] text-cream">
        app is not responding{health.status ? ` (http ${health.status})` : ""}
        {health.detail && <span className="text-muted"> · {health.detail}</span>}
      </span>
    </div>
  );
}

// EnvironmentSwitcher is the row of environment chips at the top of an open repo
// card: it selects which environment the tabs below reflect, and "+ environment"
// opens a lightweight name -> branch form that routes to the configure page
// (environment-first; branch is one attribute of the environment, set there).
function EnvironmentSwitcher({
  group,
  selectedEnv,
  onSelect,
}: {
  group: RepoGroup;
  selectedEnv: string;
  onSelect: (env: string) => void;
}) {
  const router = useRouter();
  const [owner, repo] = splitRepo(group.repo);
  const [showAdd, setShowAdd] = useState(false);
  const [newEnv, setNewEnv] = useState("");
  const [newBranch, setNewBranch] = useState("");
  const [branches, setBranches] = useState<string[] | null>(null);

  // The new environment defaults to the repo's production (else first) server;
  // the operator can change it on the configure page.
  const defaultServer =
    (group.environments.find((e) => e.environment === "production") ?? group.environments[0])
      ?.serverId ?? "";

  // Load the repo's branches once the add form is opened (never synchronously).
  useEffect(() => {
    if (!showAdd || branches !== null) return;
    const ctrl = new AbortController();
    getRepoBranches(owner, repo, ctrl.signal).then((list) => {
      if (!ctrl.signal.aborted) setBranches(list);
    });
    return () => ctrl.abort();
  }, [showAdd, branches, owner, repo]);

  const envInvalid = newEnv.trim() !== "" && !/^[A-Za-z0-9._-]+$/.test(newEnv.trim());

  return (
    <div className="flex flex-col gap-3 border-t border-line bg-surface/40 px-6 py-4 sm:px-8">
      <div className="flex flex-wrap items-center gap-2">
        <span className="mr-1 text-[12px] font-medium uppercase tracking-wide text-muted">
          environments
        </span>
        {group.environments.map((e) => {
          const active = e.environment === selectedEnv;
          return (
            <button
              key={e.environment}
              onClick={() => onSelect(e.environment)}
              aria-pressed={active}
              className={`inline-flex items-center gap-2 rounded-md border px-3 py-1.5 text-[13px] font-medium transition-colors ${
                active
                  ? "border-lime/50 bg-lime/15 text-lime"
                  : "border-line text-muted hover:bg-surface-2 hover:text-cream"
              }`}
            >
              <span
                className={`h-1.5 w-1.5 rounded-full ${
                  e.status === "live" ? "bg-blue" : e.status === "failing" ? "bg-red-400" : "bg-muted"
                }`}
              />
              {e.environment}
            </button>
          );
        })}
        <button
          onClick={() => setShowAdd((v) => !v)}
          aria-expanded={showAdd}
          className="inline-flex items-center gap-1 rounded-md border border-dashed border-line px-3 py-1.5 text-[13px] font-medium text-muted transition-colors hover:border-lime/50 hover:text-lime"
        >
          + environment
        </button>
      </div>

      {showAdd && (
        <form
          onSubmit={(e) => {
            e.preventDefault();
            const env = newEnv.trim();
            const branch = newBranch.trim();
            if (!env || !branch || envInvalid) return;
            router.push(
              `/configure?repo=${encodeURIComponent(group.repo)}&environment=${encodeURIComponent(env)}&branch=${encodeURIComponent(branch)}&server=${encodeURIComponent(defaultServer)}`,
            );
          }}
          className="rounded-xl border border-dashed border-line bg-surface/60 p-4"
        >
          <p className="text-[13px] leading-6 text-body">
            name the environment and pick its branch. the configure page opens prefilled, where you set its
            variables and domain, then deploy.
          </p>
          <div className="mt-3 flex flex-col gap-2 sm:flex-row">
            <input
              value={newEnv}
              onChange={(e) => setNewEnv(e.target.value)}
              placeholder="environment, e.g. staging"
              spellCheck={false}
              autoCapitalize="off"
              autoCorrect="off"
              className="min-w-0 flex-1 rounded-md border border-line bg-bg px-3 py-2 font-mono text-[13px] text-cream placeholder:text-faint focus:border-line-strong focus:outline-none"
            />
            {branches && branches.length > 0 ? (
              <select
                value={newBranch}
                onChange={(e) => setNewBranch(e.target.value)}
                className="min-w-0 flex-1 rounded-md border border-line bg-bg px-3 py-2 font-mono text-[13px] text-cream focus:border-line-strong focus:outline-none"
              >
                <option value="" className="text-faint">
                  pick a branch from github
                </option>
                {branches.map((b) => (
                  <option key={b} value={b}>
                    {b}
                  </option>
                ))}
              </select>
            ) : (
              <input
                value={newBranch}
                onChange={(e) => setNewBranch(e.target.value)}
                placeholder={branches === null ? "reading branches from github," : "branch, e.g. main"}
                spellCheck={false}
                autoCapitalize="off"
                autoCorrect="off"
                className="min-w-0 flex-1 rounded-md border border-line bg-bg px-3 py-2 font-mono text-[13px] text-cream placeholder:text-faint focus:border-line-strong focus:outline-none"
              />
            )}
            <button
              type="submit"
              disabled={newEnv.trim() === "" || newBranch.trim() === "" || envInvalid}
              className="shrink-0 rounded-md border border-lime/50 bg-lime/[0.08] px-4 py-2 text-[13px] font-medium text-lime transition-colors hover:bg-lime/[0.16] disabled:cursor-not-allowed disabled:opacity-40"
            >
              configure
            </button>
          </div>
          {envInvalid && (
            <p className="mt-2 text-[12px] text-red-400">
              environment names use letters, numbers, dots, dashes, and underscores.
            </p>
          )}
        </form>
      )}
    </div>
  );
}

// InfoTile is one read-only key/value cell in the overview/settings grids.
function InfoTile({ label, value, href }: { label: string; value: string; href?: string }) {
  return (
    <div className="bg-surface px-5 py-4">
      <p className="text-[11px] uppercase tracking-wide text-muted">{label}</p>
      {href ? (
        <a
          href={href}
          target="_blank"
          rel="noopener noreferrer"
          className="mt-1.5 block truncate text-[14px] font-medium text-lime hover:underline"
        >
          {value}
        </a>
      ) : (
        <p className="mt-1.5 truncate text-[14px] font-medium text-cream" title={value}>
          {value}
        </p>
      )}
    </div>
  );
}

// ExpandedCard is the open environment's full body: a tabbed surface with an
// overview (state, health, metrics, links), the deploys history (walkthrough +
// runs + releases), container logs, secrets, custom domains, monitoring, and
// settings (metadata + the danger zone). Everything is scoped to the selected
// environment's deployment.
function ExpandedCard({
  deployment: d,
  server,
  metrics: m,
  health,
  siblings,
  onAddDomain,
  onRemoveDomain,
  onDelete,
  onClearLock,
}: {
  deployment: Deployment;
  server?: ServerView;
  metrics: ServerMetrics | null | undefined;
  health: AppHealth | null | undefined;
  // The repo's other environments, for the secrets-compare row.
  siblings: Deployment[];
  onAddDomain: (server: ServerView, value: DomainFormValue) => void;
  onRemoveDomain: (server: ServerView, host: string) => void;
  onDelete: () => void;
  onClearLock: () => void;
}) {
  const latest = d.runs[0];
  const [owner, repo] = splitRepo(d.repo);
  const live = d.liveUrl ? d.liveUrl.replace(/^https?:\/\//, "") : "";
  const dashboards = installedDashboards(server?.options);

  // Custom domains are scoped to this environment by its published port: only
  // domains whose nginx upstream is this deployment's port are shown, so one
  // server hosting several environments never cross-lists another's domain.
  const envPort = d.port > 0 ? String(d.port) : undefined;
  const envDomains = envPort ? (server?.domains ?? []).filter((dm) => dm.upstream === envPort) : [];

  type TabKey = "overview" | "deploys" | "logs" | "secrets" | "domains" | "monitoring" | "settings";

  // Per-environment secret names, fetched lazily when the secrets tab opens.
  // GitHub Actions secret values are write-only, so only names + updated times
  // come back. Kept here so the tab label can show the count and a mutation in
  // the panel can refetch via reloadSecrets.
  const [secrets, setSecrets] = useState<DeploymentSecrets | null>(null);
  const reloadSecrets = useCallback(() => {
    fetchDeploymentSecrets(owner, repo, d.environment).then(setSecrets);
  }, [owner, repo, d.environment]);

  const timelineCount = d.timeline?.length ?? 0;
  const tabs: { key: TabKey; label: string }[] = [
    { key: "overview", label: "overview" },
    { key: "deploys", label: `deploys${d.deploys ? ` (${d.deploys})` : ""}` },
    { key: "logs", label: "logs" },
    ...(server && d.environment
      ? [{ key: "secrets" as const, label: `secrets${secrets ? ` (${secrets.secrets.length})` : ""}` }]
      : []),
    ...(server ? [{ key: "domains" as const, label: `domains (${envDomains.length})` }] : []),
    ...(server && dashboards.length > 0
      ? [{ key: "monitoring" as const, label: `monitoring (${dashboards.length})` }]
      : []),
    { key: "settings", label: "settings" },
  ];

  // Default to "overview" — the environment's current-state home (URL, branch,
  // server, health, last deploy, metrics, domains). Diagnostics and dangerous
  // actions live one click away in the other tabs.
  const [tab, setTab] = useState<TabKey>("overview");

  // Pull the environment's secret names lazily, once the secrets tab is opened.
  // setState only fires from the resolved callback, never synchronously.
  useEffect(() => {
    if (tab !== "secrets" || secrets !== null) return;
    const ctrl = new AbortController();
    fetchDeploymentSecrets(owner, repo, d.environment, ctrl.signal).then((data) => {
      if (!ctrl.signal.aborted && data) setSecrets(data);
    });
    return () => ctrl.abort();
  }, [tab, secrets, owner, repo, d.environment]);

  return (
    <div className="border-t border-line px-6 py-8 sm:px-8">
      <div className="space-y-5">
        {/* tab bar: only the active panel renders below, keeping the card short. */}
        <div className="flex flex-wrap items-center gap-x-4 gap-y-3">
          <span className="text-[12px] font-medium uppercase tracking-wide text-muted">section</span>
          <span
            role="tablist"
            aria-label="deployment sections"
            className="inline-flex max-w-full flex-wrap items-center gap-1 rounded-lg border border-line bg-surface p-1"
          >
            {tabs.map((t) => {
              const active = tab === t.key;
              return (
                <button
                  key={t.key}
                  role="tab"
                  aria-selected={active}
                  onClick={() => setTab(t.key)}
                  className={`rounded-md px-3.5 py-1.5 text-[13px] font-medium transition-colors ${
                    active ? "bg-lime/15 text-lime" : "text-muted hover:bg-surface-2 hover:text-cream"
                  }`}
                >
                  {t.label}
                </button>
              );
            })}
          </span>
        </div>

        {/* overview: the environment's current state — status, health, metrics,
            links, and identity. Kept compact; deeper detail lives in other tabs. */}
        {tab === "overview" && (
          <div className="space-y-5">
            <div className="flex flex-col gap-5 sm:flex-row sm:items-center sm:justify-between">
              <div className="flex items-center gap-4">
                <DeployStatusBadge status={latest?.status} />
                <div>
                  <p className="text-[17px] font-medium text-cream">
                    {latest
                      ? latest.status === "success"
                        ? "latest deployment succeeded"
                        : latest.status === "failed"
                          ? "latest deployment failed"
                          : "deployment is running"
                      : "no deployment runs yet"}
                  </p>
                  <p className="mt-1 text-[14px] text-muted">
                    {latest ? `${latest.message} · ${latest.when}` : "trigger a deploy to see status here"}
                  </p>
                </div>
              </div>
              <div className="flex flex-wrap items-center gap-2.5">
                {d.liveUrl && (
                  <a
                    href={d.liveUrl}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="cta-glow flex items-center gap-2 rounded-lg bg-lime-fill px-4 py-2.5 text-[14px] font-bold text-black transition-transform hover:-translate-y-0.5"
                  >
                    open {live} <ArrowRight />
                  </a>
                )}
                {d.workflowUrl && (
                  <a
                    href={d.workflowUrl}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="flex items-center gap-2 rounded-lg border border-line px-4 py-2.5 text-[13.5px] text-muted transition-colors hover:border-line-strong hover:text-cream"
                  >
                    <GithubMark /> actions workflow <ExternalLink />
                  </a>
                )}
              </div>
            </div>

            <HealthBanner health={health} />

            <div className="grid grid-cols-2 gap-px overflow-hidden rounded-xl border border-line bg-line sm:grid-cols-4">
              <BigMetric label="cpu" value={m ? fmtLoad(m) : "reading…"} />
              <BigMetric label="memory" value={m ? fmtMem(m) : "reading…"} />
              <BigMetric label="disk" value={m ? fmtDisk(m) : "reading…"} />
              <BigMetric label="uptime" value={m ? fmtUptime(m) : "reading…"} />
            </div>

            <dl className="grid grid-cols-2 gap-px overflow-hidden rounded-xl border border-line bg-line sm:grid-cols-4">
              <InfoTile label="branch" value={d.branch} />
              <InfoTile label="server" value={server?.name ?? d.serverId} />
              <InfoTile label="last deploy" value={d.lastDeploy} />
              <InfoTile
                label="custom domains"
                value={envDomains.length ? envDomains.map((dm) => dm.host).join(", ") : "none"}
              />
            </dl>
          </div>
        )}

        {/* deploys: the live walkthrough, recent runs, and release history. */}
        {tab === "deploys" && (
          <div className="space-y-6">
            <RunWalkthrough
              owner={owner}
              repo={repo}
              branch={d.branch}
              environment={d.environment}
              latestStatus={latest?.status}
            />
            <div>
              <p className="mb-2 text-[12px] font-medium uppercase tracking-wide text-muted">recent runs</p>
              {d.runs.length === 0 ? (
                <p className="text-[14px] text-muted">no runs recorded yet.</p>
              ) : (
                <ul className="divide-y divide-line overflow-hidden rounded-xl border border-line">
                  {d.runs.map((r, i) => (
                    <RunRow key={`${r.runUrl || r.sha}:${i}`} run={r} />
                  ))}
                </ul>
              )}
            </div>
            {timelineCount > 0 && (
              <div>
                <p className="mb-2 text-[12px] font-medium uppercase tracking-wide text-muted">
                  releases ({d.deploys ?? 0})
                </p>
                <ul className="divide-y divide-line overflow-hidden rounded-xl border border-line">
                  {(d.timeline ?? []).map((rel, i) => (
                    <ReleaseRow key={i} release={rel} />
                  ))}
                </ul>
              </div>
            )}
          </div>
        )}

        {/* container logs (server-scoped; follows the selected env's server) */}
        {tab === "logs" && <LogsViewer serverId={d.serverId} ready={Boolean(server)} active />}

        {/* per-environment GitHub Actions secrets: names + updated times (values
            are write-only), managed classification, .env.example gaps, compare. */}
        {tab === "secrets" && server && d.environment && (
          <SecretsPanel
            owner={owner}
            repo={repo}
            environment={d.environment}
            data={secrets}
            reload={reloadSecrets}
            siblings={siblings}
          />
        )}

        {/* custom domains, scoped to this environment by its published port */}
        {tab === "domains" && server && (
          <div className="overflow-hidden rounded-xl border border-line bg-surface">
            <ServerDomains
              key={`dom:${envPort ?? "none"}:${envDomains.map((dm) => dm.host).join(",")}`}
              server={server}
              onAdd={(value) => onAddDomain(server, value)}
              onRemove={(host) => onRemoveDomain(server, host)}
              scoped
              filterUpstream={envPort}
              defaultPort={envPort}
            />
          </div>
        )}

        {/* monitoring dashboards reached through the ssh tunnel */}
        {tab === "monitoring" && server && dashboards.length > 0 && (
          <DashboardsPanel serverId={server.id} dashboards={dashboards} active />
        )}

        {/* settings: read-only metadata + the danger zone (lock recovery, delete) */}
        {tab === "settings" && (
          <div className="space-y-5">
            <dl className="grid grid-cols-2 gap-px overflow-hidden rounded-xl border border-line bg-line sm:grid-cols-3">
              <InfoTile label="environment" value={d.environment} />
              <InfoTile label="branch" value={d.branch} />
              <InfoTile label="server" value={server?.name ?? d.serverId} />
              <InfoTile label="published port" value={d.port > 0 ? String(d.port) : "none"} />
              <InfoTile label="live url" value={live || "none"} href={d.liveUrl || undefined} />
              <InfoTile
                label="workflow"
                value={d.workflowUrl ? "actions workflow" : "none"}
                href={d.workflowUrl || undefined}
              />
            </dl>

            {/* stuck-lock recovery: only while the deploy lock is held. */}
            {d.locked && (
              <div className="rounded-xl border border-line bg-surface/40 p-5">
                <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
                  <div>
                    <p className="text-[15px] font-medium text-cream">a deploy is in progress</p>
                    <p className="mt-1 max-w-xl text-[13px] leading-6 text-muted">
                      {d.app} is locked{d.lockedSince ? ` (since ${d.lockedSince})` : ""} while a deploy runs.
                      if a deploy wedged and left it stuck, clear the lock to allow a new deploy.
                    </p>
                  </div>
                  <button
                    onClick={onClearLock}
                    className="flex shrink-0 items-center gap-2 rounded-md border border-line px-4 py-2 text-[13px] font-medium text-body transition-colors hover:bg-surface"
                  >
                    clear stuck lock
                  </button>
                </div>
              </div>
            )}

            {/* danger zone: deleting tears the deployment down. */}
            <div className="rounded-xl border border-red-400/30 bg-red-400/[0.03] p-5">
              <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
                <div>
                  <p className="text-[15px] font-medium text-cream">delete this deployment</p>
                  <p className="mt-1 max-w-xl text-[13px] leading-6 text-muted">
                    stops and removes the running container, disables the deploy workflow on the repo, and
                    forgets the record. this cannot be undone.
                  </p>
                </div>
                <button
                  onClick={onDelete}
                  className="flex shrink-0 items-center gap-2 rounded-md border border-red-400/50 px-4 py-2 text-[13px] font-medium text-red-300 transition-colors hover:bg-red-400/10"
                >
                  <Trash /> delete deployment
                </button>
              </div>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

// secretUpdatedLabel renders a secret's GitHub updated-at as a short relative
// time ("3d ago"), or "" when there is no timestamp.
function secretUpdatedLabel(iso: string): string {
  if (!iso) return "";
  const then = new Date(iso).getTime();
  if (!Number.isFinite(then)) return "";
  const secs = Math.max(0, Math.floor((Date.now() - then) / 1000));
  if (secs >= 86400) return `${Math.floor(secs / 86400)}d ago`;
  if (secs >= 3600) return `${Math.floor(secs / 3600)}h ago`;
  if (secs >= 60) return `${Math.floor(secs / 60)}m ago`;
  return "just now";
}

const secretInputCls =
  "min-w-0 flex-1 rounded-md border border-line bg-bg px-3 py-2 font-mono text-[13px] text-cream placeholder:text-faint focus:border-line-strong focus:outline-none";

// applyNotice turns a secret-change ApplyResult into the inline notice: a lime
// "redeploy triggered" when it applied, or a blue warning (the backend's reason,
// e.g. a legacy deployment that needs a full redeploy) when it didn't.
function applyNotice(res: ApplyResult, okText: string): { tone: "info" | "warn"; text: string } {
  if (res.redeployed) return { tone: "info", text: `${okText} Redeploy triggered to apply it.` };
  return { tone: "warn", text: res.message || `${okText} Redeploy to apply it.` };
}

// SecretsPanel is the per-environment secrets tab, laid out as a configuration
// table. GitHub Actions env secrets are the single store: mountabo shows which
// names are configured (with GitHub's updated time), lets you set values
// (write-only) and delete secrets, but can never read a value back — so there is
// no reveal or copy. Operator variables come first; system-managed connection
// secrets sit under a disclosure.
function SecretsPanel({
  owner,
  repo,
  environment,
  data,
  reload,
  siblings,
}: {
  owner: string;
  repo: string;
  environment: string;
  data: DeploymentSecrets | null;
  reload: () => void;
  siblings: Deployment[];
}) {
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<{ tone: "info" | "warn" | "error"; text: string } | null>(null);
  const [confirmRemove, setConfirmRemove] = useState<string | null>(null);
  // The add/edit dialog: null when closed. `name` seeds the field (fixed on edit,
  // editable on add).
  const [dialog, setDialog] = useState<{ mode: "add" | "edit"; name: string } | null>(null);

  // Sibling environments' configured secret names for the compare row. Keyed on a
  // serialized identity string so it refetches only when the set of siblings
  // actually changes, not on every parent re-render.
  const [siblingNames, setSiblingNames] = useState<Record<string, string[]>>({});
  const compareKey = JSON.stringify(siblings.map((s) => [s.repo, s.environment]));
  useEffect(() => {
    const pairs = JSON.parse(compareKey) as [string, string][];
    if (pairs.length === 0) return;
    let live = true;
    const targets = pairs.map(([full, env]) => {
      const [o, r] = splitRepo(full);
      return { owner: o, repo: r, environment: env };
    });
    Promise.all(
      targets.map((t) =>
        fetchDeploymentSecrets(t.owner, t.repo, t.environment).then((d) => ({
          env: t.environment,
          names: d?.secrets.map((x) => x.name) ?? [],
        })),
      ),
    ).then((results) => {
      if (!live) return;
      const map: Record<string, string[]> = {};
      for (const res of results) map[res.env] = res.names;
      setSiblingNames(map);
    });
    return () => {
      live = false;
    };
  }, [compareKey]);

  // write sets or overwrites one secret's value on GitHub (write-only), then the
  // backend re-applies the deployment. Returns whether the write reached GitHub,
  // so the dialog closes on a real write even if the redeploy is deferred. The
  // notice reflects whether the change was actually redeployed.
  async function write(name: string, value: string): Promise<boolean> {
    if (!name.trim() || !value) return false;
    setBusy(true);
    setNotice(null);
    const res = await setDeploymentSecrets(owner, repo, environment, [{ key: name.trim(), value }]);
    setBusy(false);
    if (!res.ok) {
      setNotice({ tone: "error", text: "couldn't set that secret — check you're connected and the name is valid and not managed." });
      return false;
    }
    reload();
    setNotice(applyNotice(res, `Saved ${name.trim()}.`));
    return true;
  }

  async function remove(name: string) {
    setBusy(true);
    setNotice(null);
    const res = await deleteDeploymentSecret(owner, repo, environment, name);
    setBusy(false);
    setConfirmRemove(null);
    if (!res.ok) {
      setNotice({ tone: "error", text: `couldn't remove ${name}.` });
      return;
    }
    reload();
    setNotice(applyNotice(res, `Removed ${name}.`));
  }

  if (data === null) {
    return <p className="text-[14px] text-muted">reading secrets from github...</p>;
  }

  const operator = data.secrets.filter((s) => !s.managed);
  const managed = data.secrets.filter((s) => s.managed);
  const configured = new Set(data.secrets.map((s) => s.name));
  // Names a sibling environment has that this one doesn't, grouped by env.
  const compare = Object.entries(siblingNames)
    .map(([env, names]) => ({ env, missing: names.filter((n) => !configured.has(n)) }))
    .filter((row) => row.missing.length > 0);

  return (
    <div className="space-y-5">
      {notice && (
        <div
          className={`rounded-lg border px-4 py-3 text-[13px] ${
            notice.tone === "error"
              ? "border-red-400/40 bg-red-400/[0.06] text-red-300"
              : notice.tone === "warn"
                ? "border-blue/30 bg-blue/[0.06] text-blue"
                : "border-lime/40 bg-lime/[0.06] text-lime"
          }`}
        >
          {notice.text}
        </div>
      )}

      {/* header: title + write-only framing + the add action. */}
      <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div>
          <h3 className="text-[15px] font-semibold text-cream">environment variables</h3>
          <p className="mt-1 max-w-xl text-[13px] leading-6 text-muted">
            github actions is the source of truth. values are write-only — set or overwrite them here; they
            can&apos;t be read back or revealed. saving triggers a redeploy so the running app picks it up.
            deployments created before this feature need one full redeploy from the configure page first.
          </p>
        </div>
        <button
          onClick={() => setDialog({ mode: "add", name: "" })}
          className="shrink-0 self-start rounded-md border border-lime/50 bg-lime/[0.08] px-4 py-2 text-[13px] font-medium text-lime transition-colors hover:bg-lime/[0.16]"
        >
          + add variable
        </button>
      </div>

      {/* operator variables: the application's own env vars, first and foremost. */}
      {operator.length === 0 ? (
        <p className="rounded-xl border border-dashed border-line px-4 py-8 text-center text-[13px] text-muted">
          no application variables yet. add one, or pick a missing .env.example key below.
        </p>
      ) : (
        <div className="overflow-x-auto rounded-xl border border-line">
          <table className="w-full min-w-[560px] border-collapse text-left text-[13px]">
            <thead>
              <tr className="border-b border-line text-[11px] uppercase tracking-wide text-muted">
                <th className="px-4 py-2.5 font-medium">name</th>
                <th className="px-4 py-2.5 font-medium">value</th>
                <th className="px-4 py-2.5 font-medium">status</th>
                <th className="hidden px-4 py-2.5 font-medium md:table-cell">updated</th>
                <th className="px-4 py-2.5 text-right font-medium">actions</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-line">
              {operator.map((s) => (
                <SecretTableRow
                  key={s.name}
                  secret={s}
                  onEdit={() => setDialog({ mode: "edit", name: s.name })}
                  onRemove={() => setConfirmRemove(s.name)}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/* .env.example variables not configured on this environment */}
      {data.missing.length > 0 && (
        <div className="rounded-xl border border-line bg-surface px-4 py-3">
          <p className="text-[12px] uppercase tracking-wide text-muted">missing from .env.example</p>
          <div className="mt-2 flex flex-wrap gap-2">
            {data.missing.map((name) => (
              <button
                key={name}
                onClick={() => setDialog({ mode: "add", name })}
                className="inline-flex items-center gap-1.5 rounded-md border border-line px-2.5 py-1 font-mono text-[12px] text-cream transition-colors hover:border-lime/50 hover:text-lime"
              >
                {name} <span className="text-muted">+ add</span>
              </button>
            ))}
          </div>
        </div>
      )}

      {/* compare: variable names the repo's other environments have but this lacks */}
      {compare.length > 0 && (
        <div className="rounded-xl border border-line bg-surface px-4 py-3">
          <p className="text-[12px] uppercase tracking-wide text-muted">in other environments, not here</p>
          <ul className="mt-2 space-y-2">
            {compare.map((row) => (
              <li key={row.env} className="flex flex-wrap items-center gap-2 text-[13px]">
                <Badge tone="lime-solid">{row.env}</Badge>
                {row.missing.map((name) => (
                  <button
                    key={name}
                    onClick={() => setDialog({ mode: "add", name })}
                    className="inline-flex items-center gap-1.5 rounded-md border border-line px-2.5 py-1 font-mono text-[12px] text-cream transition-colors hover:border-lime/50 hover:text-lime"
                  >
                    {name} <span className="text-muted">+ add</span>
                  </button>
                ))}
              </li>
            ))}
          </ul>
        </div>
      )}

      {/* system-managed connection secrets: set by mountabo when it configures the
          deploy (server host/user/ssh key + deploy dir). These are NOT application
          variables; they are read-only here and, like all secrets, write-only. */}
      {managed.length > 0 && (
        <details className="overflow-hidden rounded-xl border border-line">
          <summary className="cursor-pointer list-none px-4 py-3 text-[13px] text-muted transition-colors hover:text-cream">
            <span className="font-medium">managed deployment secrets</span> ({managed.length}) — system
            connection secrets, read-only
          </summary>
          <div className="overflow-x-auto border-t border-line">
            <table className="w-full min-w-[480px] border-collapse text-left text-[13px]">
              <tbody className="divide-y divide-line">
                {managed.map((s) => {
                  const updated = secretUpdatedLabel(s.updatedAt);
                  return (
                    <tr key={s.name}>
                      <td className="px-4 py-3 font-mono text-cream">{s.name}</td>
                      <td className="px-4 py-3 text-muted">managed by mountabo</td>
                      <td className="hidden px-4 py-3 text-right text-muted md:table-cell">
                        {updated ? `updated ${updated}` : ""}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </details>
      )}

      {dialog && (
        <SecretDialog
          mode={dialog.mode}
          initialName={dialog.name}
          environment={environment}
          busy={busy}
          onSubmit={async (name, value) => {
            const ok = await write(name, value);
            if (ok) setDialog(null);
            return ok;
          }}
          onClose={() => setDialog(null)}
        />
      )}

      {confirmRemove && (
        <ConfirmAction
          title={`remove ${confirmRemove}?`}
          summary={
            <>
              this deletes <span className="text-cream">{confirmRemove}</span> from the github{" "}
              <span className="text-cream">{environment}</span> environment. deployments that read it will
              lose it on their next run.
            </>
          }
          steps={[`delete the secret ${confirmRemove} from the ${environment} environment on github`]}
          confirmLabel="remove secret"
          destructive
          onConfirm={() => remove(confirmRemove)}
          onCancel={() => setConfirmRemove(null)}
        />
      )}
    </div>
  );
}

// SecretTableRow renders one operator secret as a configuration-table row. GitHub
// secret values are write-only, so the value cell is a fixed write-only marker
// (never a real or masked value); edit overwrites it, delete removes it.
function SecretTableRow({
  secret: s,
  onEdit,
  onRemove,
}: {
  secret: DeploymentSecret;
  onEdit: () => void;
  onRemove: () => void;
}) {
  const updated = secretUpdatedLabel(s.updatedAt);
  return (
    <tr className="align-middle">
      <td className="px-4 py-3 font-mono text-cream">{s.name}</td>
      <td className="px-4 py-3">
        <span className="inline-flex items-center gap-2 font-mono text-[12px] text-muted">
          <span aria-hidden>............</span>
          <span className="text-[11px] uppercase tracking-wide text-faint">write-only</span>
        </span>
      </td>
      <td className="px-4 py-3">
        <Badge tone="gray">configured</Badge>
      </td>
      <td className="hidden px-4 py-3 text-muted md:table-cell">{updated}</td>
      <td className="px-4 py-3">
        <div className="flex items-center justify-end gap-1.5">
          <SecretAction label="edit" onClick={onEdit} />
          <SecretAction label="delete" onClick={onRemove} danger />
        </div>
      </td>
    </tr>
  );
}

// SecretAction is one compact text action in a secret row's actions cell.
function SecretAction({
  label,
  onClick,
  danger = false,
}: {
  label: string;
  onClick: () => void;
  danger?: boolean;
}) {
  return (
    <button
      onClick={onClick}
      className={`rounded-md border px-2.5 py-1 text-[12px] transition-colors ${
        danger
          ? "border-red-400/40 text-red-300 hover:bg-red-400/10"
          : "border-line text-muted hover:text-cream"
      }`}
    >
      {label}
    </button>
  );
}

// SecretDialog is the focused add/edit surface. The value is a password field and
// is written straight through to GitHub Actions; the name is read-only when
// editing (rename = delete + add). GitHub values are write-only, so editing means
// overwriting with a new value, not viewing the current one.
function SecretDialog({
  mode,
  initialName,
  environment,
  busy,
  onSubmit,
  onClose,
}: {
  mode: "add" | "edit";
  initialName: string;
  environment: string;
  busy: boolean;
  onSubmit: (name: string, value: string) => Promise<boolean>;
  onClose: () => void;
}) {
  const [name, setName] = useState(initialName);
  const [value, setValue] = useState("");
  const nameInvalid = name.trim() !== "" && !/^[A-Za-z_][A-Za-z0-9_]*$/.test(name.trim());
  // Values are injected as single .env lines, so a newline can't be represented;
  // reject it client-side for fast feedback (the backend rejects it too).
  const valueInvalid = /[\r\n]/.test(value);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-3 sm:p-6"
      onClick={onClose}
    >
      <form
        role="dialog"
        aria-modal="true"
        onClick={(e) => e.stopPropagation()}
        onSubmit={async (e) => {
          e.preventDefault();
          if (!name.trim() || !value || nameInvalid || valueInvalid) return;
          await onSubmit(name.trim(), value);
        }}
        className="w-full max-w-md overflow-hidden rounded-xl border border-line bg-surface"
      >
        <div className="flex items-center justify-between gap-3 border-b border-line px-5 py-3">
          <span className="text-[13px] text-cream">
            {mode === "add" ? "add variable" : `edit ${initialName}`}
            <span className="text-faint"> · {environment}</span>
          </span>
          <button
            type="button"
            onClick={onClose}
            className="shrink-0 rounded-md border border-line px-2.5 py-1 text-[12px] text-muted transition-colors hover:text-cream"
          >
            close
          </button>
        </div>
        <div className="space-y-3 p-5">
          <div>
            <label className="text-[12px] uppercase tracking-wide text-muted">name</label>
            {mode === "edit" ? (
              <p className="mt-1 font-mono text-[13px] text-cream">{initialName}</p>
            ) : (
              <input
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="DATABASE_URL"
                spellCheck={false}
                autoCapitalize="off"
                autoCorrect="off"
                autoFocus
                className={`mt-1 w-full ${secretInputCls}`}
              />
            )}
            {nameInvalid && (
              <p className="mt-1 text-[12px] text-red-400">
                names use letters, digits, and underscores, and can&apos;t start with a digit.
              </p>
            )}
          </div>
          <div>
            <label className="text-[12px] uppercase tracking-wide text-muted">
              {mode === "edit" ? "new value" : "value"}
            </label>
            <input
              value={value}
              onChange={(e) => setValue(e.target.value)}
              placeholder="value"
              type="password"
              autoComplete="new-password"
              spellCheck={false}
              autoFocus={mode === "edit"}
              className={`mt-1 w-full ${secretInputCls}`}
            />
            {valueInvalid && (
              <p className="mt-1 text-[12px] text-red-400">
                values can&apos;t contain line breaks (multiline values aren&apos;t supported).
              </p>
            )}
          </div>
          <p className="text-[12px] leading-6 text-muted">
            {mode === "edit" ? "overwrites" : "sets"} the value on the github{" "}
            <span className="text-cream">{environment}</span> environment and triggers a redeploy. values
            are write-only and can&apos;t be read back.
          </p>
        </div>
        <div className="flex items-center justify-end gap-2 border-t border-line px-5 py-3">
          <button
            type="button"
            onClick={onClose}
            className="rounded-md border border-line px-4 py-2 text-[13px] text-muted transition-colors hover:text-cream"
          >
            cancel
          </button>
          <button
            type="submit"
            disabled={busy || name.trim() === "" || value === "" || nameInvalid || valueInvalid}
            className="rounded-md border border-lime/50 bg-lime/[0.08] px-4 py-2 text-[13px] font-medium text-lime transition-colors hover:bg-lime/[0.16] disabled:cursor-not-allowed disabled:opacity-40"
          >
            {busy ? "saving..." : "save secret"}
          </button>
        </div>
      </form>
    </div>
  );
}

// DashboardsPanel shows each installed monitoring tool's dashboard, reached
// through an SSH local port-forward tunnel the backend opens on demand (the tool
// binds to the server's loopback, so it is never exposed publicly). When the
// section is open it asks the backend to open the tunnel, gets back a local
// http://127.0.0.1:<port>/ URL, and loads it straight in an iframe: because the
// tunnel carries raw TCP, the tool is served at root and its websockets connect
// directly. It also offers an "open in a tab" link to that same local URL.
function DashboardsPanel({
  serverId,
  dashboards,
  active,
}: {
  serverId: string;
  dashboards: DashboardTool[];
  active: boolean;
}) {
  return (
    <div className="space-y-5">
      {dashboards.map((tool) => (
        <DashboardCard key={tool.id} serverId={serverId} tool={tool} active={active} />
      ))}
    </div>
  );
}

// DashboardCard opens (and reuses) one tool's tunnel when its section is active,
// then loads the returned local URL in an iframe. It surfaces an honest opening
// state and an error state rather than a silently broken frame.
function DashboardCard({
  serverId,
  tool,
  active,
}: {
  serverId: string;
  tool: DashboardTool;
  active: boolean;
}) {
  // url is the local tunnel address once open; errored is set when it could not
  // be opened. "opening" is derived (active, no url, not errored), so the effect
  // never sets state synchronously. nonce re-triggers the open on retry.
  const [url, setUrl] = useState<string | null>(null);
  const [errored, setErrored] = useState(false);
  const [nonce, setNonce] = useState(0);
  // Uptime Kuma has no public HTTP setup endpoint, so the operator can never
  // guess the admin password. mountabo offers to generate one and seed it into
  // UK's SQLite from inside the container; the result is shown above the
  // iframe so the operator can copy it into the login form below. null = not
  // loaded; { hasAdmin:false } = none stored; { hasAdmin:true, ... } = stored.
  const isKuma = tool.id === "uptime-kuma";
  const [admin, setAdmin] = useState<UptimeKumaAdmin | null>(null);
  const [resetting, setResetting] = useState(false);
  const [showPassword, setShowPassword] = useState(false);

  // Read the stored credentials the first time the section becomes active. The
  // setState only runs once the fetch resolves, never synchronously here.
  useEffect(() => {
    if (!isKuma || !active || admin !== null) return;
    const ctrl = new AbortController();
    fetchUptimeKumaAdmin(serverId, ctrl.signal).then((res) => {
      if (!ctrl.signal.aborted) setAdmin(res);
    });
    return () => ctrl.abort();
  }, [isKuma, active, serverId, admin]);

  async function setUpAdmin() {
    if (resetting) return;
    setResetting(true);
    const next = await resetUptimeKumaAdmin(serverId);
    if (next) {
      setAdmin(next);
      setShowPassword(true);
    }
    setResetting(false);
  }

  // Open the tunnel the first time the section becomes active. The backend
  // reuses an existing tunnel for the same (server, tool) pair, so re-opening is
  // cheap. setState only runs once the fetch resolves, never synchronously.
  useEffect(() => {
    if (!active || url !== null || errored) return;
    const ctrl = new AbortController();
    openDashboard(serverId, tool.id, ctrl.signal).then((res) => {
      if (ctrl.signal.aborted) return;
      if (res) setUrl(res.url);
      else setErrored(true);
    });
    return () => ctrl.abort();
  }, [active, serverId, tool.id, url, errored, nonce]);

  function retry() {
    setUrl(null);
    setErrored(false);
    setNonce((n) => n + 1);
  }

  return (
    <div className="overflow-hidden rounded-xl border border-line bg-surface">
      <div className="flex items-center justify-between gap-3 border-b border-line px-5 py-3.5">
        <span className="flex items-center gap-2.5">
          <span className="h-2 w-2 rounded-full bg-lime" />
          <span className="text-[15px] font-medium text-cream">{tool.label}</span>
        </span>
      </div>
      {isKuma && (
        <div className="border-b border-line bg-surface/60 px-5 py-3.5">
          {admin === null ? (
            <p className="text-[12.5px] text-muted">reading admin credentials…</p>
          ) : admin.hasAdmin ? (
            <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
              <div className="min-w-0 text-[12.5px] leading-6 text-body">
                <p className="text-muted">
                  log in with these credentials. mountabo generated them and seeded them into uptime kuma.
                </p>
                <p className="mt-1 flex flex-wrap items-center gap-x-4 gap-y-1 font-mono text-[13px]">
                  <span>
                    <span className="text-muted">username</span>{" "}
                    <span className="select-all text-cream">{admin.username}</span>
                  </span>
                  <span>
                    <span className="text-muted">password</span>{" "}
                    <span className="select-all text-cream">
                      {showPassword ? admin.password : "•".repeat(admin.password.length)}
                    </span>
                  </span>
                  <button
                    onClick={() => setShowPassword((v) => !v)}
                    className="rounded-md border border-line px-2 py-0.5 text-[11.5px] text-muted transition-colors hover:border-line-strong hover:text-cream"
                  >
                    {showPassword ? "hide" : "show"}
                  </button>
                </p>
              </div>
              <button
                onClick={() => void setUpAdmin()}
                disabled={resetting}
                className="shrink-0 rounded-md border border-line px-3 py-1.5 text-[12px] text-muted transition-colors hover:border-line-strong hover:text-cream disabled:opacity-50"
              >
                {resetting ? "regenerating…" : "regenerate"}
              </button>
            </div>
          ) : (
            <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
              <p className="text-[12.5px] leading-6 text-body">
                uptime kuma is asking for an admin to log in, but it has no public setup api. click set up
                admin and mountabo will generate a password, seed it directly into the container, and show
                it once.
              </p>
              <button
                onClick={() => void setUpAdmin()}
                disabled={resetting}
                className="shrink-0 rounded-md border border-lime/50 bg-lime/[0.08] px-4 py-2 text-[12.5px] font-medium text-lime transition-colors hover:bg-lime/[0.16] disabled:opacity-50"
              >
                {resetting ? "setting up…" : "set up admin"}
              </button>
            </div>
          )}
        </div>
      )}
      {url ? (
        <div className="relative">
          {/* The backend's reverse proxy strips X-Frame-Options and CSP from the
              dashboard's responses, so the iframe loads even though it is a
              different origin from the app. A "pop out" link is kept beside it
              for full-screen use. */}
          <iframe
            src={url}
            title={tool.label}
            className="block h-[640px] w-full border-0 bg-black"
            // Allow same-origin so the dashboard's cookies/auth keep working,
            // and allow scripts + forms so its UI is interactive.
            sandbox="allow-same-origin allow-scripts allow-forms allow-popups allow-modals"
          />
          <a
            href={url}
            target="_blank"
            rel="noopener noreferrer"
            className="absolute right-3 top-3 flex items-center gap-1.5 rounded-md border border-line bg-surface/95 px-2.5 py-1.5 text-[12px] text-muted backdrop-blur transition-colors hover:border-line-strong hover:text-cream"
          >
            open in a tab <ExternalLink />
          </a>
        </div>
      ) : errored ? (
        <div className="flex h-[200px] flex-col items-center justify-center gap-3 bg-black/40 px-5 text-center">
          <p className="text-[14px] leading-7 text-body">
            could not open the {tool.label} tunnel over ssh. the server may be unreachable, or the tool may
            not be running.
          </p>
          <button
            onClick={retry}
            className="flex items-center gap-1.5 rounded-md border border-line px-3 py-1.5 text-[13px] text-lime transition-colors hover:bg-lime/10"
          >
            <Refresh /> try again
          </button>
        </div>
      ) : (
        <div className="flex h-[200px] items-center justify-center bg-black/40">
          <p className="text-[13px] text-muted">opening the dashboard through the ssh tunnel…</p>
        </div>
      )}
    </div>
  );
}

// RunWalkthrough fetches the open deployment's latest GitHub Actions run and
// renders its jobs and steps with live status, polling while the run is in
// progress and stopping once it completes.
function RunWalkthrough({
  owner,
  repo,
  branch,
  environment,
  latestStatus,
}: {
  owner: string;
  repo: string;
  branch: string;
  environment: string;
  latestStatus?: RunStatus;
}) {
  const [steps, setSteps] = useState<RunSteps | null>(null);
  // null = not read yet; the effect resolves it on mount and on each poll.
  const enabled = Boolean(owner && repo && branch);

  useEffect(() => {
    if (!enabled) return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;

    async function tick() {
      const ctrl = new AbortController();
      const res = await fetchRunSteps(owner, repo, branch, environment, ctrl.signal);
      if (cancelled) return;
      setSteps(res);
      // Keep polling only while the run is still going; stop once it completes.
      if (runActive(res)) timer = setTimeout(tick, STEP_POLL_MS);
    }

    void tick();
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
    // latestStatus is included so a refresh that flips the run back to running
    // (a new deploy) restarts polling.
  }, [enabled, owner, repo, branch, environment, latestStatus]);

  if (enabled && steps === null) {
    return <p className="mt-4 text-[13px] text-muted">reading the latest workflow run…</p>;
  }
  if (!steps || (!steps.runUrl && steps.jobs.length === 0)) {
    return (
      <p className="mt-4 rounded-xl border border-dashed border-line px-5 py-8 text-center text-[13px] text-muted">
        no workflow run found for {branch} yet. push to {branch} or trigger the deploy workflow and the
        steps show up here.
      </p>
    );
  }

  const active = runActive(steps);
  return (
    <div className="mt-4 space-y-4">
      <div className="flex items-center gap-3 text-[12.5px]">
        <span className={`flex items-center gap-2 ${active ? "text-lime" : "text-muted"}`}>
          <span className={`h-1.5 w-1.5 rounded-full ${active ? "animate-pulse bg-lime" : "bg-muted"}`} />
          {active ? "run in progress, refreshing every few seconds" : "run finished"}
        </span>
        {steps.runUrl && (
          <a
            href={steps.runUrl}
            target="_blank"
            rel="noopener noreferrer"
            className="ml-auto flex items-center gap-1.5 text-muted transition-colors hover:text-cream"
          >
            <GithubMark /> view on github <ExternalLink />
          </a>
        )}
      </div>

      {steps.jobs.length === 0 ? (
        <p className="text-[13px] text-muted">the run has no jobs reported yet.</p>
      ) : (
        <div className="space-y-4">
          {steps.jobs.map((job, ji) => (
            <JobPanel key={job.jobId || ji} owner={owner} repo={repo} job={job} />
          ))}
        </div>
      )}
    </div>
  );
}

// JobPanel renders one job of the run: its steps with live status, and an
// expandable log of what the job printed so the operator can read what worked
// and what failed without leaving the page. A failed job opens by default so the
// error is visible immediately. The log loads lazily the first time the panel
// opens (and only setting state inside the resolved fetch, never synchronously
// in the effect body).
function JobPanel({ owner, repo, job }: { owner: string; repo: string; job: RunJob }) {
  const failed = job.conclusion === "failure" || job.conclusion === "timed_out";
  const [open, setOpen] = useState(failed);
  const [log, setLog] = useState<string[] | null>(null); // null = not loaded, [] = loaded empty

  useEffect(() => {
    if (!open || log !== null || !job.jobId) return;
    const ctrl = new AbortController();
    fetchJobLogs(owner, repo, job.jobId, ctrl.signal).then((lines) => {
      if (!ctrl.signal.aborted) setLog(lines);
    });
    return () => ctrl.abort();
  }, [open, log, owner, repo, job.jobId]);

  return (
    <div className="overflow-hidden rounded-xl border border-line bg-surface">
      <button
        onClick={() => setOpen((v) => !v)}
        className="flex w-full items-center gap-3 border-b border-line px-5 py-3.5 text-left"
        aria-expanded={open}
      >
        <ChevronRight className={`text-muted transition-transform ${open ? "rotate-90" : ""}`} />
        <StepIcon status={job.status} conclusion={job.conclusion} />
        <span className="flex-1 text-[14px] font-medium text-cream">{job.name}</span>
        <StepStatusLabel status={job.status} conclusion={job.conclusion} />
        {job.htmlUrl && (
          <a
            href={job.htmlUrl}
            target="_blank"
            rel="noopener noreferrer"
            onClick={(e) => e.stopPropagation()}
            className="text-muted transition-colors hover:text-cream"
            aria-label={`open job ${job.name} on github`}
          >
            <ExternalLink />
          </a>
        )}
      </button>

      <ol className="divide-y divide-line">
        {job.steps.map((s, si) => (
          <li key={si} className="flex items-center gap-3 px-5 py-2.5 text-[13px]">
            <StepIcon status={s.status} conclusion={s.conclusion} />
            <span className="flex-1 text-body">{s.name}</span>
            <StepStatusLabel status={s.status} conclusion={s.conclusion} />
          </li>
        ))}
        {job.steps.length === 0 && (
          <li className="px-5 py-2.5 text-[12.5px] text-muted">no steps reported yet</li>
        )}
      </ol>

      {open && (
        <div className="border-t border-line">
          <div className="flex items-center justify-between px-5 py-2.5">
            <span className="flex items-center gap-2 text-[12px] text-muted">
              <Terminal className="text-faint" /> job log
            </span>
            <button
              onClick={() => setLog(null)}
              className="flex items-center gap-1.5 text-[12px] text-lime transition-colors hover:text-cream"
            >
              <Refresh /> refresh
            </button>
          </div>
          <div className="max-h-80 overflow-y-auto overscroll-contain bg-black/40 px-5 py-4 font-mono text-[12px] leading-6">
            <JobLogLines lines={log} />
          </div>
        </div>
      )}
    </div>
  );
}

// JobLogLines renders a job's raw log: each line leads with its emphasised date
// and time, GitHub's ##[group] markers become muted section labels, and
// ##[error]/##[warning] lines are coloured so a failure stands out.
function JobLogLines({ lines }: { lines: string[] | null }) {
  if (lines === null) return <p className="text-muted">reading the job log…</p>;
  if (lines.length === 0) return <p className="text-muted">no log output for this job.</p>;
  return (
    <>
      {lines.map((raw, i) => {
        const { ts, text } = splitLogTimestamp(raw);
        const { kind, text: clean } = classifyJobLogLine(text);
        if (kind === "group" && clean === "") return null; // drop endgroup markers
        const tone =
          kind === "error"
            ? "text-red-400"
            : kind === "warning"
              ? "text-amber-300"
              : kind === "group"
                ? "text-muted font-semibold uppercase tracking-wide text-[11px]"
                : kind === "command"
                  ? "text-blue"
                  : "text-body";
        return (
          <p key={i} className={`whitespace-pre-wrap break-words ${tone}`}>
            {ts && <span className="mr-3 select-none font-semibold text-cream">{formatLogTimestamp(ts)}</span>}
            {clean}
          </p>
        );
      })}
    </>
  );
}

// LogsViewer fetches and shows the open server's running container logs in a
// dark, scrollable terminal panel, with a refresh control and an empty state.
function LogsViewer({ serverId, ready, active }: { serverId: string; ready: boolean; active: boolean }) {
  // null = not read yet (shows "reading…"); [] = read, but no logs.
  const [lines, setLines] = useState<string[] | null>(null);
  // Tracks an in-flight refresh triggered from the button (an event handler).
  const [refreshing, setRefreshing] = useState(false);
  // The picked service chip: "" = all containers, otherwise the container name
  // (which on a compose stack is "<project>-<service>-<index>"). One container
  // per chip in the row above the log body, so the operator can zoom into the
  // app's own service instead of reading every line a compose stack prints.
  const [pickedService, setPickedService] = useState<string>("");
  const scrollRef = useRef<HTMLDivElement>(null);

  // Read once the section is open (not just the card), so collapsed logs do not
  // pull over SSH until the operator expands them. setState only happens once
  // the fetch resolves, so the effect never sets state synchronously.
  useEffect(() => {
    if (!serverId || !active || lines !== null) return;
    const ctrl = new AbortController();
    fetchServerLogs(serverId, 200, ctrl.signal).then((res) => {
      if (!ctrl.signal.aborted) setLines(res?.lines ?? []);
    });
    return () => ctrl.abort();
  }, [serverId, active, lines]);

  // Manual refresh from the button (an event handler, not an effect).
  async function refresh() {
    if (!serverId || refreshing) return;
    setRefreshing(true);
    const res = await fetchServerLogs(serverId, 200);
    setLines(res?.lines ?? []);
    setRefreshing(false);
  }

  useEffect(() => {
    scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight });
  }, [lines, pickedService]);

  // Containers found in the log output, in the order they first appear. The
  // chip row below lets the operator filter to one service's lines (the app's
  // own output, instead of every container in the compose stack).
  const containers: string[] = [];
  if (lines) {
    for (const line of lines) {
      if (isLogHeader(line)) {
        const name = logHeaderName(line);
        if (!containers.includes(name)) containers.push(name);
      }
    }
  }

  // Filter lines to the picked container's group: include the header itself
  // plus every line up to the next header. With no pick, show everything.
  const visibleLines: string[] = [];
  if (lines) {
    let inPicked = pickedService === "";
    for (const line of lines) {
      if (isLogHeader(line)) {
        inPicked = pickedService === "" || logHeaderName(line) === pickedService;
      }
      if (inPicked) visibleLines.push(line);
    }
  }

  return (
    <div className="mt-4 overflow-hidden rounded-xl border border-line bg-black/40">
      <div className="flex items-center justify-between border-b border-line px-5 py-3">
        <span className="flex items-center gap-2 text-[12.5px] text-muted">
          <Terminal className="text-faint" /> {pickedService === "" ? "running containers" : pickedService} · last 200 lines
        </span>
        <button
          onClick={() => void refresh()}
          disabled={refreshing || !ready}
          className="flex items-center gap-1.5 rounded-md border border-line px-2.5 py-1 text-[12px] text-lime transition-colors hover:bg-lime/10 disabled:opacity-50"
        >
          <Refresh className={refreshing ? "animate-spin" : ""} /> {refreshing ? "reading…" : "refresh"}
        </button>
      </div>
      {/* Service chips: "all" + each running container. Same segmented-control
          pattern as the home page filter chips, so an operator can switch from
          the full container stack to one service's lines and back. */}
      {containers.length > 1 && (
        <div className="flex flex-wrap items-center gap-x-3 gap-y-2 border-b border-line px-5 py-3">
          <span className="text-[11px] font-medium uppercase tracking-wide text-muted">log</span>
          <span className="inline-flex flex-wrap items-center gap-1 rounded-lg border border-line bg-surface p-1">
            <button
              onClick={() => setPickedService("")}
              className={`rounded-md px-3 py-1 text-[12px] font-medium transition-colors ${
                pickedService === ""
                  ? "bg-lime/15 text-lime"
                  : "text-muted hover:bg-surface-2 hover:text-cream"
              }`}
            >
              every container
            </button>
            {containers.map((name) => (
              <button
                key={name}
                onClick={() => setPickedService(name)}
                className={`rounded-md px-3 py-1 text-[12px] font-medium transition-colors ${
                  pickedService === name
                    ? "bg-lime/15 text-lime"
                    : "text-muted hover:bg-surface-2 hover:text-cream"
                }`}
              >
                {name}
              </button>
            ))}
          </span>
        </div>
      )}
      <div
        ref={scrollRef}
        className="h-72 overflow-y-auto overscroll-contain px-5 py-4 font-mono text-[12px] leading-6"
      >
        {!ready ? (
          <p className="text-muted">this server is not set up yet, so it has no container logs.</p>
        ) : lines === null ? (
          <p className="text-muted">reading container logs…</p>
        ) : lines.length === 0 ? (
          <p className="text-muted">no logs yet. once a container is running its output shows here.</p>
        ) : visibleLines.length === 0 ? (
          <p className="text-muted">no lines for {pickedService} yet.</p>
        ) : (
          visibleLines.map((line, i) => {
            if (isLogHeader(line)) {
              // When the operator has zoomed in on one service, the header
              // becomes redundant (the chip already names it), so it is
              // suppressed and only the lines below it render.
              if (pickedService !== "") return null;
              return (
                <p key={i} className="mt-3 first:mt-0 text-[11px] font-semibold uppercase tracking-wide text-lime">
                  {logHeaderName(line)}
                </p>
              );
            }
            const { ts, text } = splitLogTimestamp(line);
            return (
              <p key={i} className="whitespace-pre-wrap break-words text-body">
                {ts && (
                  <span className="mr-3 select-none font-semibold text-cream">{formatLogTimestamp(ts)}</span>
                )}
                {text}
              </p>
            );
          })
        )}
      </div>
    </div>
  );
}


// Builds the add-domain SSE URL: ?host=&upstream=&aliases=&email=&staging=.
function addDomainUrl(serverId: string, v: DomainFormValue): string {
  const qs = new URLSearchParams();
  qs.set("host", v.host);
  if (v.upstream) qs.set("upstream", v.upstream);
  if (v.aliases.length) qs.set("aliases", v.aliases.join(","));
  if (v.email) qs.set("email", v.email);
  if (v.staging) qs.set("staging", "1");
  return `/api/servers/${serverId}/domains/add?${qs.toString()}`;
}

function removeDomainUrl(serverId: string, host: string): string {
  return `/api/servers/${serverId}/domains/remove?host=${encodeURIComponent(host)}`;
}

// Splits an "owner/repo" string into its parts for the run-steps fetch.
function splitRepo(full: string): [string, string] {
  const slash = full.indexOf("/");
  if (slash < 0) return ["", full];
  return [full.slice(0, slash), full.slice(slash + 1)];
}

function RunStrip({ runs }: { runs: DeployRun[] }) {
  return (
    <span className="hidden items-center gap-1 md:flex" title="recent deploys">
      {runs.map((r, i) => (
        <span
          key={`${r.runUrl || r.sha}:${i}`}
          className={`h-5 w-1.5 rounded-sm ${runColor[r.status]} ${
            r.status === "running" ? "animate-pulse" : ""
          }`}
        />
      ))}
    </span>
  );
}

function BigMetric({ label, value }: { label: string; value: string }) {
  return (
    <div className="bg-surface px-5 py-4">
      <p className="text-[11px] uppercase tracking-wide text-muted">{label}</p>
      <p className="mt-1.5 text-[18px] font-semibold text-cream">{value}</p>
    </div>
  );
}

function DeployStatusBadge({ status }: { status?: RunStatus }) {
  if (!status) {
    return (
      <span className="flex h-11 w-11 items-center justify-center rounded-full border border-line text-muted">
        <CircleDot />
      </span>
    );
  }
  if (status === "success") {
    return (
      <span className="flex h-11 w-11 items-center justify-center rounded-full border border-blue/40 text-blue">
        <CircleCheck />
      </span>
    );
  }
  if (status === "failed") {
    return (
      <span className="flex h-11 w-11 items-center justify-center rounded-full border border-red-500/40 text-red-400">
        <CircleX />
      </span>
    );
  }
  return (
    <span className="flex h-11 w-11 items-center justify-center rounded-full border border-lime/40 text-lime">
      <CircleDot className="animate-pulse" />
    </span>
  );
}

function RunRow({ run }: { run: DeployRun }) {
  const tone = run.status === "success" ? "blue" : run.status === "failed" ? "red" : "lime";
  return (
    <li className="flex items-center gap-3 px-5 py-3 text-[13px]">
      <span className={`h-1.5 w-1.5 shrink-0 rounded-full ${runColor[run.status]}`} />
      {run.commitUrl ? (
        <a
          href={run.commitUrl}
          target="_blank"
          rel="noopener noreferrer"
          className="shrink-0 font-mono text-cream transition-colors hover:text-lime hover:underline"
        >
          {run.sha}
        </a>
      ) : (
        <code className="shrink-0 text-cream">{run.sha}</code>
      )}
      <span className="min-w-0 flex-1 truncate text-body">{run.message}</span>
      <Badge tone={tone}>{run.status}</Badge>
      <span className="w-16 text-right text-muted">{run.duration}</span>
      <span className="hidden w-16 text-right text-muted sm:inline">{run.when}</span>
      {run.runUrl ? (
        <a
          href={run.runUrl}
          target="_blank"
          rel="noopener noreferrer"
          className="shrink-0 text-muted transition-colors hover:text-cream"
          aria-label={`open run ${run.sha} on github`}
        >
          <ExternalLink />
        </a>
      ) : (
        <span className="w-3.5 shrink-0" />
      )}
    </li>
  );
}

// ReleaseRow renders one entry of a deployment's release history: its status
// (overlaid live from the matching Actions run when available), the deployed
// commit, the environment, who triggered it, and when. Read-only; rollback
// actions come in a later phase.
function ReleaseRow({ release }: { release: Release }) {
  const hasSHA = release.sha && release.sha !== "n/a";
  return (
    <li className="flex items-center gap-3 px-5 py-3 text-[13px]">
      <span className={`h-1.5 w-1.5 shrink-0 rounded-full ${releaseColor[release.status]}`} />
      {hasSHA ? (
        <code className="shrink-0 font-mono text-cream">{release.sha}</code>
      ) : (
        <span className="w-14 shrink-0 text-muted">n/a</span>
      )}
      <span className="min-w-0 flex-1 truncate text-body">{release.environment}</span>
      <Badge tone={releaseTone[release.status]}>{release.status}</Badge>
      {release.actor && (
        <span className="hidden text-muted sm:inline">{release.actor}</span>
      )}
      <span className="w-16 text-right text-muted">{release.when}</span>
      {release.runUrl ? (
        <a
          href={release.runUrl}
          target="_blank"
          rel="noopener noreferrer"
          className="shrink-0 text-muted transition-colors hover:text-cream"
          aria-label={`open the run for release ${release.sha} on github`}
        >
          <ExternalLink />
        </a>
      ) : (
        <span className="w-3.5 shrink-0" />
      )}
    </li>
  );
}

// StepIcon maps GitHub's run/step status + conclusion to a status dot/check.
function StepIcon({ status, conclusion }: { status: GithubStatus; conclusion: GithubConclusion }) {
  if (status === "completed") {
    if (conclusion === "success")
      return <CircleCheck className="shrink-0 text-blue" />;
    if (conclusion === "failure" || conclusion === "timed_out")
      return <CircleX className="shrink-0 text-red-400" />;
    // cancelled / skipped / other
    return <CircleDot className="shrink-0 text-muted" />;
  }
  if (status === "in_progress")
    return <CircleDot className="shrink-0 animate-pulse text-lime" />;
  // queued / unknown
  return <CircleDot className="shrink-0 text-faint" />;
}

// StepStatusLabel is the short, plain-English status next to a job/step.
function StepStatusLabel({ status, conclusion }: { status: GithubStatus; conclusion: GithubConclusion }) {
  let label: string;
  let cls: string;
  if (status === "completed") {
    if (conclusion === "success") {
      label = "done";
      cls = "text-blue";
    } else if (conclusion === "failure" || conclusion === "timed_out") {
      label = "failed";
      cls = "text-red-400";
    } else if (conclusion === "skipped") {
      label = "skipped";
      cls = "text-muted";
    } else if (conclusion === "cancelled") {
      label = "cancelled";
      cls = "text-muted";
    } else {
      label = "done";
      cls = "text-muted";
    }
  } else if (status === "in_progress") {
    label = "running";
    cls = "text-lime";
  } else if (status === "waiting") {
    // GitHub holds the job until a required reviewer (an admin) approves it.
    label = "needs approval";
    cls = "text-amber-300";
  } else if (status === "queued") {
    label = "queued";
    cls = "text-faint";
  } else {
    label = "waiting";
    cls = "text-faint";
  }
  return <span className={`shrink-0 text-[12px] ${cls}`}>{label}</span>;
}
