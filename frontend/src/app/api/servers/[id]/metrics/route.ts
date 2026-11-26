import { NextResponse } from "next/server";
import { backendFetch, backendURL } from "@/lib/backend";

export const dynamic = "force-dynamic";

// Proxies live host metrics to the Go backend, which reads cpu/mem/disk/uptime
// from the server over SSH. The monitor calls this on demand (no daemon).
export async function GET(_request: Request, { params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  let resp: Response;
  try {
    resp = await backendFetch(`${backendURL()}/api/servers/${id}/metrics`, {
      cache: "no-store",
      signal: AbortSignal.timeout(25_000),
    });
  } catch {
    return NextResponse.json({ error: "couldn't reach the berth backend" }, { status: 502 });
  }
  const body = await resp.text();
  return new NextResponse(body, {
    status: resp.status,
    headers: { "content-type": "application/json" },
  });
}
