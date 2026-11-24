import { NextResponse, type NextRequest } from "next/server";
import { SESSION_COOKIE } from "@/lib/session-cookie";

// Routes reachable without a session: the sign-in page and the sign-in flow.
const PUBLIC = ["/login", "/api/auth/login", "/api/auth/callback"];

export function proxy(request: NextRequest) {
  const { pathname, search } = request.nextUrl;
  const isApi = pathname.startsWith("/api/");
  const isPublic = PUBLIC.some((p) => pathname === p || pathname.startsWith(`${p}/`));

  // CSRF: the API is only ever called by this app's own pages. Several routes
  // act on a GET (streamed setup, domain changes), so a cross-site request is
  // refused outright rather than relying on SameSite alone. The OAuth callback
  // is the one legitimate cross-site arrival (GitHub redirects back to it).
  if (isApi && !isPublic && request.headers.get("sec-fetch-site") === "cross-site") {
    return NextResponse.json({ error: "cross-site request refused" }, { status: 403 });
  }

  if (isPublic || request.cookies.get(SESSION_COOKIE)) {
    return NextResponse.next();
  }
  if (isApi) {
    return NextResponse.json({ error: "sign in required" }, { status: 401 });
  }
  const login = new URL("/login", request.url);
  if (pathname !== "/") login.searchParams.set("next", pathname + search);
  return NextResponse.redirect(login);
}

export const config = {
  // Everything except Next's own assets and static files.
  matcher: ["/((?!_next/static|_next/image|favicon.ico|icon.svg|.*\\.(?:png|svg|jpg|jpeg|webp|ico)$).*)"],
};
