// Package oidc is a minimal OIDC authorization-code + PKCE client for the BFFs.
// Endpoints are derived from the realm URL instead of discovery so the browser
// can use the public URL while the BFF talks to Keycloak on the docker network.
package oidc

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"pocauth/internal/jwtx"
)

type Client struct {
	PublicRealmURL   string // http://localhost:8080/realms/staff  (browser, iss)
	InternalRealmURL string // http://keycloak:8080/realms/staff   (back channel)
	ClientID         string
	ClientSecret     string
	RedirectURL      string
	IdpHint          string // skip the broker's own login page, go straight upstream
	http             *http.Client
	idTokens         *jwtx.Verifier
}

func (c *Client) Init() *Client {
	c.http = &http.Client{Timeout: 10 * time.Second}
	c.idTokens = jwtx.NewVerifier(
		[]jwtx.Issuer{{Issuer: c.PublicRealmURL, JWKSURL: c.InternalRealmURL + "/protocol/openid-connect/certs"}},
		jwtx.Options{Audience: c.ClientID},
	)
	return c
}

type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
	Expiry       time.Time
}

func RandomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func Challenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// AuthURL builds the authorization request. loginHint (optional) pre-fills the
// username on the upstream test IdP's login form.
func (c *Client) AuthURL(state, nonce, pkceVerifier, loginHint string) string {
	q := url.Values{
		"client_id":             {c.ClientID},
		"response_type":         {"code"},
		"scope":                 {"openid"},
		"redirect_uri":          {c.RedirectURL},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {Challenge(pkceVerifier)},
		"code_challenge_method": {"S256"},
	}
	if loginHint != "" {
		q.Set("login_hint", loginHint)
	}
	if c.IdpHint != "" {
		q.Set("kc_idp_hint", c.IdpHint)
	}
	return c.PublicRealmURL + "/protocol/openid-connect/auth?" + q.Encode()
}

func (c *Client) Exchange(code, pkceVerifier, nonce string) (*Tokens, error) {
	t, err := c.token(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {c.RedirectURL},
		"code_verifier": {pkceVerifier},
	})
	if err != nil {
		return nil, err
	}
	id, err := c.idTokens.Verify(t.IDToken)
	if err != nil {
		return nil, fmt.Errorf("id_token: %w", err)
	}
	if jwtx.Str(id.Claims, "nonce") != nonce {
		return nil, fmt.Errorf("id_token: nonce mismatch")
	}
	return t, nil
}

func (c *Client) Refresh(refreshToken string) (*Tokens, error) {
	return c.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}})
}

func (c *Client) token(form url.Values) (*Tokens, error) {
	form.Set("client_id", c.ClientID)
	form.Set("client_secret", c.ClientSecret)
	resp, err := c.http.Post(c.InternalRealmURL+"/protocol/openid-connect/token",
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("token endpoint %d: %s", resp.StatusCode, body)
	}
	var t Tokens
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, err
	}
	t.Expiry = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	return &t, nil
}

func (c *Client) LogoutURL(idToken, postLogoutRedirect string) string {
	q := url.Values{"client_id": {c.ClientID}, "post_logout_redirect_uri": {postLogoutRedirect}}
	if idToken != "" {
		q.Set("id_token_hint", idToken)
	}
	return c.PublicRealmURL + "/protocol/openid-connect/logout?" + q.Encode()
}
