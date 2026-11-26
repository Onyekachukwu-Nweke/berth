package ssh

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Onyekachukwu-Nweke/berth/internal/usecase"
)

// Sample `ss -Hltnp` lines, as root sees them.
const (
	ssSSHD     = `LISTEN 0 128 0.0.0.0:22 0.0.0.0:* users:(("sshd",pid=812,fd=3))`
	ssSSHD2222 = `LISTEN 0 128 0.0.0.0:2222 0.0.0.0:* users:(("sshd",pid=812,fd=3))`
	ssResolved = `LISTEN 0 4096 127.0.0.53%lo:53 0.0.0.0:* users:(("systemd-resolve",pid=601,fd=14))`
	ssNginx    = `LISTEN 0 511 0.0.0.0:80 0.0.0.0:* users:(("nginx",pid=990,fd=6))`
	ssMySQLLo  = `LISTEN 0 151 127.0.0.1:3306 0.0.0.0:* users:(("mysqld",pid=777,fd=21))`
	ssApp8080  = `LISTEN 0 4096 [::]:8080 [::]:* users:(("docker-proxy",pid=1500,fd=4))`
)

// runWithFakes runs a bash snippet with fake ss/ufw on PATH. ss prints
// ssLines; ufw records its arguments to a file the test reads back.
func runWithFakes(t *testing.T, snippet string, ssLines ...string) (stdout, ufwCalls string) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	ufwLog := filepath.Join(dir, "ufw.log")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/usr/bin/env bash\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("ss", "cat <<'EOF'\n"+strings.Join(ssLines, "\n")+"\nEOF\n")
	write("ufw", `echo "$*" >> "`+ufwLog+`"`+"\n")
	write("apt-get", "exit 0\n")
	cmd := exec.Command(bash, "-c", "set -euo pipefail\nlog() { echo \"==> $*\"; }\n"+snippet)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("snippet failed: %v\n%s", err, out)
	}
	calls, _ := os.ReadFile(ufwLog)
	return string(out), string(calls)
}

// inUseSnippet is the bootstrap's own detection block, cut from the template.
func inUseSnippet(t *testing.T) string {
	t.Helper()
	raw, err := scripts.ReadFile("scripts/bootstrap.sh.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)IN_USE=0\n.*?\nfi\n`).Find(raw)
	if m == nil {
		t.Fatal("IN_USE block not found in the bootstrap template")
	}
	return string(m) + `echo "IN_USE=$IN_USE"`
}

func TestBootstrap_DetectsAServerThatAlreadyRunsServices(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"fresh server", []string{ssSSHD, ssResolved}, "IN_USE=0"},
		{"ssh on a custom port", []string{ssSSHD2222, ssResolved}, "IN_USE=0"},
		{"web server running", []string{ssSSHD, ssResolved, ssNginx}, "IN_USE=1"},
		{"only a local database", []string{ssSSHD, ssMySQLLo}, "IN_USE=1"},
	}
	for _, c := range cases {
		out, _ := runWithFakes(t, inUseSnippet(t), c.lines...)
		if !strings.Contains(out, c.want) {
			t.Errorf("%s: got %q, want %s", c.name, strings.TrimSpace(out), c.want)
		}
	}
}

func TestBootstrap_LeavesALiveServersPackagesAndTimezoneAlone(t *testing.T) {
	s, err := composeScript(usecase.BootstrapParams{User: "mountabo", Timezone: "Africa/Lagos", PublicKey: "ssh-ed25519 AAAA test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, gated := range []string{"apt-get upgrade -y", "timedatectl set-timezone"} {
		i := strings.Index(s, gated)
		if i < 0 {
			t.Fatalf("%q missing", gated)
		}
		guard := strings.LastIndex(s[:i], `if [ "$IN_USE" = 0 ]; then`)
		if guard < 0 || strings.Contains(s[guard:i], "\nfi\n") {
			t.Errorf("%q must only run on a fresh server", gated)
		}
	}
	if strings.Contains(s, "\napt-get update -y\n") {
		t.Error("bootstrap must use the tolerant apt_update")
	}
}

func TestFirewall_KeepsPortsAlreadyInUseOpen(t *testing.T) {
	_, calls := runWithFakes(t, optionScripts["firewall"], ssSSHD2222, ssResolved, ssNginx, ssMySQLLo, ssApp8080)
	for _, want := range []string{"allow 2222/tcp", "allow 8080/tcp"} {
		if !strings.Contains(calls, want) {
			t.Errorf("ufw calls missing %q:\n%s", want, calls)
		}
	}
	for _, unwanted := range []string{"allow 3306/tcp", "allow 53/tcp"} {
		if strings.Contains(calls, unwanted) {
			t.Errorf("loopback-only port opened: %q", unwanted)
		}
	}
	if !strings.Contains(calls, "--force enable") {
		t.Error("firewall should still be enabled")
	}
}

func TestScripts_AreValidBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	all := make([]string, 0, len(optionScripts))
	for id := range optionScripts {
		all = append(all, id)
	}
	boot, err := composeScript(usecase.BootstrapParams{User: "mountabo", Timezone: "UTC", PublicKey: "k", Options: all})
	if err != nil {
		t.Fatal(err)
	}
	apply, err := composeApply(all, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]string{"bootstrap": boot, "apply": apply} {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = strings.NewReader(s)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: bash -n: %v\n%s", name, err, out)
		}
	}
}
