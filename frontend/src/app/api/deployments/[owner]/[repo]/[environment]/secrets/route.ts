import { NextResponse } from "next/server";
import { backendURL } from "@/lib/servers";

export const dynamic = "force-dynamic";

// Cap the secret-write body so a runaway or hostile payload is rejected here
// rather than streamed on to the backend. 64 KiB mirrors the backend's own cap.
const MAX_BODY_BYTES = 64 * 1024;

// Secrets are a sensitive read/write surface: every response here is no-store,
// and this proxy never logs request or response bodies (which carry values).
function secretsURL(owner: string, repo: string, environment: string): string {
  return `${backendURL()}/api/deployments/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/${encodeURIComponent(environment)}/secrets`;
}

// readBounded reads the request body without ever buffering more than maxBytes,
// counting real bytes (not characters). It rejects early on a declared oversized
// Content-Length, then reads the stream chunk by chunk and aborts the moment the
// running total exceeds the cap — so an oversized or chunked body never fills
// this route's memory, protecting the proxy itself and not just the backend.
async function readBounded(
  request: Request,
  maxBytes: number,
): Promise<{ body: string; tooLarge: boolean }> {
  const declared = Number(request.headers.get("content-length"));
  if (Number.isFinite(declared) && declared > maxBytes) {
    return { body: "", tooLarge: true };
  }
  const reader = request.body?.getReader();
  if (!reader) return { body: "", tooLarge: false };
  const chunks: Uint8Array[] = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.byteLength;
    if (total > maxBytes) {
      await reader.cancel();
      return { body: "", tooLarge: true };
    }
    chunks.push(value);
  }
  const merged = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    merged.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return { body: new TextDecoder().decode(merged), tooLarge: false };
}

// GET proxies the environment's configured-secrets listing (names + updated
// times only — GitHub Actions secret values are write-only).
export async function GET(
  _request: Request,
  { params }: { params: Promise<{ owner: string; repo: string; environment: string }> },
) {
  const { owner, repo, environment } = await params;
  let resp: Response;
  try {
    resp = await fetch(secretsURL(owner, repo, environment), {
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

// PUT proxies a secret write (straight to GitHub Actions; write-only).
export async function PUT(
  request: Request,
  { params }: { params: Promise<{ owner: string; repo: string; environment: string }> },
) {
  const { owner, repo, environment } = await params;
  const { body: requestBody, tooLarge } = await readBounded(request, MAX_BODY_BYTES);
  if (tooLarge) {
    return NextResponse.json({ error: "request body too large" }, { status: 413 });
  }
  let resp: Response;
  try {
    resp = await fetch(secretsURL(owner, repo, environment), {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: requestBody,
    });
  } catch {
    return NextResponse.json({ error: "couldn't reach the mountabo backend" }, { status: 502 });
  }
  const responseBody = await resp.text();
  return new NextResponse(responseBody, {
    status: resp.status,
    headers: { "content-type": "application/json", "cache-control": "no-store" },
  });
}
