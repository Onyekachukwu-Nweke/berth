import { redirect } from "next/navigation";
import { Page, PageHeader } from "@/components/page-header";
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
export const metadata = { title: "Access" };

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
    <Page>
      <PageHeader
        title="Access"
        description="Clients, which servers they own, which repos belong to them, and where each environment deploys."
      />
      <AdminView initial={{ policy, servers, users, installations, installUrl: config.installUrl }} />
    </Page>
  );
}
