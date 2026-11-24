package usecase

import (
	"testing"
)

// memPolicy is an in-memory PolicyStore.
type memPolicy struct{ p Policy }

func (m *memPolicy) Policy() (Policy, error)         { return m.p, nil }
func (m *memPolicy) SaveClient(c Client) error       { m.p.Clients = append(m.p.Clients, c); return nil }
func (m *memPolicy) DeleteClient(string) error       { return nil }
func (m *memPolicy) SaveLink(l RepoLink) error       { m.p.Links = append(m.p.Links, l); return nil }
func (m *memPolicy) DeleteLink(string, string) error { return nil }
func (m *memPolicy) SaveEnvRule(r EnvRule) error {
	m.p.EnvRules = append(m.p.EnvRules, r)
	return nil
}
func (m *memPolicy) DeleteEnvRule(string, string) error { return nil }
func (m *memPolicy) SaveRepoRule(r RepoRule) error {
	m.p.RepoRules = append(m.p.RepoRules, r)
	return nil
}
func (m *memPolicy) DeleteRepoRule(string, string, string) error { return nil }

type memRepoAccess map[int64][]RepoAccess

func (m memRepoAccess) RepoAccessFor(id int64) ([]RepoAccess, error) { return m[id], nil }

// The agency's shape: acme's dev goes to the agency's shared box, acme's prod
// to acme's own server; globex owns its own server too.
func agencyFixture(t *testing.T) (*AccessService, *memServerStore, *fakeDeploymentStore) {
	t.Helper()
	servers := newMemServerStore()
	_ = servers.Save(Server{ID: "shared-dev", Name: "agency-dev-1"})
	_ = servers.Save(Server{ID: "acme-prod", Name: "acme-prod", ClientID: "acme"})
	_ = servers.Save(Server{ID: "globex-prod", Name: "globex-prod", ClientID: "globex"})
	policy := &memPolicy{p: Policy{
		Clients: []Client{{ID: "agency", Name: "Agency"}, {ID: "acme", Name: "Acme"}, {ID: "globex", Name: "Globex"}},
		Links: []RepoLink{
			{Owner: "acme-org", Repo: "*", ClientID: "acme"},
			{Owner: "agency", Repo: "acme-site", ClientID: "acme"},
			{Owner: "agency", Repo: "globex-api", ClientID: "globex"},
		},
		EnvRules: []EnvRule{
			{ClientID: "acme", Environment: "development", ServerID: "shared-dev"},
			{ClientID: "acme", Environment: "production", ServerID: "acme-prod"},
			{ClientID: "globex", Environment: "production", ServerID: "globex-prod"},
		},
	}}
	access := memRepoAccess{
		1: {{Owner: "agency", Repo: "acme-site", Permission: PermWrite}, {Owner: "acme-org", Repo: "shop", Permission: PermRead}},
	}
	deps := &fakeDeploymentStore{}
	return NewAccessService(policy, servers, access, deps), servers, deps
}

var dev = User{ID: 1, Login: "dev"}
var admin = User{ID: 99, Login: "boss", IsAdmin: true}

func TestPolicy_ClientResolution(t *testing.T) {
	a, _, _ := agencyFixture(t)
	p, _ := a.Policy()
	cases := map[[2]string]string{
		{"acme-org", "shop"}:      "acme",   // owner-wide link
		{"ACME-ORG", "Shop"}:      "acme",   // GitHub names are case-insensitive
		{"agency", "acme-site"}:   "acme",   // exact link
		{"agency", "globex-api"}:  "globex", // exact link in the agency's own org
		{"agency", "internal-ui"}: "agency", // unlinked: the agency's own project
	}
	for in, want := range cases {
		if got := p.ClientFor(in[0], in[1]); got != want {
			t.Errorf("ClientFor(%s/%s) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func TestCheckDeploy_DeveloperFollowsTheEnvironmentRule(t *testing.T) {
	a, _, _ := agencyFixture(t)
	if err := a.CheckDeploy(dev, "agency", "acme-site", "development", "shared-dev"); err != nil {
		t.Errorf("dev -> shared dev box should be allowed: %v", err)
	}
	if err := a.CheckDeploy(dev, "agency", "acme-site", "production", "acme-prod"); err != nil {
		t.Errorf("prod -> acme's own server should be allowed: %v", err)
	}
	// Right client, wrong environment's server.
	if err := a.CheckDeploy(dev, "agency", "acme-site", "development", "acme-prod"); !IsForbidden(err) {
		t.Errorf("dev env onto the prod server must be refused, got %v", err)
	}
	// No rule for this environment.
	if err := a.CheckDeploy(dev, "agency", "acme-site", "staging", "shared-dev"); !IsForbidden(err) {
		t.Errorf("unconfigured environment must be refused, got %v", err)
	}
}

func TestCheckDeploy_NeedsWriteAccess(t *testing.T) {
	a, _, _ := agencyFixture(t)
	// Read-only on acme-org/shop.
	if err := a.CheckDeploy(dev, "acme-org", "shop", "development", "shared-dev"); !IsForbidden(err) {
		t.Errorf("read-only collaborator must not deploy, got %v", err)
	}
	// No access at all.
	if err := a.CheckDeploy(dev, "agency", "globex-api", "production", "globex-prod"); !IsForbidden(err) {
		t.Errorf("no repo access must not deploy, got %v", err)
	}
}

// A client's server runs only that client's code, whoever asks.
func TestCheckDeploy_ClientIsolationBindsAdminsToo(t *testing.T) {
	a, _, _ := agencyFixture(t)
	if err := a.CheckDeploy(admin, "agency", "acme-site", "production", "globex-prod"); !IsForbidden(err) {
		t.Errorf("acme code onto globex's server must be refused even for an admin, got %v", err)
	}
	if err := a.CheckDeploy(admin, "agency", "internal-ui", "production", "acme-prod"); !IsForbidden(err) {
		t.Errorf("agency code onto acme's server must be refused, got %v", err)
	}
	// Admins may choose any server the isolation rule allows, rule or not.
	if err := a.CheckDeploy(admin, "agency", "internal-ui", "preview", "shared-dev"); err != nil {
		t.Errorf("admin onto the shared pool should be allowed: %v", err)
	}
}

func TestRepoRuleOverridesClientRule(t *testing.T) {
	a, servers, _ := agencyFixture(t)
	_ = servers.Save(Server{ID: "acme-prod-2", Name: "acme-prod-2", ClientID: "acme"})
	if err := a.SaveRepoRule(RepoRule{Owner: "agency", Repo: "acme-site", Environment: "production", ServerID: "acme-prod-2"}); err != nil {
		t.Fatal(err)
	}
	if err := a.CheckDeploy(dev, "agency", "acme-site", "production", "acme-prod-2"); err != nil {
		t.Errorf("override target should be allowed: %v", err)
	}
	if err := a.CheckDeploy(dev, "agency", "acme-site", "production", "acme-prod"); !IsForbidden(err) {
		t.Errorf("client rule must be replaced by the override, got %v", err)
	}
}

func TestPolicyWrites_RefuseCrossClientPlacement(t *testing.T) {
	a, _, _ := agencyFixture(t)
	if err := a.SaveEnvRule(EnvRule{ClientID: "acme", Environment: "staging", ServerID: "globex-prod"}); !IsForbidden(err) {
		t.Errorf("acme rule onto globex's server must be refused, got %v", err)
	}
	if err := a.SaveRepoRule(RepoRule{Owner: "agency", Repo: "globex-api", Environment: "staging", ServerID: "acme-prod"}); !IsForbidden(err) {
		t.Errorf("globex repo onto acme's server must be refused, got %v", err)
	}
	if err := a.SaveClient(Client{ID: "Bad Id", Name: "x"}); !IsForbidden(err) {
		t.Errorf("invalid client id must be refused, got %v", err)
	}
}

func TestSetServerClient_RefusedWhileRunningAnotherClientsCode(t *testing.T) {
	a, servers, deps := agencyFixture(t)
	deps.saved = []Deployment{{Owner: "agency", Repo: "globex-api", ServerID: "shared-dev"}}
	if err := a.SetServerClient("shared-dev", "acme"); !IsForbidden(err) {
		t.Fatalf("handing a box running globex code to acme must be refused, got %v", err)
	}
	deps.saved = nil
	if err := a.SetServerClient("shared-dev", "acme"); err != nil {
		t.Fatal(err)
	}
	if s, _ := servers.Get("shared-dev"); s.ClientID != "acme" {
		t.Errorf("clientID = %q, want acme", s.ClientID)
	}
	if err := a.SetServerClient("shared-dev", AgencyClientID); err != nil {
		t.Fatal(err)
	}
	if s, _ := servers.Get("shared-dev"); s.ClientID != "" {
		t.Errorf("agency pool should be stored as empty, got %q", s.ClientID)
	}
}

func TestVisibleServers_DeveloperSeesOnlyTheirTargets(t *testing.T) {
	a, servers, _ := agencyFixture(t)
	all, _ := servers.List()
	got, err := a.VisibleServers(dev, all)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, s := range got {
		seen[s.ID] = true
	}
	if !seen["shared-dev"] || !seen["acme-prod"] || seen["globex-prod"] {
		t.Errorf("visible = %v; want shared-dev + acme-prod, never globex-prod", seen)
	}
	if got, _ := a.VisibleServers(admin, all); len(got) != len(all) {
		t.Errorf("admin should see every server")
	}
}

func TestRepoFilter(t *testing.T) {
	a, _, _ := agencyFixture(t)
	keep, err := a.RepoFilter(dev)
	if err != nil {
		t.Fatal(err)
	}
	if !keep("Agency", "Acme-Site") || !keep("acme-org", "shop") || keep("agency", "globex-api") {
		t.Error("filter should keep exactly the repos the dev can read")
	}
}
