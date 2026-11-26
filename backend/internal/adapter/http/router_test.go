package http

import (
	"io"
	"log/slog"
	nethttp "net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Onyekachukwu-Nweke/berth/internal/usecase"
)

// publicRoutes are the only routes reachable without a session.
var publicRoutes = map[string]bool{
	"GET /healthz":          true,
	"GET /api/auth/config":  true,
	"POST /api/auth/github": true,
}

var routeLine = regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+) ([^"]+)", (.+)\)$`)

// routes reads every registered route straight from router.go, so a route
// added later is covered without touching this test.
func routes(t *testing.T) [][3]string {
	t.Helper()
	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatal(err)
	}
	var out [][3]string
	for _, line := range strings.Split(string(src), "\n") {
		if m := routeLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			out = append(out, [3]string{m[1], m[2], m[3]})
		}
	}
	if len(out) < 40 {
		t.Fatalf("parsed only %d routes from router.go; the pattern is stale", len(out))
	}
	return out
}

// Every route except the public ones must go through the guard.
func TestRouter_EveryNonPublicRouteIsGuarded(t *testing.T) {
	for _, r := range routes(t) {
		key := r[0] + " " + r[1]
		if publicRoutes[key] {
			continue
		}
		if !strings.HasPrefix(r[2], "g.") {
			t.Errorf("%s is registered without a guard: %s", key, r[2])
		}
	}
}

// And the guard really refuses: every non-public route answers 401 without a
// session, before any handler runs (the handlers here are nil and would panic).
func TestRouter_NonPublicRoutesRequireASession(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	auth := usecase.NewAuthService(nil, nil, nil, nil, nil, nil, nil, nil, log)
	g := NewGuard(auth, nil, log)
	mux := NewRouter(g, &AuthHandler{log: log}, nil, nil, nil, nil, nil, nil, nil, nil)

	param := regexp.MustCompile(`\{[^}]+\}`)
	for _, r := range routes(t) {
		key := r[0] + " " + r[1]
		if publicRoutes[key] {
			continue
		}
		path := param.ReplaceAllString(r[1], "x")
		req := httptest.NewRequest(r[0], path+"?owner=x&repo=x", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer ")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != nethttp.StatusUnauthorized {
			t.Errorf("%s without a session = %d, want 401", key, rec.Code)
		}
	}
}
