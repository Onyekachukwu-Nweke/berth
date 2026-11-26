// Package github talks to GitHub: as the berth GitHub App (installation
// tokens for repo operations) and as a signed-in user (identity and the repos
// they can reach). These concrete types satisfy ports declared in
// internal/usecase.
package github

import (
	"context"
	"fmt"
	"time"

	"github.com/Onyekachukwu-Nweke/berth/internal/usecase"
	"golang.org/x/oauth2"
	githuboauth "golang.org/x/oauth2/github"
)

// OAuth runs the GitHub App's user sign-in: it trades an authorization code for
// a user token and renews expired ones. The client secret is only ever sent to
// GitHub's token endpoint over TLS and is never logged or returned.
type OAuth struct {
	config *oauth2.Config
}

var _ usecase.UserAuthorizer = (*OAuth)(nil)

// NewOAuth builds an exchanger for the given OAuth App credentials. The
// redirect URI is supplied per-exchange (it depends on the request origin), so
// it is not fixed on the config here.
func NewOAuth(clientID, clientSecret string) *OAuth {
	return &OAuth{
		config: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint:     githuboauth.Endpoint,
		},
	}
}

// Exchange trades an authorization code for an access token. redirectURI
// must match the value used when the code was issued, or GitHub rejects the
// exchange.
func (o *OAuth) Exchange(ctx context.Context, code, redirectURI string) (usecase.Token, error) {
	if o.config.ClientID == "" || o.config.ClientSecret == "" {
		return usecase.Token{}, fmt.Errorf("github oauth not configured: set GITHUB_CLIENT_ID and GITHUB_CLIENT_SECRET")
	}

	tok, err := o.config.Exchange(ctx, code, oauth2.SetAuthURLParam("redirect_uri", redirectURI))
	if err != nil {
		return usecase.Token{}, fmt.Errorf("oauth token exchange: %w", err)
	}

	return fromOAuth(tok), nil
}

// Refresh renews an expired user token with its refresh token. GitHub rotates
// the refresh token on every use, so the returned Token must replace the old.
func (o *OAuth) Refresh(ctx context.Context, t usecase.Token) (usecase.Token, error) {
	if t.RefreshToken == "" {
		return usecase.Token{}, fmt.Errorf("user token expired and has no refresh token; sign in again")
	}
	src := o.config.TokenSource(ctx, &oauth2.Token{RefreshToken: t.RefreshToken, Expiry: time.Unix(1, 0)})
	tok, err := src.Token()
	if err != nil {
		return usecase.Token{}, fmt.Errorf("refresh user token: %w", err)
	}
	return fromOAuth(tok), nil
}

func fromOAuth(tok *oauth2.Token) usecase.Token {
	return usecase.Token{AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, Expiry: tok.Expiry}
}
