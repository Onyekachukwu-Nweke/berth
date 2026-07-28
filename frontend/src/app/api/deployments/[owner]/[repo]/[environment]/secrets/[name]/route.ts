import { NextResponse } from "next/server";
import { backendURL } from "@/lib/servers";

export const dynamic = "force-dynamic";

// Proxies "remove one operator secret" (GitHub + local value) to the Go backend.
// A managed secret name is rejected there with 400.
export async function DELETE(
  _request: Request,
  { params }: { params: Promise<{ owner: string; repo: string; environment: string; name: string }> },
) {
  const { owner, repo, environment, name } = await params;
  try {
    const resp = await fetch(
      `${backendURL()}/api/deployments/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/${encodeURIComponent(environment)}/secrets/${encodeURIComponent(name)}`,
      { method: "DELETE" },
    );
    return new NextResponse(null, { status: resp.status, headers: { "cache-control": "no-store" } });
  } catch {
    return NextResponse.json({ error: "couldn't reach the mountabo backend" }, { status: 502 });
  }
}
