// Package jwtx verifies JWTs issued by the auth broker using cached public
// keys fetched from each realm's jwks_uri. Stdlib only, so every check from the
// architecture ("typ at+jwt, alg not none, iss, aud, exp") is visible here.
package jwtx

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Issuer is one trusted realm. JWKSURL may point at an internal hostname while
// Issuer is the public URL that appears in the iss claim.
type Issuer struct {
	Issuer  string
	JWKSURL string
}

type Options struct {
	ExpectTyp string // e.g. "at+jwt"; empty = don't check
	Audience  string // required aud value; empty = don't check
	Leeway    time.Duration
}

// Error carries a short machine reason so callers can log why a token failed.
type Error struct{ Reason string }

func (e *Error) Error() string { return e.Reason }

func fail(format string, a ...any) error { return &Error{Reason: fmt.Sprintf(format, a...)} }

type Header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

type Token struct {
	Header Header
	Claims map[string]any
	Raw    string
}

type keySet struct {
	url     string
	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

type Verifier struct {
	opts    Options
	issuers map[string]*keySet
	client  *http.Client
}

func NewVerifier(issuers []Issuer, opts Options) *Verifier {
	v := &Verifier{opts: opts, issuers: map[string]*keySet{}, client: &http.Client{Timeout: 5 * time.Second}}
	if v.opts.Leeway == 0 {
		v.opts.Leeway = 30 * time.Second
	}
	for _, is := range issuers {
		v.issuers[is.Issuer] = &keySet{url: is.JWKSURL}
	}
	return v
}

// Decode splits a JWT without verifying it. Used for display in the demo UIs.
func Decode(raw string) (*Token, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, fail("malformed token")
	}
	t := &Token{Raw: raw}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(hb, &t.Header) != nil {
		return nil, fail("malformed header")
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(pb, &t.Claims) != nil {
		return nil, fail("malformed payload")
	}
	return t, nil
}

func (v *Verifier) Verify(raw string) (*Token, error) {
	t, err := Decode(raw)
	if err != nil {
		return nil, err
	}
	// alg: allowlist, never "none", never HMAC (would allow key confusion).
	if t.Header.Alg != "RS256" {
		return nil, fail("alg %q not allowed", t.Header.Alg)
	}
	if v.opts.ExpectTyp != "" {
		typ := strings.ToLower(strings.TrimPrefix(t.Header.Typ, "application/"))
		if typ != v.opts.ExpectTyp {
			return nil, fail("typ %q, want %q", t.Header.Typ, v.opts.ExpectTyp)
		}
	}
	iss, _ := t.Claims["iss"].(string)
	ks, ok := v.issuers[iss]
	if !ok {
		return nil, fail("untrusted issuer %q", iss)
	}
	key, err := ks.key(v.client, t.Header.Kid)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(raw, ".")
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fail("malformed signature")
	}
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, h[:], sig) != nil {
		return nil, fail("bad signature")
	}
	now := time.Now()
	exp, ok := num(t.Claims["exp"])
	if !ok {
		return nil, fail("missing exp")
	}
	if now.After(time.Unix(exp, 0).Add(v.opts.Leeway)) {
		return nil, fail("token expired")
	}
	if nbf, ok := num(t.Claims["nbf"]); ok && now.Add(v.opts.Leeway).Before(time.Unix(nbf, 0)) {
		return nil, fail("token not yet valid")
	}
	if v.opts.Audience != "" && !contains(Strings(t.Claims["aud"]), v.opts.Audience) {
		return nil, fail("aud does not contain %q", v.opts.Audience)
	}
	return t, nil
}

func (ks *keySet) key(c *http.Client, kid string) (*rsa.PublicKey, error) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if k, ok := ks.keys[kid]; ok {
		return k, nil
	}
	// Unknown kid: refetch (key rotation), but at most every 10s.
	if time.Since(ks.fetched) < 10*time.Second && ks.keys != nil {
		return nil, fail("unknown kid %q", kid)
	}
	keys, err := fetchJWKS(c, ks.url)
	ks.fetched = time.Now()
	if err != nil {
		return nil, fail("jwks fetch failed: %v", err)
	}
	ks.keys = keys
	if k, ok := ks.keys[kid]; ok {
		return k, nil
	}
	return nil, fail("unknown kid %q", kid)
}

func fetchJWKS(c *http.Client, url string) (map[string]*rsa.PublicKey, error) {
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kid, Kty, Use, N, E string
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}
	out := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		out[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(out) == 0 {
		return nil, errors.New("no RSA signing keys")
	}
	return out, nil
}

func num(v any) (int64, bool) {
	f, ok := v.(float64)
	return int64(f), ok
}

// Strings normalises a claim that may be a string or an array of strings.
func Strings(v any) []string {
	switch x := v.(type) {
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func Str(claims map[string]any, k string) string {
	s, _ := claims[k].(string)
	return s
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func Contains(xs []string, s string) bool { return contains(xs, s) }
