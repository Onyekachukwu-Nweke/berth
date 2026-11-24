import { redirect } from "next/navigation";
import { Header } from "@/components/header";
import { AdminView, type AdminData } from "@/components/admin-view";
import { backendFetch } from "@/lib/backend";
import { getCurrentUser } from "@/lib/session";

async function load<T>(path: string, fallback: T): Promise<T> {
  try {
    const resp = await backendFetch(path, { cache: "no-store" });
    return resp.ok ? ((await resp.json()) as T) : fallback;
  } catch {
    return fallback;
  }
}

// Access settings: which client owns which servers and repos, and where each
// client's environments deploy. Admins only (the API enforces it as well).
export default async function AdminPage() {
  const user = await getCurrentUser();
  if (!user?.isAdmin) redirect("/");

  const [policy, servers, users, installations, config] = await Promise.all([
    load<AdminData["policy"]>("/api/admin/policy", { clients: [], links: [], envRules: [], repoRules: [] }),
    load<AdminData["servers"]>("/api/servers", []),
    load<AdminData["users"]>("/api/admin/users", []),
    load<AdminData["installations"] | null>("/api/admin/installations", null),
    load<{ installUrl: string }>("/api/auth/config", { installUrl: "" }),
  ]);

  return (
    <div className="flex min-h-screen flex-col">
      <Header crumbs={[{ label: "access" }]} account={user.login} container="max-w-[1100px]" />
      <AdminView initial={{ policy, servers, users, installations, installUrl: config.installUrl }} />
    </div>
  );
}
