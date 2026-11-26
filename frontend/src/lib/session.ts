import { backendFetch, sessionToken } from "@/lib/backend";

export { SESSION_COOKIE, OAUTH_STATE_COOKIE } from "@/lib/session-cookie";

export type SessionUser = {
  id: number;
  login: string;
  name: string;
  avatarUrl: string;
  isAdmin: boolean;
};

// The signed-in person, as the API sees them (admin status included), or null
// when there is no valid session.
export async function getCurrentUser(): Promise<SessionUser | null> {
  if (!(await sessionToken())) return null;
  try {
    const resp = await backendFetch("/api/auth/me", { cache: "no-store" });
    if (!resp.ok) return null;
    return (await resp.json()) as SessionUser;
  } catch {
    return null;
  }
}

export type GithubConnection =
  | { connected: true; login: string; isAdmin: boolean }
  | { connected: false };

// Kept for the pages that gate on "who is this": a valid session is the
// connection now; there is no separate GitHub connect step.
export async function getGithubConnection(): Promise<GithubConnection> {
  const user = await getCurrentUser();
  return user ? { connected: true, login: user.login, isAdmin: user.isAdmin } : { connected: false };
}
