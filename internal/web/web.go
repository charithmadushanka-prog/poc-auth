// Package web holds the bits shared by both BFFs: server-side sessions behind
// an HttpOnly cookie, CSRF checks, a small API client and the page layout.
package web

import (
	"bytes"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"pocauth/internal/jwtx"
	"pocauth/internal/oidc"
)

func Env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---------- sessions ----------

type Session struct {
	ID   string
	CSRF string

	// in-flight login
	State, Nonce, PKCE string

	Mode   string // "", "customer", "staff", "impersonation"
	Tokens *oidc.Tokens

	// impersonation (customer portal, TEST ONLY)
	ActAs     string // customer personal number
	ActAsName string

	Last  *APIResult
	Flash string
}

func (s *Session) LoggedIn() bool { return s != nil && s.Tokens != nil }

type Store struct {
	Cookie string // unique per app: cookies on localhost are shared across ports
	Origin string // allowed Origin for state-changing requests
	mu     sync.Mutex
	m      map[string]*Session
}

func NewStore(cookie, origin string) *Store {
	return &Store{Cookie: cookie, Origin: origin, m: map[string]*Session{}}
}

func (st *Store) Get(r *http.Request) *Session {
	c, err := r.Cookie(st.Cookie)
	if err != nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.m[c.Value]
}

func (st *Store) GetOrCreate(w http.ResponseWriter, r *http.Request) *Session {
	if s := st.Get(r); s != nil {
		return s
	}
	s := &Session{ID: oidc.RandomString(32), CSRF: oidc.RandomString(32)}
	st.mu.Lock()
	st.m[s.ID] = s
	st.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: st.Cookie, Value: s.ID, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	return s
}

func (st *Store) Destroy(w http.ResponseWriter, r *http.Request) {
	if s := st.Get(r); s != nil {
		st.mu.Lock()
		delete(st.m, s.ID)
		st.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: st.Cookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
}

// CheckCSRF: session-bound token in the form + Origin check when the browser sends one.
func (st *Store) CheckCSRF(r *http.Request, s *Session) bool {
	if s == nil || r.Method != http.MethodPost {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" && o != st.Origin {
		return false
	}
	return r.PostFormValue("csrf") == s.CSRF
}

// EnsureFresh refreshes the access token shortly before expiry. A failed
// refresh means the broker ended the session (logout, leaver check, idle).
func EnsureFresh(c *oidc.Client, s *Session) error {
	if time.Until(s.Tokens.Expiry) > 15*time.Second {
		return nil
	}
	t, err := c.Refresh(s.Tokens.RefreshToken)
	if err != nil {
		return err
	}
	if t.IDToken == "" {
		t.IDToken = s.Tokens.IDToken
	}
	s.Tokens = t
	return nil
}

// ---------- API client ----------

type APIResult struct {
	Method, Path string
	Status       int
	Body         string
	At           time.Time
}

func CallAPI(base, method, path, token, actAs string, body any) *APIResult {
	res := &APIResult{Method: method, Path: path, At: time.Now()}
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, base+path, rdr)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if actAs != "" {
		req.Header.Set("X-Act-As-Customer", actAs)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		res.Status, res.Body = 0, err.Error()
		return res
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	res.Status, res.Body = resp.StatusCode, PrettyJSON(b)
	return res
}

func PrettyJSON(b []byte) string {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return string(b)
	}
	out, _ := json.MarshalIndent(v, "", "  ")
	return string(out)
}

// ---------- templates ----------

type JWTView struct {
	Header, Payload string
	Claims          map[string]any
}

func ViewJWT(raw string) *JWTView {
	t, err := jwtx.Decode(raw)
	if err != nil {
		return &JWTView{Header: err.Error()}
	}
	h, _ := json.MarshalIndent(t.Header, "", "  ")
	p, _ := json.MarshalIndent(t.Claims, "", "  ")
	return &JWTView{Header: string(h), Payload: string(p), Claims: t.Claims}
}

var Funcs = template.FuncMap{
	"join": func(xs []string) string { return strings.Join(xs, ", ") },
	"strs": jwtx.Strings,
	// roles hides Keycloak's built-in roles so the UI shows business roles only.
	"roles": func(v any) []string {
		var out []string
		for _, r := range jwtx.Strings(v) {
			if r != "offline_access" && r != "uma_authorization" && !strings.HasPrefix(r, "default-roles-") {
				out = append(out, r)
			}
		}
		return out
	},
	"claim": func(c map[string]any, k string) string {
		switch v := c[k].(type) {
		case string:
			return v
		case nil:
			return ""
		default:
			return strings.Join(jwtx.Strings(v), ", ")
		}
	},
	"ok":   func(status int) bool { return status >= 200 && status < 300 },
	"time": func(t time.Time) string { return t.Format("15:04:05") },
}

// HeaderUser is the signed-in person shown in the top bar with the logout button.
type HeaderUser struct{ Name, Detail, CSRF string }

// Page parses an app's content template into the shared layout.
func Page(content string) *template.Template {
	return template.Must(template.Must(template.New("layout").Funcs(Funcs).Parse(layout)).Parse(content))
}

const layout = `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
:root{--bg:#0f141b;--panel:#18202b;--line:#2a3544;--text:#e6edf3;--muted:#8b98a8;--accent:{{.Accent}};--ok:#3fb950;--bad:#f85149;--warn:#d29922}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px/1.5 -apple-system,Segoe UI,Roboto,sans-serif}
header{padding:14px 24px;border-bottom:1px solid var(--line);display:flex;align-items:center;gap:12px;position:sticky;top:0;z-index:10;background:var(--bg)}
header b{font-size:17px}header .tag{color:var(--accent);border:1px solid var(--accent);border-radius:99px;padding:1px 10px;font-size:12px}
main{max-width:1150px;margin:0 auto;padding:20px 16px;display:grid;gap:16px}
.card{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:16px}
h2{margin:0 0 10px;font-size:15px;color:var(--accent)}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(320px,1fr));gap:16px}
.who{font-size:22px;font-weight:600}.muted{color:var(--muted)}
.pill{display:inline-block;background:#223044;border:1px solid var(--line);border-radius:99px;padding:0 9px;margin:2px;font-size:12px}
.pill.role{border-color:var(--accent);color:var(--accent)}
pre{background:#0b1016;border:1px solid var(--line);border-radius:8px;padding:10px;overflow:auto;max-height:420px;font-size:12px;margin:0}
button,.btn{background:var(--accent);color:#0b1016;border:0;border-radius:7px;padding:7px 13px;font-weight:600;cursor:pointer;text-decoration:none;display:inline-block;font-size:13px}
button.ghost,.btn.ghost{background:transparent;color:var(--text);border:1px solid var(--line)}
button.danger{background:var(--bad);color:#fff}
form.inline{display:inline}
input,select{background:#0b1016;color:var(--text);border:1px solid var(--line);border-radius:6px;padding:6px 8px}
table{width:100%;border-collapse:collapse;font-size:13px}th,td{text-align:left;padding:6px 8px;border-bottom:1px solid var(--line);vertical-align:top}
th{color:var(--muted);font-weight:500}
.status{font-weight:700}.s-ok{color:var(--ok)}.s-bad{color:var(--bad)}
.banner{background:#3a2a0b;border:1px solid var(--warn);color:#f2cc60;border-radius:10px;padding:12px 16px;font-weight:600}
.userbox{display:flex;align-items:center;gap:10px;padding-left:16px;margin-left:8px;border-left:1px solid var(--line)}
.flash{background:#2b1416;border:1px solid var(--bad);border-radius:10px;padding:10px 16px}
table.kv td:first-child{color:var(--muted);width:150px}
</style></head>
<body><header><b>{{.App}}</b><span class="tag">{{.Tag}}</span><span class="muted" style="margin-left:auto">{{.Sub}}</span>
{{with .User}}<span class="userbox">Signed in as <b>{{.Name}}</b>{{if .Detail}} <span class="muted">{{.Detail}}</span>{{end}}
<form class="inline" method="post" action="/logout"><input type="hidden" name="csrf" value="{{.CSRF}}"><button id="logout" class="danger">Log out</button></form></span>{{end}}
</header>
<main>{{template "content" .}}</main></body></html>`
