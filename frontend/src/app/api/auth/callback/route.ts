import { NextResponse } from "next/server";
import { cookies } from "next/headers";
import { backendURL } from "@/lib/backend";
import { publicOrigin } from "@/lib/public-url";
import { OAUTH_STATE_COOKIE, SESSION_COOKIE } from "@/lib/session-cookie";

// GitHub redirects here after sign-in. The state is checked against the cookie
// set when sign-in started, the API trades the code for a session, and the
// session token goes into an httpOnly cookie the browser's JavaScript never sees.
export async function GET(request: Request) {
  const url = new URL(request.url);
  const origin = publicOrigin(request);
  const jar = await cookies();
  const raw = jar.get(OAUTH_STATE_COOKIE)?.value;
  let expected: { state: string; next: string } | null = null;
  try {
    expected = raw ? JSON.parse(raw) : null;
  } catch {
    expected = null;
  }
  const code = url.searchParams.get("code");
  if (!expected || !code || url.searchParams.get("state") !== expected.state) {
    return NextResponse.redirect(`${origin}/login?error=state`);
  }

  let resp: Response;
  try {
    resp = await fetch(`${backendURL()}/api/auth/github`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ code, redirectUri: `${origin}/api/auth/callback` }),
    });
  } catch {
    return NextResponse.redirect(`${origin}/login?error=backend`);
  }
  if (resp.status === 403) {
    const body = (await resp.json().catch(() => ({}))) as { error?: string };
    const reason = body.error === "removed" ? "removed" : "no_access";
    return NextResponse.redirect(`${origin}/login?error=${reason}`);
  }
  if (!resp.ok) {
    return NextResponse.redirect(`${origin}/login?error=exchange`);
  }
  const session = (await resp.json()) as { token: string; expiresAt: string };

  const res = NextResponse.redirect(`${origin}${expected.next || "/"}`);
  res.cookies.delete(OAUTH_STATE_COOKIE);
  res.cookies.set(SESSION_COOKIE, session.token, {
    httpOnly: true,
    sameSite: "lax",
    secure: origin.startsWith("https://"),
    path: "/",
    expires: new Date(session.expiresAt),
  });
  return res;
}
