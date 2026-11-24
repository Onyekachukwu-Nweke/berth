import { backendFetch } from "@/lib/backend";

// Generic pass-through to the API for browser-side calls that have no
// dedicated route (admin, targets, branches, ...). Specific routes under
// /api take precedence over this one. The person's session is attached by
// backendFetch; the API decides what they may do.
//
// Sign-in endpoints are never proxied: the code exchange must only happen in
// the callback route, so a session token never reaches browser JavaScript.
const BLOCKED = new Set(["auth/github", "auth/logout"]);

async function forward(request: Request, ctx: { params: Promise<{ path: string[] }> }) {
  const { path } = await ctx.params;
  const joined = path.map(encodeURIComponent).join("/");
  if (BLOCKED.has(joined)) {
    return Response.json({ error: "not found" }, { status: 404 });
  }
  const search = new URL(request.url).search;
  const headers: Record<string, string> = {};
  const contentType = request.headers.get("content-type");
  if (contentType) headers["content-type"] = contentType;
  const accept = request.headers.get("accept");
  if (accept) headers.accept = accept;

  const hasBody = request.method !== "GET" && request.method !== "HEAD";
  let upstream: Response;
  try {
    upstream = await backendFetch(`/api/${joined}${search}`, {
      method: request.method,
      headers,
      body: hasBody ? await request.text() : undefined,
      cache: "no-store",
      signal: request.signal,
    });
  } catch {
    return Response.json({ error: "could not reach the mountabo API" }, { status: 502 });
  }
  const out = new Headers();
  const ct = upstream.headers.get("content-type");
  if (ct) out.set("content-type", ct);
  out.set("cache-control", "no-store");
  return new Response(upstream.body, { status: upstream.status, headers: out });
}

export const GET = forward;
export const POST = forward;
export const PUT = forward;
export const DELETE = forward;
