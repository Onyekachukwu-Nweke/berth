// A quiet monogram tile for a server or app: the first letter on a neutral
// square. Identity comes from the name next to it, not from color.
export function ServerAvatar({ seed, size = "md" }: { seed: string; size?: "md" | "lg" }) {
  const dim = size === "lg" ? "h-9 w-9 text-[14px]" : "h-8 w-8 text-[13px]";
  return (
    <span
      aria-hidden
      className={`${dim} flex shrink-0 items-center justify-center rounded-md border border-line bg-surface-2 font-semibold uppercase text-cream`}
    >
      {(seed.trim()[0] ?? "?").toUpperCase()}
    </span>
  );
}
