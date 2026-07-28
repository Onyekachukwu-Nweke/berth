import { NextResponse } from "next/server";
import { backendURL } from "@/lib/servers";

export const dynamic = "force-dynamic";

// Secrets are a sensitive read/write surface: every response here is no-store,
// and this proxy never logs request or response bodies (which carry values).
function secretsURL(owner: string, repo: string, environment: string): string {
  return `${backendURL()}/api/deployments/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/${encodeURIComponent(environment)}/secrets`;
}

// GET proxies the environment's configured-secrets listing. It forwards the
// includeValues flag so the compare view (names only) never pulls values.
export async function GET(
  request: Request,
  { params }: { params: Promise<{ owner: string; repo: string; environment: string }> },
) {
  const { owner, repo, environment } = await params;
  const includeValues = new URL(request.url).searchParams.get("includeValues") === "true";
  let resp: Response;
  try {
    resp = await fetch(`${secretsURL(owner, repo, environment)}${includeValues ? "?includeValues=true" : ""}`, {
      cache: "no-store",
      signal: AbortSignal.timeout(25_000),
    });
  } catch {
    return NextResponse.json({ error: "couldn't reach the mountabo backend" }, { status: 502 });
  }
  const body = await resp.text();
  return new NextResponse(body, {
    status: resp.status,
    headers: { "content-type": "application/json", "cache-control": "no-store" },
  });
}

// PUT proxies a secret write (GitHub + local encrypted store).
export async function PUT(
  request: Request,
  { params }: { params: Promise<{ owner: string; repo: string; environment: string }> },
) {
  const { owner, repo, environment } = await params;
  let resp: Response;
  try {
    resp = await fetch(secretsURL(owner, repo, environment), {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: await request.text(),
    });
  } catch {
    return NextResponse.json({ error: "couldn't reach the mountabo backend" }, { status: 502 });
  }
  const body = await resp.text();
  return new NextResponse(body, {
    status: resp.status,
    headers: { "content-type": "application/json", "cache-control": "no-store" },
  });
}
