// Package traefik renders the Traefik reverse-proxy setup that fronts deployed
// apps on their own domains with HTTPS. It is the optional alternative to the
// nginx package: one Traefik container per server (host networking, so it
// reaches apps on 127.0.0.1 and owns ports 80/443), a static config with a
// Let's Encrypt resolver, and one dynamic file per domain that Traefik picks up
// without a reload. Generation is pure: config in, text out.
package traefik

import (
	"fmt"
	"strings"
)

const (
	// Dir holds everything mountabo writes for Traefik on the server.
	Dir = "/etc/mountabo/traefik"
	// StaticPath is Traefik's static configuration (entry points, providers,
	// certificate resolvers).
	StaticPath = Dir + "/traefik.yml"
	// Container is the name of the Traefik container on the server.
	Container = "mountabo-traefik"
	// Image is the pinned Traefik release.
	Image = "traefik:v3.3"

	defaultUpstream = "3000"
)

// Config is one domain routed to a local app port, the same inputs the nginx
// generator takes.
type Config struct {
	Domain   string
	Aliases  []string
	Upstream string
	Email    string
	Staging  bool
}

func (c Config) names() []string {
	out := make([]string, 0, 1+len(c.Aliases))
	if d := strings.TrimSpace(c.Domain); d != "" {
		out = append(out, d)
	}
	for _, a := range c.Aliases {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

func (c Config) primary() string {
	if n := c.names(); len(n) > 0 {
		return n[0]
	}
	return ""
}

func (c Config) upstream() string {
	if u := strings.TrimSpace(c.Upstream); u != "" {
		return u
	}
	return defaultUpstream
}

func (c Config) resolver() string {
	if c.Staging {
		return "letsencrypt-staging"
	}
	return "letsencrypt"
}

func (c Config) acmeFile() string {
	if c.Staging {
		return Dir + "/acme/acme-staging.json"
	}
	return Dir + "/acme/acme.json"
}

// id names the router and service after the primary domain.
func (c Config) id() string {
	return strings.ReplaceAll(c.primary(), ".", "-")
}

// RoutePath is the dynamic config file for one domain.
func RoutePath(c Config) string {
	return Dir + "/dynamic/" + c.primary() + ".yml"
}

// Route renders the dynamic config for one domain: a router on the HTTPS entry
// point for every name, a certificate from the resolver, and a service pointing
// at the app's local port.
func Route(c Config) string {
	rules := make([]string, 0, len(c.names()))
	for _, n := range c.names() {
		rules = append(rules, "Host(`"+n+"`)")
	}
	return fmt.Sprintf(`# Managed by mountabo. Routes %[1]s to the app on 127.0.0.1:%[2]s.
http:
  routers:
    %[3]s:
      rule: "%[4]s"
      entryPoints: [websecure]
      service: %[3]s
      tls:
        certResolver: %[5]s
  services:
    %[3]s:
      loadBalancer:
        servers:
          - url: "http://127.0.0.1:%[2]s"
`, strings.Join(c.names(), ", "), c.upstream(), c.id(), strings.Join(rules, " || "), c.resolver())
}

// Static renders Traefik's static config: HTTP redirects to HTTPS, the file
// provider watching the dynamic directory, and Let's Encrypt resolvers (the
// staging one is untrusted but has generous rate limits for testing DNS).
func Static(email string) string {
	acmeEmail := ""
	if e := strings.TrimSpace(email); e != "" {
		acmeEmail = fmt.Sprintf("      email: %q\n", e)
	}
	return `# Managed by mountabo.
entryPoints:
  web:
    address: ":80"
    http:
      redirections:
        entryPoint:
          to: websecure
          scheme: https
  websecure:
    address: ":443"
providers:
  file:
    directory: /etc/traefik/dynamic
    watch: true
certificatesResolvers:
  letsencrypt:
    acme:
` + acmeEmail + `      storage: /etc/traefik/acme/acme.json
      httpChallenge:
        entryPoint: web
  letsencrypt-staging:
    acme:
` + acmeEmail + `      caServer: https://acme-staging-v02.api.letsencrypt.org/directory
      storage: /etc/traefik/acme/acme-staging.json
      httpChallenge:
        entryPoint: web
log:
  level: INFO
`
}

// ensure is the shell that makes Traefik the server's proxy: nginx stopped (it
// would hold 80/443), the static config in place, and the container running.
// The container is recreated only when the static config changed or it is not
// running, so adding a domain never restarts a healthy proxy. With no email the
// existing static config is kept, so one domain's email isn't dropped by the
// next domain's blank one.
func ensure(email string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `DIR=%q
mkdir -p "$DIR/dynamic" "$DIR/acme"
chmod 700 "$DIR/acme"

if systemctl is-active --quiet nginx 2>/dev/null; then
  log "stopping nginx so Traefik can take ports 80 and 443"
  systemctl disable --now nginx
fi

CHANGED=0
`, Dir)
	b.WriteString("cat > \"$DIR/traefik.yml.new\" <<'MOUNTABO_TRAEFIK_STATIC'\n")
	b.WriteString(Static(email))
	b.WriteString("MOUNTABO_TRAEFIK_STATIC\n")
	keep := "0"
	if strings.TrimSpace(email) == "" {
		keep = "1"
	}
	fmt.Fprintf(&b, `if [ ! -f "$DIR/traefik.yml" ] || { [ %s = 0 ] && ! cmp -s "$DIR/traefik.yml.new" "$DIR/traefik.yml"; }; then
  mv "$DIR/traefik.yml.new" "$DIR/traefik.yml"
  CHANGED=1
else
  rm -f "$DIR/traefik.yml.new"
fi

if [ "$CHANGED" = 1 ] || [ "$(docker inspect -f '{{.State.Running}}' %[2]s 2>/dev/null)" != "true" ]; then
  log "starting Traefik (%[3]s)"
  docker rm -f %[2]s >/dev/null 2>&1 || true
  docker run -d --name %[2]s --restart unless-stopped --network host \
    -v "$DIR/traefik.yml:/etc/traefik/traefik.yml:ro" \
    -v "$DIR/dynamic:/etc/traefik/dynamic:ro" \
    -v "$DIR/acme:/etc/traefik/acme" \
    %[3]s >/dev/null
fi
`, keep, Container, Image)
	return b.String()
}

// writeRoute is the shell that writes one domain's dynamic file and waits for
// its certificate. Traefik requests it in the background; if DNS doesn't point
// at the server the http-01 challenge fails, so the wait fails loudly with
// Traefik's own log rather than reporting a domain that isn't serving HTTPS.
func writeRoute(c Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "log \"routing %s to 127.0.0.1:%s\"\n", strings.Join(c.names(), ", "), c.upstream())
	fmt.Fprintf(&b, "cat > %q <<'MOUNTABO_TRAEFIK_ROUTE'\n", RoutePath(c))
	b.WriteString(Route(c))
	b.WriteString("MOUNTABO_TRAEFIK_ROUTE\n")
	fmt.Fprintf(&b, `log "waiting for the Let's Encrypt certificate for %[1]s"
for i in $(seq 1 40); do
  if grep -q '"main": "%[1]s"' %[2]q 2>/dev/null; then
    log "certificate issued for %[1]s"
    break
  fi
  if [ "$i" = 40 ]; then
    echo "no certificate after 2 minutes; check that DNS for %[1]s points at this server" >&2
    docker logs --tail 40 %[3]s 2>&1 | grep -iE "acme|error|%[1]s" >&2 || true
    exit 1
  fi
  sleep 3
done
`, c.primary(), c.acmeFile(), Container)
	return b.String()
}

const header = `#!/usr/bin/env bash
set -euo pipefail
log() { echo "==> $*"; }

`

// Script makes Traefik the proxy if it isn't already and routes one domain.
// Idempotent: rerunning it rewrites the route in place.
func Script(c Config) string {
	return header + ensure(c.Email) + "\n" + writeRoute(c) + fmt.Sprintf("log \"done, https://%s is live\"\n", c.primary())
}

// RemoveScript drops a domain's route. Traefik stops serving it as soon as the
// file is gone; its certificate stays in acme.json until it expires, harmlessly.
func RemoveScript(c Config) string {
	return header + fmt.Sprintf(`log "removing the route for %[1]s"
rm -f %[2]q
log "done, %[1]s removed"
`, c.primary(), RoutePath(c))
}

// SwitchScript moves a server from nginx to Traefik: stop nginx, start Traefik,
// and route every existing domain. Certificates are re-issued by Traefik.
func SwitchScript(domains []Config) string {
	email := ""
	for _, d := range domains {
		if strings.TrimSpace(d.Email) != "" {
			email = d.Email
			break
		}
	}
	var b strings.Builder
	b.WriteString(header)
	b.WriteString(ensure(email))
	for _, d := range domains {
		b.WriteString("\n")
		b.WriteString(writeRoute(d))
	}
	b.WriteString("log \"done, Traefik is serving this server's domains\"\n")
	return b.String()
}

// StopScript takes Traefik down when a server moves back to nginx. Route files
// and certificates are left in place so switching back is quick.
func StopScript() string {
	return header + fmt.Sprintf(`log "stopping Traefik"
docker rm -f %s >/dev/null 2>&1 || true
`, Container)
}
