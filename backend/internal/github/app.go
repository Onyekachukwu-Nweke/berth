package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/goodylili/mountabo/internal/usecase"
	gogithub "github.com/google/go-github/v88/github"
)

// App acts as the mountabo GitHub App. Repo operations (deploy keys, Actions
// secrets, workflow commits) run on an installation token for the account that
// owns the repo, so they belong to the platform rather than to whoever clicked.
type App struct {
	id   int64
	slug string
	key  *rsa.PrivateKey

	mu       sync.Mutex
	owner    string           // the account that owns the App, once read
	installs map[string]int64 // owner (lowercased) -> installation id
	tokens   map[int64]usecase.Token
}

var (
	_ usecase.RepoTokens         = (*App)(nil)
	_ usecase.InstallationLister = (*App)(nil)
	_ usecase.HomeAccount        = (*App)(nil)
)

// NewApp builds the App from its numeric id, slug and PEM private key. The key
// may be given as PEM text (with real or escaped "\n" newlines) or as base64 of
// the PEM, which is easier to carry in an env var.
func NewApp(id int64, slug, privateKey string) (*App, error) {
	if id == 0 || privateKey == "" {
		return nil, errors.New("GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY are required")
	}
	key, err := parseKey(privateKey)
	if err != nil {
		return nil, err
	}
	return &App{id: id, slug: slug, key: key, installs: map[string]int64{}, tokens: map[int64]usecase.Token{}}, nil
}

func parseKey(raw string) (*rsa.PrivateKey, error) {
	text := strings.TrimSpace(raw)
	if !strings.Contains(text, "-----BEGIN") {
		decoded, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return nil, errors.New("GITHUB_APP_PRIVATE_KEY is neither PEM nor base64 PEM")
		}
		text = string(decoded)
	}
	text = strings.ReplaceAll(text, `\n`, "\n")
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, errors.New("GITHUB_APP_PRIVATE_KEY: no PEM block found")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("GITHUB_APP_PRIVATE_KEY: %w", err)
	}
	k, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("GITHUB_APP_PRIVATE_KEY is not an RSA key")
	}
	return k, nil
}

// Owner returns the login of the account that owns the App, read from GitHub
// once and then cached: an App can't change owner without a transfer, and a
// restart picks that up. A failed read is not cached.
func (a *App) Owner(ctx context.Context) (string, error) {
	a.mu.Lock()
	cached := a.owner
	a.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	api, err := a.appClient()
	if err != nil {
		return "", err
	}
	info, _, err := api.Apps.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("get app: %w", err)
	}
	login := info.GetOwner().GetLogin()
	if login == "" {
		return "", errors.New("GitHub returned the App without an owner")
	}
	a.mu.Lock()
	a.owner = login
	a.mu.Unlock()
	return login, nil
}

// Slug is the App's URL name, used for its public install link.
func (a *App) Slug() string { return a.slug }

// jwt signs the short-lived RS256 token that authenticates as the App itself.
// iat is backdated a minute to tolerate clock drift, as GitHub recommends.
func (a *App) jwt(now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": a.id,
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign app jwt: %w", err)
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

func (a *App) appClient() (*gogithub.Client, error) {
	tok, err := a.jwt(time.Now())
	if err != nil {
		return nil, err
	}
	return gogithub.NewClient(gogithub.WithAuthToken(tok))
}

// TokenFor returns an installation token for the account that owns repos,
// cached until shortly before it expires. It returns usecase.ErrNotConnected
// when the App is not installed on that account.
func (a *App) TokenFor(ctx context.Context, owner string) (usecase.Token, error) {
	id, err := a.installationFor(ctx, owner)
	if err != nil {
		return usecase.Token{}, err
	}
	a.mu.Lock()
	if t, ok := a.tokens[id]; ok && time.Until(t.Expiry) > 5*time.Minute {
		a.mu.Unlock()
		return t, nil
	}
	a.mu.Unlock()

	api, err := a.appClient()
	if err != nil {
		return usecase.Token{}, err
	}
	it, _, err := api.Apps.CreateInstallationToken(ctx, id, nil)
	if err != nil {
		return usecase.Token{}, fmt.Errorf("create installation token for %s: %w", owner, err)
	}
	t := usecase.Token{AccessToken: it.GetToken(), Expiry: it.GetExpiresAt().Time}
	a.mu.Lock()
	a.tokens[id] = t
	a.mu.Unlock()
	return t, nil
}

func (a *App) installationFor(ctx context.Context, owner string) (int64, error) {
	key := strings.ToLower(owner)
	a.mu.Lock()
	if id, ok := a.installs[key]; ok {
		a.mu.Unlock()
		return id, nil
	}
	a.mu.Unlock()

	api, err := a.appClient()
	if err != nil {
		return 0, err
	}
	inst, resp, err := api.Apps.GetOrganizationInstallation(ctx, owner)
	if err != nil && resp != nil && resp.StatusCode == http.StatusNotFound {
		inst, resp, err = api.Apps.GetUserInstallation(ctx, owner)
	}
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return 0, fmt.Errorf("%w: install the GitHub App on %s", usecase.ErrNotConnected, owner)
		}
		return 0, fmt.Errorf("find installation for %s: %w", owner, err)
	}
	a.mu.Lock()
	a.installs[key] = inst.GetID()
	a.mu.Unlock()
	return inst.GetID(), nil
}

// Installations lists every account the App is installed on.
func (a *App) Installations(ctx context.Context) ([]usecase.Installation, error) {
	api, err := a.appClient()
	if err != nil {
		return nil, err
	}
	out := []usecase.Installation{}
	opt := &gogithub.ListOptions{PerPage: 100}
	for {
		page, resp, err := api.Apps.ListInstallations(ctx, opt)
		if err != nil {
			return nil, fmt.Errorf("list installations: %w", err)
		}
		for _, in := range page {
			out = append(out, usecase.Installation{ID: in.GetID(), Account: in.GetAccount().GetLogin(), Type: in.GetAccount().GetType()})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opt.Page = resp.NextPage
	}
}
