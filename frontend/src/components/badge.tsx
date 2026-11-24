// Tones carry meaning: ok (ready/succeeded), warn (in progress), err (failed),
// info (worth noticing), neutral (everything else).
export type Tone = "ok" | "warn" | "err" | "info" | "neutral";

const toneClass: Record<Tone, string> = {
  ok: "bg-ok/10 text-ok",
  warn: "bg-warn/10 text-warn",
  err: "bg-err/10 text-err",
  info: "bg-blue/10 text-blue",
  neutral: "bg-surface-hover text-muted",
};

const dotClass: Record<Tone, string> = {
  ok: "bg-ok",
  warn: "bg-warn",
  err: "bg-err",
  info: "bg-blue",
  neutral: "bg-faint",
};

export function Badge({
  children,
  tone = "neutral",
  dot = false,
}: {
  children: React.ReactNode;
  tone?: Tone;
  dot?: boolean;
}) {
  return (
    <span className={`inline-flex items-center gap-1.5 rounded px-1.5 py-0.5 text-[11px] font-medium ${toneClass[tone]}`}>
      {dot && <span className={`h-1.5 w-1.5 rounded-full ${dotClass[tone]}`} />}
      {children}
    </span>
  );
}
