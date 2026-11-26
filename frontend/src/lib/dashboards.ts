// The self-hosted monitoring tools Berth can surface from a deployment. Each
// binds to the server's loopback; it's opened through a domain on the server
// (the reverse proxy forwards HTTPS and websockets to the tool's port), the
// same way an app gets a domain.

export type DashboardTool = {
  // id is the hardening option id, matched against a server's options.
  id: string;
  label: string;
  // port is the loopback port the tool listens on; its domain proxies here.
  port: number;
  // note is the one line explanation shown with the dashboard.
  note: string;
};

export const DASHBOARD_TOOLS: DashboardTool[] = [
  {
    id: "uptime-kuma",
    label: "Uptime Kuma",
    port: 3001,
    note: "Checks your apps every minute, keeps uptime history, and alerts you (Slack, email, Telegram…) when one goes down.",
  },
];

// installedDashboards returns the dashboards whose tool is in the server's
// applied options, in catalog order, so only what is actually installed shows.
export function installedDashboards(options: string[] | null | undefined): DashboardTool[] {
  const set = new Set(options ?? []);
  return DASHBOARD_TOOLS.filter((t) => set.has(t.id));
}
