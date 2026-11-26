import { redirect } from "next/navigation";
import { Sidebar } from "@/components/sidebar";
import { getCurrentUser } from "@/lib/session";

// Everything behind sign-in shares this frame. The proxy already sends
// signed-out visitors to /login; this covers an expired session.
export default async function AppLayout({ children }: { children: React.ReactNode }) {
  const user = await getCurrentUser();
  if (!user) redirect("/login");
  return (
    <div className="min-h-screen">
      <Sidebar login={user.login} isAdmin={user.isAdmin} />
      <div className="flex min-h-screen flex-col lg:pl-60">{children}</div>
    </div>
  );
}
