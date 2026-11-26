package traefik

import (
	"os/exec"
	"strings"
	"testing"
)

func TestRoute_RoutesEveryNameToTheLocalPort(t *testing.T) {
	got := Route(Config{Domain: "app.example.com", Aliases: []string{"www.example.com"}, Upstream: "8080"})
	for _, want := range []string{
		"rule: \"Host(`app.example.com`) || Host(`www.example.com`)\"",
		"entryPoints: [websecure]",
		"certResolver: letsencrypt\n",
		`url: "http://127.0.0.1:8080"`,
		"app-example-com:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("route missing %q:\n%s", want, got)
		}
	}
}

func TestRoute_StagingUsesTheStagingResolver(t *testing.T) {
	got := Route(Config{Domain: "app.example.com", Staging: true})
	if !strings.Contains(got, "certResolver: letsencrypt-staging") {
		t.Fatalf("staging route should use the staging resolver:\n%s", got)
	}
	if !strings.Contains(got, "127.0.0.1:3000") {
		t.Fatalf("blank upstream should default to 3000:\n%s", got)
	}
}

func TestStatic_EmailIsOptional(t *testing.T) {
	if strings.Contains(Static(""), "email:") {
		t.Fatal("no email should leave the email field out")
	}
	if !strings.Contains(Static("ops@example.com"), `email: "ops@example.com"`) {
		t.Fatal("email missing from the resolver")
	}
}

func TestScript_StopsNginxStartsTraefikAndWaitsForTheCertificate(t *testing.T) {
	s := Script(Config{Domain: "app.example.com", Upstream: "3000"})
	for _, want := range []string{
		"systemctl disable --now nginx",
		"--network host",
		Image,
		RoutePath(Config{Domain: "app.example.com"}),
		`grep -q '"main": "app.example.com"'`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q", want)
		}
	}
	// The route must be written after Traefik is ensured, never before.
	if strings.Index(s, "docker run") > strings.Index(s, "MOUNTABO_TRAEFIK_ROUTE") {
		t.Error("route written before Traefik is started")
	}
}

func TestSwitchScript_RoutesEveryDomain(t *testing.T) {
	s := SwitchScript([]Config{{Domain: "a.example.com"}, {Domain: "b.example.com", Email: "ops@example.com"}})
	for _, want := range []string{"/dynamic/a.example.com.yml", "/dynamic/b.example.com.yml", `email: "ops@example.com"`} {
		if !strings.Contains(s, want) {
			t.Errorf("switch script missing %q", want)
		}
	}
}

func TestRemoveScript_DeletesOnlyTheRoute(t *testing.T) {
	s := RemoveScript(Config{Domain: "app.example.com"})
	if !strings.Contains(s, `rm -f "/etc/mountabo/traefik/dynamic/app.example.com.yml"`) {
		t.Fatalf("remove script:\n%s", s)
	}
	if strings.Contains(s, "docker rm") {
		t.Fatal("removing one domain must not stop Traefik")
	}
}

// Every generated script must at least parse as bash.
func TestScripts_AreValidBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	c := Config{Domain: "app.example.com", Aliases: []string{"www.example.com"}, Upstream: "3000", Email: "ops@example.com"}
	for name, s := range map[string]string{
		"script":         Script(c),
		"script-noemail": Script(Config{Domain: "a.example.com"}),
		"remove":         RemoveScript(c),
		"switch":         SwitchScript([]Config{c, {Domain: "b.example.com"}}),
		"stop":           StopScript(),
	} {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = strings.NewReader(s)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: bash -n: %v\n%s", name, err, out)
		}
	}
}
