import { redirect } from "next/navigation";
import { GithubMark, LogoMark } from "@/components/icons";
import { getCurrentUser } from "@/lib/session";

export const metadata = { title: "Sign in" };

const ERRORS: Record<string, string> = {
  no_access:
    "Your GitHub account isn't invited to any of this team's repositories that Mountabo manages. Ask an admin to add you to your project's repo on GitHub, then sign in again.",
  state: "The sign-in link expired or was opened twice. Start again.",
  exchange: "GitHub didn't accept the sign-in. Start again.",
  backend: "Couldn't reach the Mountabo API. Try again in a moment.",
  config: "Sign-in isn't configured on this install (GITHUB_CLIENT_ID is missing).",
};

export default async function LoginPage({
  searchParams,
}: {
  searchParams: Promise<{ error?: string; next?: string }>;
}) {
  const { error, next } = await searchParams;
  if (!error && (await getCurrentUser())) redirect(next && next.startsWith("/") && !next.startsWith("//") ? next : "/");
  const href = `/api/auth/login${next ? `?next=${encodeURIComponent(next)}` : ""}`;

  return (
    <main className="flex min-h-screen flex-col items-center justify-center px-4">
      <div className="w-full max-w-[360px]">
        <div className="mb-8 flex items-center gap-2 text-cream">
          <LogoMark className="text-lime" width={26} height={26} />
          <span className="text-[17px] font-semibold tracking-tight">Mountabo</span>
        </div>
        <h1 className="text-[22px] font-semibold tracking-tight text-cream">Sign in</h1>
        <p className="mt-1.5 text-[13px] leading-relaxed text-muted">
          Use your GitHub account. You&apos;ll see the repositories you already have on GitHub and deploy them to the
          environments your team has set up.
        </p>
        {error && (
          <p role="alert" className="mt-5 rounded-md border border-err/40 bg-err/10 px-3 py-2 text-[13px] text-err">
            {ERRORS[error] ?? "Sign-in failed. Start again."}
          </p>
        )}
        <a
          href={href}
          className="mt-6 flex w-full items-center justify-center gap-2 rounded-md bg-cream px-4 py-2.5 text-[13px] font-medium text-bg transition-opacity hover:opacity-90"
        >
          <GithubMark />
          Continue with GitHub
        </a>
        <p className="mt-6 text-[12px] leading-relaxed text-faint">
          Access follows GitHub: push access to a repo lets you deploy it, read access lets you view it.
        </p>
      </div>
    </main>
  );
}
