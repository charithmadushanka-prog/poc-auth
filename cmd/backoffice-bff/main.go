// Backoffice BFF (staff portal). Config only: holds tokens server-side, the
// browser gets an HttpOnly session cookie, every POST needs the CSRF token.
// Login: staff realm -> Google Workspace SAML (mock).
package main

import (
	"encoding/json"
	"log"
	"net/http"

	"pocauth/internal/oidc"
	"pocauth/internal/web"
)

var (
	self   = web.Env("PUBLIC_URL", "http://localhost:3001")
	apiURL = web.Env("API_URL", "http://localhost:4000")
	store  = web.NewStore("bo_sid", self)
	// Set by run.sh when a real Google Workspace SAML app is connected.
	realGoogle = web.Env("REAL_GOOGLE_IDP", "")
	client     = (&oidc.Client{
		PublicRealmURL:   web.Env("KC_PUBLIC", "http://localhost:8080") + "/realms/staff",
		InternalRealmURL: web.Env("KC_INTERNAL", "http://localhost:8080") + "/realms/staff",
		ClientID:         "backoffice-bff",
		ClientSecret:     web.Env("CLIENT_SECRET", "backoffice-bff-secret"),
		RedirectURL:      self + "/callback",
		IdpHint:          "google-saml",
	}).Init()
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", home)
	mux.HandleFunc("GET /login", login)
	mux.HandleFunc("GET /callback", callback)
	mux.HandleFunc("POST /logout", logout)
	mux.HandleFunc("POST /action", action)
	mux.HandleFunc("GET /state.json", stateJSON)
	addr := web.Env("ADDR", ":3001")
	log.Printf("backoffice-bff on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func login(w http.ResponseWriter, r *http.Request) {
	s := store.GetOrCreate(w, r)
	s.State, s.Nonce, s.PKCE = oidc.RandomString(16), oidc.RandomString(16), oidc.RandomString(32)
	idp := client.IdpHint
	if r.URL.Query().Get("idp") == "real" && realGoogle != "" {
		idp = realGoogle
	}
	http.Redirect(w, r, client.AuthURLFor(idp, s.State, s.Nonce, s.PKCE, r.URL.Query().Get("user")), http.StatusFound)
}

func callback(w http.ResponseWriter, r *http.Request) {
	s := store.Get(r)
	if s == nil || s.State == "" || r.URL.Query().Get("state") != s.State {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		http.Error(w, "login failed: "+e+" "+r.URL.Query().Get("error_description"), http.StatusUnauthorized)
		return
	}
	t, err := client.Exchange(r.URL.Query().Get("code"), s.PKCE, s.Nonce)
	if err != nil {
		http.Error(w, "token exchange failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.State, s.Tokens, s.Mode, s.Last, s.Flash = "", t, "staff", nil, ""
	http.Redirect(w, r, "/", http.StatusFound)
}

func logout(w http.ResponseWriter, r *http.Request) {
	s := store.Get(r)
	if !store.CheckCSRF(r, s) {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	id := ""
	if s.Tokens != nil {
		id = s.Tokens.IDToken
	}
	store.Destroy(w, r)
	http.Redirect(w, r, client.LogoutURL(id, self+"/?logged_out=1"), http.StatusFound)
}

// action proxies a button press to the Entra Origination API with the JWT.
func action(w http.ResponseWriter, r *http.Request) {
	s := store.Get(r)
	if !store.CheckCSRF(r, s) {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	if !s.LoggedIn() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := web.EnsureFresh(client, s); err != nil {
		s.Tokens, s.Flash = nil, "Your session was ended by the auth broker (refresh failed). Please log in again."
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	at, id := s.Tokens.AccessToken, r.PostFormValue("id")
	switch r.PostFormValue("op") {
	case "me":
		s.Last = web.CallAPI(apiURL, "GET", "/api/me", at, "", nil)
	case "list":
		s.Last = web.CallAPI(apiURL, "GET", "/api/staff/work-items", at, "", nil)
	case "claim":
		s.Last = web.CallAPI(apiURL, "POST", "/api/staff/work-items/"+id+"/claim", at, "", nil)
	case "approve":
		s.Last = web.CallAPI(apiURL, "POST", "/api/staff/work-items/"+id+"/approve", at, "", nil)
	case "available", "away":
		s.Last = web.CallAPI(apiURL, "POST", "/api/staff/availability", at, "", map[string]string{"status": r.PostFormValue("op")})
	default:
		http.Error(w, "unknown op", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// stateJSON exposes the session view for the e2e test runner (same-origin only).
func stateJSON(w http.ResponseWriter, r *http.Request) {
	s := store.Get(r)
	out := map[string]any{"logged_in": s.LoggedIn()}
	if s != nil {
		out["csrf"], out["flash"], out["last"] = s.CSRF, s.Flash, s.Last
		if s.LoggedIn() {
			out["access_token"] = web.ViewJWT(s.Tokens.AccessToken).Claims
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func home(w http.ResponseWriter, r *http.Request) {
	s := store.Get(r)
	data := map[string]any{
		"Title": "Backoffice", "App": "Backoffice portal", "Tag": "staff · advisor & underwriter",
		"Sub": "Backoffice BFF → Entra Origination API", "Accent": "#d2a8ff", "S": s, "RealGoogle": realGoogle != "",
		"LoggedOut": r.URL.Query().Has("logged_out"),
	}
	if s.LoggedIn() {
		data["AT"], data["ID"] = web.ViewJWT(s.Tokens.AccessToken), web.ViewJWT(s.Tokens.IDToken)
		c := data["AT"].(*web.JWTView).Claims
		data["User"] = web.HeaderUser{Name: str(c["name"]), Detail: "staff " + str(c["staff_id"]), CSRF: s.CSRF}
		// Work-item table for the buttons (best effort, not shown in "last call").
		if res := web.CallAPI(apiURL, "GET", "/api/staff/work-items", s.Tokens.AccessToken, "", nil); res.Status == 200 {
			var v struct {
				Items []map[string]any `json:"work_items"`
			}
			_ = json.Unmarshal([]byte(res.Body), &v)
			data["Items"] = v.Items
		}
	}
	_ = page.Execute(w, data)
}

var page = web.Page(`{{define "content"}}
{{if .S}}{{if .S.Flash}}<div class="flash">{{.S.Flash}}</div>{{end}}{{end}}
{{if not .S.LoggedIn}}
{{if .LoggedOut}}<div class="card" style="border-color:var(--ok)"><b class="s-ok">You have signed out of {{.App}}.</b> <span class="muted">Your session in the auth broker (and the upstream test IdP) has ended.</span></div>{{end}}
<div class="card"><h2>Sign in</h2>
<p>Staff sign in through the auth broker (<b>staff realm</b>), which forwards to <b>Google Workspace SAML</b> (mocked locally).</p>
<p><b>Quick sign-in</b> <span class="muted">(pre-fills the username on the mock Google screen, then type password <code>test</code>)</span></p>
<p><a class="btn" href="/login?user=anna.advisor">Anna Advisor · advisor</a>
<a class="btn" href="/login?user=ulf.underwriter">Ulf Underwriter · underwriter</a>
<a class="btn" href="/login?user=sara.support">Sara Support · support</a>
<a class="btn" href="/login?user=lars.leaver">Lars Leaver · advisor (leaver demo)</a></p>
<a class="btn ghost" href="/login">Sign in with Google Workspace (mock) - type username yourself</a>
{{if .RealGoogle}}<p style="margin-top:14px"><a class="btn" style="background:#3fb950" href="/login?idp=real">Sign in with real Google Workspace (TEST app)</a>
<span class="muted">Uses your real Google account through the company's test SAML app.</span></p>{{end}}
<p class="muted">Use a staff user above, not the Keycloak admin login (admin/admin only works in the Keycloak admin console). If your browser autofills "admin", clear it.</p></div>
{{else}}
<div class="grid">
<div class="card"><h2>Signed in</h2>
<div class="who">{{claim .AT.Claims "name"}}</div>
<table class="kv">
<tr><td>Staff ID</td><td>{{claim .AT.Claims "staff_id"}}</td></tr>
<tr><td>Email</td><td>{{claim .AT.Claims "email"}}</td></tr>
<tr><td>Roles</td><td>{{range roles (index .AT.Claims "roles")}}<span class="pill role">{{.}}</span>{{end}}</td></tr>
<tr><td>Google groups</td><td>{{range strs (index .AT.Claims "groups")}}<span class="pill">{{.}}</span>{{end}}</td></tr>
<tr><td>MFA (amr)</td><td>{{claim .AT.Claims "amr"}}</td></tr>
<tr><td>iss</td><td>{{claim .AT.Claims "iss"}}</td></tr>
<tr><td>sub</td><td>{{claim .AT.Claims "sub"}}</td></tr>
<tr><td>aud / scope</td><td>{{claim .AT.Claims "aud"}} · {{claim .AT.Claims "scope"}}</td></tr>
</table>
<form method="post" action="/logout" style="margin-top:12px"><input type="hidden" name="csrf" value="{{.S.CSRF}}"><button class="ghost">Log out</button></form>
</div>
<div class="card"><h2>Call Entra Origination API</h2>
{{$c := .S.CSRF}}
<form class="inline" method="post" action="/action"><input type="hidden" name="csrf" value="{{$c}}"><input type="hidden" name="op" value="me"><button>GET /api/me</button></form>
<form class="inline" method="post" action="/action"><input type="hidden" name="csrf" value="{{$c}}"><input type="hidden" name="op" value="list"><button>List work items</button></form>
<form class="inline" method="post" action="/action"><input type="hidden" name="csrf" value="{{$c}}"><input type="hidden" name="op" value="available"><button class="ghost">I'm available</button></form>
<form class="inline" method="post" action="/action"><input type="hidden" name="csrf" value="{{$c}}"><input type="hidden" name="op" value="away"><button class="ghost">I'm away</button></form>
{{with .S.Last}}<p><b>{{.Method}} {{.Path}}</b> → <span class="status {{if ok .Status}}s-ok{{else}}s-bad{{end}}">{{.Status}}</span> <span class="muted">{{time .At}}</span></p><pre>{{.Body}}</pre>{{end}}
</div></div>

<div class="card"><h2>Work items</h2>
<table><tr><th>ID</th><th>Customer</th><th>Amount</th><th>Purpose</th><th>Status</th><th>Claimed by</th><th></th></tr>
{{range .Items}}<tr><td>{{.id}}</td><td>{{.customer_name}}</td><td>{{.amount}} kr</td><td>{{.purpose}}</td><td>{{.status}}</td><td>{{.claimed_by_name}}</td>
<td>{{if ne .status "approved"}}
<form class="inline" method="post" action="/action"><input type="hidden" name="csrf" value="{{$c}}"><input type="hidden" name="op" value="claim"><input type="hidden" name="id" value="{{.id}}"><button class="ghost">Claim</button></form>
<form class="inline" method="post" action="/action"><input type="hidden" name="csrf" value="{{$c}}"><input type="hidden" name="op" value="approve"><input type="hidden" name="id" value="{{.id}}"><button>Approve</button></form>{{end}}</td></tr>
{{else}}<tr><td colspan="7" class="muted">No work items visible (or the API refused - see last call).</td></tr>{{end}}</table></div>

<div class="grid">
<div class="card"><h2>Access token (sent to the API)</h2><pre>{{.AT.Header}}</pre><br><pre>{{.AT.Payload}}</pre></div>
<div class="card"><h2>ID token (kept in the BFF)</h2><pre>{{.ID.Header}}</pre><br><pre>{{.ID.Payload}}</pre></div>
</div>
{{end}}{{end}}`)

func str(v any) string { s, _ := v.(string); return s }
