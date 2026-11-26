import { NewDeployment } from "@/components/new-deployment";
import { Page, PageHeader } from "@/components/page-header";
import { getServers } from "@/lib/servers";
import { getCurrentUser } from "@/lib/session";

export const metadata = { title: "Servers" };

export default async function ServersPage() {
  const [user, servers] = await Promise.all([getCurrentUser(), getServers()]);
  const isAdmin = Boolean(user?.isAdmin);
  return (
    <Page>
      <PageHeader
        title="Servers"
        description={
          isAdmin
            ? "Add, set up and harden servers, and manage their domains. Assign them to clients under Access."
            : "The servers your projects deploy to."
        }
      />
      <NewDeployment servers={servers} account={user?.login ?? null} mode="servers" isAdmin={isAdmin} />
    </Page>
  );
}
