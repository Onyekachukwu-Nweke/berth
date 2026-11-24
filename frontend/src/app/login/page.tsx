import { redirect } from "next/navigation";
import { GithubMark, LogoMark } from "@/components/icons";
import { getCurrentUser } from "@/lib/session";

const ERRORS: Record<string, string> = {
  no_access:
    "your GitHub account can't reach any repository mountabo is installed on. ask an admin to add you to the project's repo on GitHub, then sign in again.",
  state: "the sign-in link expired or was opened twice. start again.",
  exchange: "GitHub didn't accept the sign-in. start again.",
  backend: "couldn't reach the mountabo API. try again in a moment.",
  config: "sign-in isn't configured on this install (GITHUB_CLIENT_ID is missing).",
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
    <main className="flex min-h-screen items-center justify-center px-4">
      <div className="w-full max-w-[420px] rounded-xl border border-line bg-surface p-8">
        <div className="mb-6 flex items-center gap-2 text-cream">
          <LogoMark className="text-lime" width={30} height={30} />
          <span className="text-lg font-bold tracking-tight">mountabo</span>
        </div>
        <h1 className="mb-2 text-[15px] font-bold text-cream">sign in to deploy</h1>
        <p className="mb-6 text-[13px] leading-relaxed text-muted">
          sign in with GitHub. you&apos;ll see the repositories you already have access to on GitHub, and deploy
          them to the environments your team has set up.
        </p>
        {error && (
          <p role="alert" className="mb-5 rounded-md border border-red-400/40 bg-red-400/10 px-3 py-2 text-[13px] text-red-300">
            {ERRORS[error] ?? "sign-in failed. start again."}
          </p>
        )}
        <a
          href={href}
          className="flex w-full items-center justify-center gap-2 rounded-md bg-lime px-4 py-2.5 text-[13px] font-bold text-bg transition-opacity hover:opacity-90"
        >
          <GithubMark />
          sign in with GitHub
        </a>
      </div>
    </main>
  );
}
