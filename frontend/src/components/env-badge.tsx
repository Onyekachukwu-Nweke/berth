import { envRank } from "@/lib/status";

// Production stands out; everything else is quiet.
export function EnvBadge({ env }: { env: string }) {
  const prod = envRank(env) === 0;
  return (
    <span
      className={`inline-flex items-center rounded px-1.5 py-0.5 text-[11px] font-medium ${
        prod ? "bg-blue/15 text-blue" : "bg-surface-hover text-body"
      }`}
    >
      {env || "default"}
    </span>
  );
}
