import { NextResponse } from "next/server";
import { backendFetch } from "@/lib/backend";
import { publicOrigin } from "@/lib/public-url";
import { SESSION_COOKIE } from "@/lib/session-cookie";

// Ends the session on the API and clears the cookie. POST only, so a link on
// another site cannot sign someone out.
export async function POST(request: Request) {
  try {
    await backendFetch("/api/auth/logout", { method: "POST" });
  } catch {
    // The cookie is cleared regardless; an unreachable API must not trap anyone signed in.
  }
  const res = NextResponse.redirect(`${publicOrigin(request)}/login`, { status: 303 });
  res.cookies.delete(SESSION_COOKIE);
  return res;
}
