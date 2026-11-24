import { backendFetch, backendURL } from "@/lib/backend";
export const dynamic = "force-dynamic";

// Proxies the live "switch reverse proxy" stream (?to=nginx|traefik) from the
// backend straight through to the browser's EventSource.
export async function GET(request: Request, { params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const search = new URL(request.url).search;

  const upstream = await backendFetch(`${backendURL()}/api/servers/${id}/proxy${search}`, {
    headers: { accept: "text/event-stream" },
  });

  return new Response(upstream.body, {
    status: upstream.status,
    headers: {
      "content-type": "text/event-stream",
      "cache-control": "no-cache, no-transform",
      connection: "keep-alive",
    },
  });
}
