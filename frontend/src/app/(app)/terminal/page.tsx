import { redirect } from "next/navigation";
import { TerminalConsole } from "@/components/terminal-console";
import { getServers } from "@/lib/servers";
import { getGithubConnection } from "@/lib/session";

// The terminal page lets the operator run a single shell command on one of their
// set-up servers over SSH, and ask an AI helper to suggest a command for a plain
// English request. The AI helper only suggests; the human reviews and runs every
// command through a confirmation gate, so nothing is ever auto executed.
export const metadata = { title: "Terminal" };

export default async function TerminalPage() {
  const [conn, servers] = await Promise.all([getGithubConnection(), getServers()]);
  // Running commands on a server is admin-only (the API enforces it too).
  if (!conn.connected || !conn.isAdmin) redirect("/");

  return (
    <TerminalConsole servers={servers} />
  );
}
