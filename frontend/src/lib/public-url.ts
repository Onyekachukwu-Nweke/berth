// The origin users reach mountabo at. Behind a reverse proxy, or in a container,
// request.url carries the internal bind address (e.g. http://0.0.0.0:4321), which
// must never end up in an OAuth redirect_uri or a redirect Location. A hosted
// install sets MOUNTABO_PUBLIC_URL (e.g. https://deploy.example.com); local dev
// falls back to the request's own origin.
export function publicOrigin(request: Request): string {
  const configured = process.env.MOUNTABO_PUBLIC_URL?.trim();
  if (configured) return configured.replace(/\/+$/, "");
  return new URL(request.url).origin;
}
