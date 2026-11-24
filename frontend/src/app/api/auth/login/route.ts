import { NextResponse } from "next/server";
import { publicOrigin } from "@/lib/public-url";
import { OAUTH_STATE_COOKIE } from "@/lib/session-cookie";

// Starts GitHub sign-in through the mountabo GitHub App. A GitHub App asks for
// no scopes: what it can touch is set by the App's permissions and where it is
// installed, and what a person can touch is their own access on GitHub.
export async function GET(request: Request) {
  const origin = publicOrigin(request);
  const clientId = process.env.GITHUB_CLIENT_ID;
  if (!clientId) {
    return NextResponse.redirect(`${origin}/login?error=config`);
  }
  const next = new URL(request.url).searchParams.get("next") ?? "/";
  const state = crypto.randomUUID();
  const url = new URL("https://github.com/login/oauth/authorize");
  url.searchParams.set("client_id", clientId);
  url.searchParams.set("redirect_uri", `${origin}/api/auth/callback`);
  url.searchParams.set("state", state);
  url.searchParams.set("allow_signup", "false");

  const res = NextResponse.redirect(url.toString());
  res.cookies.set(OAUTH_STATE_COOKIE, JSON.stringify({ state, next: safeNext(next) }), {
    httpOnly: true,
    sameSite: "lax",
    secure: origin.startsWith("https://"),
    path: "/",
    maxAge: 600,
  });
  return res;
}

// Only same-site paths are honored after sign-in (no open redirect).
function safeNext(next: string): string {
  return next.startsWith("/") && !next.startsWith("//") ? next : "/";
}
