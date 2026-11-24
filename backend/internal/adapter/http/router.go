package http

import (
	nethttp "net/http"

	"github.com/goodylili/mountabo/internal/usecase"
)

// NewRouter maps every API route to its handler behind an explicit rule. There
// is no default: a route registered here without a guard is public, and the
// only public routes are health, sign-in config and sign-in itself.
//
// Rules:
//
//	g.User          any signed-in person
//	g.Repo(read)    can see the repo on GitHub (owner/repo from path or query)
//	g.Repo(write)   can push to the repo on GitHub
//	g.Server        the server targets or runs one of the person's repos
//	g.Deploy        write access + client isolation + the environment's rule
//	g.Admin         admins (MOUNTABO_ADMINS)
func NewRouter(g *Guard, au *AuthHandler, ad *AdminHandler, gh *GitHubHandler, sv *ServersHandler, dep *DeployHandler, mon *MonitorHandler, sec *SecretsHandler, rel *ReleaseHandler, term *TerminalHandler) *nethttp.ServeMux {
	read, write := usecase.PermRead, usecase.PermWrite
	mux := nethttp.NewServeMux()

	// Public.
	mux.HandleFunc("GET /healthz", func(w nethttp.ResponseWriter, _ *nethttp.Request) { w.WriteHeader(nethttp.StatusNoContent) })
	mux.HandleFunc("GET /api/auth/config", au.Config)
	mux.HandleFunc("POST /api/auth/github", au.Login)

	// Session.
	mux.HandleFunc("GET /api/auth/me", g.User(au.Me))
	mux.HandleFunc("POST /api/auth/logout", g.User(au.Logout))

	// Repos, read through the App. The list is the person's own; everything
	// else is scoped to one repo they can see.
	mux.HandleFunc("GET /api/github/repos", g.User(gh.Repos))
	mux.HandleFunc("GET /api/github/ports", g.Repo(read, gh.Ports))
	mux.HandleFunc("GET /api/github/tree", g.Repo(read, gh.Tree))
	mux.HandleFunc("GET /api/github/env-example", g.Repo(read, gh.EnvExample))
	mux.HandleFunc("GET /api/github/run-steps", g.Repo(read, gh.RunSteps))
	mux.HandleFunc("GET /api/github/job-logs", g.Repo(read, gh.JobLogs))
	mux.HandleFunc("GET /api/repos/{owner}/{repo}/branches", g.Repo(read, gh.Branches))
	mux.HandleFunc("GET /api/repos/{owner}/{repo}/targets", g.Repo(read, au.Targets))
	mux.HandleFunc("POST /api/repos/{owner}/{repo}/files", g.Repo(usecase.PermWrite, gh.CommitFiles))

	// Servers. Developers see the servers their repos deploy to; everything
	// that changes a server, or runs commands on it, is admin-only.
	mux.HandleFunc("GET /api/servers", g.User(sv.List))
	mux.HandleFunc("GET /api/servers/options", g.User(sv.Options))
	mux.HandleFunc("GET /api/servers/{id}/ports", g.Server(sv.Ports))
	mux.HandleFunc("GET /api/servers/{id}/metrics", g.Server(sv.Metrics))
	mux.HandleFunc("POST /api/servers", g.Admin(sv.Add))
	mux.HandleFunc("GET /api/servers/{id}/setup", g.Admin(sv.Setup))
	mux.HandleFunc("GET /api/servers/{id}/options", g.Admin(sv.ApplyOptions))
	// A shared server's logs mix several clients' apps, so they stay admin-only.
	mux.HandleFunc("GET /api/servers/{id}/logs", g.Admin(sv.Logs))
	mux.HandleFunc("POST /api/servers/{id}/dashboard/{tool}/open", g.Admin(sv.OpenDashboard))
	mux.HandleFunc("GET /api/servers/{id}/dashboard/uptime-kuma/admin", g.Admin(sv.UptimeKumaAdmin))
	mux.HandleFunc("POST /api/servers/{id}/dashboard/uptime-kuma/admin/reset", g.Admin(sv.ResetUptimeKumaAdmin))
	mux.HandleFunc("GET /api/servers/domains/preview", g.Admin(sv.DomainsPreview))
	mux.HandleFunc("GET /api/servers/{id}/domains/add", g.Admin(sv.AddDomain))
	mux.HandleFunc("GET /api/servers/{id}/domains/remove", g.Admin(sv.RemoveDomain))
	mux.HandleFunc("GET /api/servers/{id}/proxy", g.Admin(sv.SetProxy))
	mux.HandleFunc("POST /api/servers/{id}/exec", g.Admin(term.Exec))
	mux.HandleFunc("POST /api/ai/command", g.Admin(term.AICommand))
	mux.HandleFunc("DELETE /api/servers/{id}", g.Admin(sv.Delete))

	// Deploying.
	mux.HandleFunc("POST /api/deploy/preview", g.User(dep.Preview))
	mux.HandleFunc("POST /api/servers/{id}/deploy", g.Deploy(dep.Deploy))

	// Deployments: the list is filtered to the person's repos; reading one
	// needs read access, changing one needs write access.
	mux.HandleFunc("GET /api/deployments", g.User(mon.Deployments))
	mux.HandleFunc("GET /api/deployments/{owner}/{repo}/{environment}/health", g.Repo(read, mon.Health))
	mux.HandleFunc("GET /api/deployments/{owner}/{repo}/{environment}/release", g.Repo(read, rel.State))
	mux.HandleFunc("GET /api/deployments/{owner}/{repo}/{environment}/secrets", g.Repo(read, sec.List))
	mux.HandleFunc("PUT /api/deployments/{owner}/{repo}/{environment}/secrets", g.Repo(write, sec.Set))
	mux.HandleFunc("DELETE /api/deployments/{owner}/{repo}/{environment}/secrets/{name}", g.Repo(write, sec.Delete))
	mux.HandleFunc("PUT /api/deployments/{owner}/{repo}/{environment}/port", g.Repo(write, mon.SetPort))
	mux.HandleFunc("DELETE /api/deployments/{owner}/{repo}/{environment}/lock", g.Repo(write, mon.ClearLock))
	// Release operations. The mutation is a POST that returns an operation id; the
	// log is a separate GET stream. Driving the mutation from the stream itself
	// would let an EventSource reconnect — which browsers do automatically on any
	// dropped connection — silently re-run the rollback.
	mux.HandleFunc("POST /api/deployments/{owner}/{repo}/{environment}/rollback", g.Repo(write, rel.Rollback))
	mux.HandleFunc("POST /api/deployments/{owner}/{repo}/{environment}/redeploy", g.Repo(write, rel.Redeploy))
	mux.HandleFunc("POST /api/deployments/{owner}/{repo}/{environment}/approve", g.Repo(write, rel.Approve))
	mux.HandleFunc("GET /api/operations/{id}/logs", g.User(rel.Logs))
	mux.HandleFunc("DELETE /api/deployments/{owner}/{repo}/{environment}", g.Repo(write, mon.Delete))

	// Admin: placement policy, people, and where the App is installed.
	mux.HandleFunc("GET /api/admin/policy", g.Admin(ad.Policy))
	mux.HandleFunc("PUT /api/admin/clients/{id}", g.Admin(ad.SaveClient))
	mux.HandleFunc("DELETE /api/admin/clients/{id}", g.Admin(ad.DeleteClient))
	mux.HandleFunc("PUT /api/admin/clients/{id}/environments/{environment}", g.Admin(ad.SaveEnvRule))
	mux.HandleFunc("DELETE /api/admin/clients/{id}/environments/{environment}", g.Admin(ad.DeleteEnvRule))
	mux.HandleFunc("PUT /api/admin/links", g.Admin(ad.SaveLink))
	mux.HandleFunc("DELETE /api/admin/links/{owner}/{repo}", g.Admin(ad.DeleteLink))
	mux.HandleFunc("PUT /api/admin/repo-rules", g.Admin(ad.SaveRepoRule))
	mux.HandleFunc("DELETE /api/admin/repo-rules/{owner}/{repo}/{environment}", g.Admin(ad.DeleteRepoRule))
	mux.HandleFunc("PUT /api/admin/servers/{id}/client", g.Admin(ad.SetServerClient))
	mux.HandleFunc("GET /api/admin/users", g.Admin(ad.Users))
	mux.HandleFunc("GET /api/admin/installations", g.Admin(ad.Installations))
	return mux
}
