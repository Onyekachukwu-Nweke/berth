"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { Docker, Grid, Layers, LogoMark, Logout, Plus, ServerIcon, Terminal, Users } from "@/components/icons";
import { ThemeToggle } from "@/components/theme-toggle";

type NavItem = { href: string; label: string; icon: React.ComponentType<{ className?: string }> };

const MAIN: NavItem[] = [
  { href: "/", label: "Overview", icon: Grid },
  { href: "/deployments", label: "Deployments", icon: Layers },
  { href: "/servers", label: "Servers", icon: ServerIcon },
  { href: "/templates", label: "Templates", icon: Docker },
];

const ADMIN: NavItem[] = [
  { href: "/admin", label: "Access", icon: Users },
  { href: "/terminal", label: "Terminal", icon: Terminal },
];

function isActive(pathname: string, href: string) {
  return href === "/" ? pathname === "/" : pathname === href || pathname.startsWith(href + "/");
}

function NavLink({ item, pathname, compact }: { item: NavItem; pathname: string; compact?: boolean }) {
  const active = isActive(pathname, item.href);
  const Icon = item.icon;
  return (
    <Link
      href={item.href}
      aria-current={active ? "page" : undefined}
      className={`flex shrink-0 items-center gap-2.5 rounded-md px-2.5 py-1.5 text-[13px] font-medium transition-colors ${
        active ? "bg-surface-hover text-cream" : "text-muted hover:bg-surface-2 hover:text-cream"
      } ${compact ? "" : "w-full"}`}
    >
      <Icon className={active ? "text-cream" : "text-faint"} />
      {item.label}
    </Link>
  );
}

// The app's frame: a fixed sidebar on desktop, a top bar with scrolling nav on
// small screens. Admin links only render for admins; the API enforces it anyway.
export function Sidebar({ login, isAdmin }: { login: string; isAdmin: boolean }) {
  const pathname = usePathname() ?? "/";
  const items = isAdmin ? [...MAIN, ...ADMIN] : MAIN;

  return (
    <>
      {/* desktop */}
      <aside className="fixed inset-y-0 left-0 z-30 hidden w-60 flex-col border-r border-line bg-surface lg:flex">
        <Link href="/" className="flex h-14 items-center gap-2 border-b border-line px-4 text-cream">
          <LogoMark className="text-lime" width={22} height={22} />
          <span className="text-[15px] font-semibold tracking-tight">Mountabo</span>
        </Link>

        <div className="px-3 pt-4">
          <Link
            href="/new"
            className="flex w-full items-center justify-center gap-2 rounded-md bg-cream px-3 py-2 text-[13px] font-medium text-bg transition-opacity hover:opacity-90"
          >
            <Plus />{" "}New deployment
          </Link>
        </div>

        <nav className="flex flex-col gap-0.5 px-3 pt-4">
          {MAIN.map((item) => (
            <NavLink key={item.href} item={item} pathname={pathname} />
          ))}
        </nav>

        {isAdmin && (
          <nav className="flex flex-col gap-0.5 px-3 pt-6">
            <span className="px-2.5 pb-1.5 text-[11px] font-medium uppercase tracking-wider text-faint">Admin</span>
            {ADMIN.map((item) => (
              <NavLink key={item.href} item={item} pathname={pathname} />
            ))}
          </nav>
        )}

        <div className="mt-auto flex items-center gap-2.5 border-t border-line px-4 py-3">
          {/* eslint-disable-next-line @next/next/no-img-element */}
          <img
            src={`https://github.com/${encodeURIComponent(login)}.png?size=64`}
            alt=""
            width={28}
            height={28}
            className="h-7 w-7 shrink-0 rounded-full border border-line bg-surface-2"
          />
          <div className="min-w-0 flex-1 leading-tight">
            <div className="truncate text-[13px] font-medium text-cream">{login}</div>
            <div className="text-[11px] text-muted">{isAdmin ? "Admin" : "Developer"}</div>
          </div>
          <span className="text-muted">
            <ThemeToggle />
          </span>
          <form action="/api/auth/logout" method="post" className="flex">
            <button type="submit" className="text-muted transition-colors hover:text-cream" aria-label="Sign out" title="Sign out">
              <Logout />
            </button>
          </form>
        </div>
      </aside>

      {/* mobile */}
      <header className="sticky top-0 z-30 border-b border-line bg-surface lg:hidden">
        <div className="flex h-12 items-center gap-3 px-4">
          <Link href="/" className="flex items-center gap-2 text-cream">
            <LogoMark className="text-lime" width={20} height={20} />
            <span className="text-[14px] font-semibold">Mountabo</span>
          </Link>
          <div className="ml-auto flex items-center gap-3 text-muted">
            <Link href="/new" className="flex items-center gap-1.5 rounded-md bg-cream px-2.5 py-1 text-[12px] font-medium text-bg">
              <Plus />{" "}New
            </Link>
            <ThemeToggle />
            <form action="/api/auth/logout" method="post" className="flex">
              <button type="submit" aria-label="Sign out">
                <Logout />
              </button>
            </form>
          </div>
        </div>
        <nav className="flex gap-1 overflow-x-auto px-3 pb-2">
          {items.map((item) => (
            <NavLink key={item.href} item={item} pathname={pathname} compact />
          ))}
        </nav>
      </header>
    </>
  );
}
