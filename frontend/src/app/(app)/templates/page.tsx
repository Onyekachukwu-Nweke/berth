import { Page, PageHeader } from "@/components/page-header";
import { TemplatesView } from "@/components/templates-view";
import { getCurrentUser } from "@/lib/session";

export const metadata = { title: "Templates" };

export default async function TemplatesPage({
  searchParams,
}: {
  searchParams: Promise<{ repo?: string; branch?: string; template?: string }>;
}) {
  const [{ repo, branch, template }, user] = await Promise.all([searchParams, getCurrentUser()]);
  return (
    <Page>
      <PageHeader
        title="Templates"
        description="Dockerfile and compose starting points that work with Mountabo deploys. Edit them here, give them to your AI, or commit them to a repo."
      />
      <TemplatesView account={user?.login ?? ""} initialRepo={repo} initialBranch={branch} initialTemplate={template} />
    </Page>
  );
}
