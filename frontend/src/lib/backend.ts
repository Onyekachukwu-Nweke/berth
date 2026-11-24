import { cookies } from "next/headers";
import { SESSION_COOKIE } from "@/lib/session-cookie";

// The Go API is never exposed to the browser: the web app's server calls it on
// the person's behalf. backendFetch is the one way to do that. It forwards the
// signed-in person's session as a bearer token, so the API can decide what they
// may do. Server-side only (it reads the request's cookies).
export function backendURL(): string {
  return process.env.MOUNTABO_BACKEND || "http://localhost:7778";
}

export async function sessionToken(): Promise<string | undefined> {
  return (await cookies()).get(SESSION_COOKIE)?.value;
}

export async function backendFetch(input: string | URL, init: RequestInit = {}): Promise<Response> {
  const headers = new Headers(init.headers);
  const token = await sessionToken();
  if (token) headers.set("authorization", `Bearer ${token}`);
  const url = typeof input === "string" && input.startsWith("/") ? `${backendURL()}${input}` : input;
  return fetch(url, { ...init, headers });
}
