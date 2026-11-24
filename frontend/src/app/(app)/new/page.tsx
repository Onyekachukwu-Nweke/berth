import { NewDeployment } from "@/components/new-deployment";
import { Page, PageHeader } from "@/components/page-header";
import { getServers } from "@/lib/servers";
import { getCurrentUser } from "@/lib/session";

export const metadata = { title: "New deployment" };

export default async function NewDeploymentPage() {
  const [user, servers] = await Promise.all([getCurrentUser(), getServers()]);
  return (
    <Page>
      <PageHeader
        title="New deployment"
        description="Pick a repository you can push to, then where it runs. Mountabo writes the workflow, sets the secrets and prepares the server."
      />
      <NewDeployment servers={servers} account={user?.login ?? null} mode="repos" isAdmin={Boolean(user?.isAdmin)} />
    </Page>
  );
}
