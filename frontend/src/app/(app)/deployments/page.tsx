import { Suspense } from "react";
import { MonitorView } from "@/components/monitor-view";
import { getDeployments } from "@/lib/deployments";
import { getServers } from "@/lib/servers";
import { getCurrentUser } from "@/lib/session";

export const metadata = { title: "Deployments" };

export default function MonitorPage() {
  // /api/deployments fans out to GitHub Actions for every deployment, so it is
  // the slow call here; the shell paints now and the monitor streams in.
  return (
    <Suspense fallback={<MonitorSkeleton />}>
      <MonitorData />
    </Suspense>
  );
}

async function MonitorData() {
  // Deployments + servers feed the expanded card. ServerView drives the domain
  // panel and the monitoring-dashboards tunnel.
  const [deployments, servers, user] = await Promise.all([
    getDeployments(),
    getServers(),
    getCurrentUser(),
  ]);

  const now = new Date();
  const stamp = now.toLocaleTimeString("en-GB", {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    timeZone: "GMT",
  });

  return (
    <MonitorView deployments={deployments} servers={servers} stamp={`${stamp} GMT`} isAdmin={Boolean(user?.isAdmin)} />
  );
}

function MonitorSkeleton() {
  return (
    <main className="mx-auto w-full max-w-[1200px] flex-1 px-4 py-8 sm:px-6 lg:px-8" aria-busy="true">
      <div className="h-3 w-24 animate-pulse rounded bg-surface-2" />
      <div className="mt-5 h-8 w-full max-w-72 animate-pulse rounded-lg bg-surface-2" />
      <div className="mt-8 space-y-3">
        {Array.from({ length: 4 }).map((_, i) => (
          <div key={i} className="h-24 animate-pulse rounded-xl border border-line bg-surface" />
        ))}
      </div>
    </main>
  );
}
