// Customer BFF (customer portal). Holds tokens server-side; browser gets an
// HttpOnly session cookie and every POST needs the CSRF token.
//
//	Customer login:   customer realm -> BankID via Scrive (mock).
//	Impersonation:    TEST ONLY. Staff log in through the staff realm with the
//	                  customer-bff-impersonation client (scope
//	                  impersonate-customer), pick a customer, and the BFF calls
//	                  the API with the *staff* JWT + X-Act-As-Customer. The API
//	                  enforces role support and records a support_grant.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"pocauth/internal/oidc"
	"pocauth/internal/web"
)

var (
	self       = web.Env("PUBLIC_URL", "http://localhost:3000")
	apiURL     = web.Env("API_URL", "http://localhost:4000")
	kcPublic   = web.Env("KC_PUBLIC", "http://localhost:8080")
	kcInternal = web.Env("KC_INTERNAL", "http://localhost:8080")
	store      = web.NewStore("cust_sid", self)

	customerClient = (&oidc.Client{
		PublicRealmURL: kcPublic + "/realms/customer", InternalRealmURL: kcInternal + "/realms/customer",
		ClientID: "customer-bff", ClientSecret: web.Env("CLIENT_SECRET", "customer-bff-secret"),
		RedirectURL: self + "/callback", IdpHint: "bankid",
	}).Init()
	impersonationClient = (&oidc.Client{
		PublicRealmURL: kcPublic + "/realms/staff", InternalRealmURL: kcInternal + "/realms/staff",
		ClientID: "customer-bff-impersonation", ClientSecret: web.Env("IMPERSONATION_SECRET", "impersonation-secret"),
		RedirectURL: self + "/impersonate/callback", IdpHint: "google-saml",
	}).Init()
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", home)
	mux.HandleFunc("GET /login", startLogin(customerClient))
	mux.HandleFunc("GET /callback", callback(customerClient, "customer"))
	mux.HandleFunc("GET /impersonate/login", startLogin(impersonationClient))
	mux.HandleFunc("GET /impersonate/callback", callback(impersonationClient, "impersonation"))
	mux.HandleFunc("POST /impersonate/select", selectCustomer)
	mux.HandleFunc("POST /logout", logout)
	mux.HandleFunc("POST /action", action)
	mux.HandleFunc("GET /state.json", stateJSON)
	addr := web.Env("ADDR", ":3000")
	log.Printf("customer-bff on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func clientFor(s *web.Session) *oidc.Client {
	if s.Mode == "impersonation" {
		return impersonationClient
	}
	return customerClient
}

func startLogin(c *oidc.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := store.GetOrCreate(w, r)
		s.State, s.Nonce, s.PKCE = oidc.RandomString(16), oidc.RandomString(16), oidc.RandomString(32)
		http.Redirect(w, r, c.AuthURL(s.State, s.Nonce, s.PKCE, r.URL.Query().Get("user")), http.StatusFound)
	}
}

func callback(c *oidc.Client, mode string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := store.Get(r)
		if s == nil || s.State == "" || r.URL.Query().Get("state") != s.State {
			http.Error(w, "invalid state", http.StatusBadRequest)
			return
		}
		if e := r.URL.Query().Get("error"); e != "" {
			http.Error(w, "login failed: "+e+" "+r.URL.Query().Get("error_description"), http.StatusUnauthorized)
			return
		}
		t, err := c.Exchange(r.URL.Query().Get("code"), s.PKCE, s.Nonce)
		if err != nil {
			http.Error(w, "token exchange failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		s.State, s.Tokens, s.Mode, s.Last, s.Flash, s.ActAs, s.ActAsName = "", t, mode, nil, "", "", ""
		http.Redirect(w, r, "/", http.StatusFound)
	}
}

func selectCustomer(w http.ResponseWriter, r *http.Request) {
	s := store.Get(r)
	if !store.CheckCSRF(r, s) {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	if !s.LoggedIn() || s.Mode != "impersonation" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.ActAs, s.ActAsName = r.PostFormValue("personal_number"), r.PostFormValue("name")
	s.Last = web.CallAPI(apiURL, "GET", "/api/customer/applications", s.Tokens.AccessToken, s.ActAs, nil)
	if s.Last.Status != 200 {
		s.ActAs, s.ActAsName = "", ""
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func logout(w http.ResponseWriter, r *http.Request) {
	s := store.Get(r)
	if !store.CheckCSRF(r, s) {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	c, id := clientFor(s), ""
	if s.Tokens != nil {
		id = s.Tokens.IDToken
	}
	store.Destroy(w, r)
	http.Redirect(w, r, c.LogoutURL(id, self+"/?logged_out=1"), http.StatusFound)
}

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
	if err := web.EnsureFresh(clientFor(s), s); err != nil {
		s.Tokens, s.Flash = nil, "Your session was ended by the auth broker (refresh failed). Please log in again."
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	at, actAs := s.Tokens.AccessToken, s.ActAs
	switch r.PostFormValue("op") {
	case "me":
		s.Last = web.CallAPI(apiURL, "GET", "/api/me", at, actAs, nil)
	case "list":
		s.Last = web.CallAPI(apiURL, "GET", "/api/customer/applications", at, actAs, nil)
	case "apply":
		amount, _ := strconv.Atoi(r.PostFormValue("amount"))
		s.Last = web.CallAPI(apiURL, "POST", "/api/customer/applications", at, actAs,
			map[string]any{"amount": amount, "purpose": r.PostFormValue("purpose")})
	case "stop":
		s.ActAs, s.ActAsName, s.Last = "", "", nil
	default:
		http.Error(w, "unknown op", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func stateJSON(w http.ResponseWriter, r *http.Request) {
	s := store.Get(r)
	out := map[string]any{"logged_in": s.LoggedIn()}
	if s != nil {
		out["csrf"], out["flash"], out["last"], out["mode"], out["act_as"] = s.CSRF, s.Flash, s.Last, s.Mode, s.ActAs
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
		"Title": "Customer portal", "App": "Customer portal", "Tag": "customer · loans",
		"Sub": "Customer BFF → Entra Origination API", "Accent": "#58a6ff", "S": s,
		"LoggedOut": r.URL.Query().Has("logged_out"),
	}
	if s.LoggedIn() {
		data["AT"], data["ID"] = web.ViewJWT(s.Tokens.AccessToken), web.ViewJWT(s.Tokens.IDToken)
		c := data["AT"].(*web.JWTView).Claims
		u := web.HeaderUser{Name: str(c["name"]), Detail: "customer", CSRF: s.CSRF}
		if s.Mode == "impersonation" {
			u.Detail = "staff " + str(c["staff_id"])
			if s.ActAs != "" {
				u.Detail += " · acting as " + s.ActAsName
			}
		}
		data["User"] = u
		if s.Mode == "impersonation" && s.ActAs == "" {
			if res := web.CallAPI(apiURL, "GET", "/api/staff/customers", s.Tokens.AccessToken, "", nil); res.Status == 200 {
				var v struct {
					Customers []map[string]any `json:"customers"`
				}
				_ = json.Unmarshal([]byte(res.Body), &v)
				data["Customers"] = v.Customers
			} else {
				data["PickerError"] = res
			}
		}
	}
	_ = page.Execute(w, data)
}

var page = web.Page(`{{define "content"}}
{{if .S}}{{if .S.Flash}}<div class="flash">{{.S.Flash}}</div>{{end}}{{end}}
{{if not .S.LoggedIn}}
{{if .LoggedOut}}<div class="card" style="border-color:var(--ok)"><b class="s-ok">You have signed out of {{.App}}.</b> <span class="muted">Your session in the auth broker (and the upstream test IdP) has ended.</span></div>{{end}}
<div class="grid">
<div class="card"><h2>Log in</h2>
<p>Customers log in through the auth broker (<b>customer realm</b>), which forwards to <b>BankID via Scrive</b> (mocked locally).</p>
<p><a class="btn" href="/login?user=sven">Log in with BankID as Sven Svensson</a>
<a class="btn" href="/login?user=lisa">Log in with BankID as Lisa Larsson</a></p>
<a class="btn ghost" href="/login">Log in with BankID (test) - type username yourself</a>
<p class="muted">The username is pre-filled on the mock BankID screen; password <code>test</code>. Not the Keycloak admin login.</p></div>
<div class="card" style="border-color:#d29922"><h2 style="color:#d29922">Staff: impersonate a customer (TEST ONLY)</h2>
<p>Sign in as staff through the <b>staff realm</b> (Google, mock). Requires role <b>support</b>. Every call is logged and recorded as a support_grant.</p>
<a class="btn" style="background:#d29922" href="/impersonate/login?user=sara.support">Sign in as Sara Support (allowed)</a>
<a class="btn ghost" href="/impersonate/login?user=anna.advisor">Sign in as Anna Advisor (refused)</a>
<p class="muted">Password <code>test</code>. Log in once as a customer first so the picker has someone to choose.</p></div>
</div>
{{else}}
{{$c := .S.CSRF}}
{{if eq .S.Mode "impersonation"}}
<div class="banner">IMPERSONATION (TEST): staff {{claim .AT.Claims "name"}} ({{claim .AT.Claims "staff_id"}})
{{if .S.ActAs}} is acting as customer <u>{{.S.ActAsName}}</u> ({{.S.ActAs}})
<form class="inline" method="post" action="/action"><input type="hidden" name="csrf" value="{{$c}}"><input type="hidden" name="op" value="stop"><button class="ghost">Pick another customer</button></form>
{{else}} — choose a customer below{{end}}
<form class="inline" method="post" action="/logout"><input type="hidden" name="csrf" value="{{$c}}"><button class="danger">End impersonation &amp; log out</button></form></div>
{{if not .S.ActAs}}<div class="card"><h2>Choose customer to impersonate</h2>
{{with .PickerError}}<p class="s-bad">API refused the customer list: {{.Status}}</p><pre>{{.Body}}</pre>{{end}}
<table><tr><th>Customer</th><th>Personal number</th><th></th></tr>
{{range .Customers}}<tr><td>{{.name}}</td><td>{{.personal_number}}</td><td><form class="inline" method="post" action="/impersonate/select">
<input type="hidden" name="csrf" value="{{$c}}"><input type="hidden" name="personal_number" value="{{.personal_number}}"><input type="hidden" name="name" value="{{.name}}"><button>Act as</button></form></td></tr>{{end}}</table>
{{with .S.Last}}<p><b>{{.Method}} {{.Path}}</b> → <span class="status {{if ok .Status}}s-ok{{else}}s-bad{{end}}">{{.Status}}</span></p><pre>{{.Body}}</pre>{{end}}
</div>{{end}}
{{end}}

<div class="grid">
<div class="card"><h2>{{if eq .S.Mode "impersonation"}}Signed in (staff token){{else}}Logged in{{end}}</h2>
<div class="who">{{claim .AT.Claims "name"}}</div>
<table class="kv">
{{if eq .S.Mode "impersonation"}}<tr><td>Staff ID</td><td>{{claim .AT.Claims "staff_id"}}</td></tr>
<tr><td>Google groups</td><td>{{range strs (index .AT.Claims "groups")}}<span class="pill">{{.}}</span>{{end}}</td></tr>
{{else}}<tr><td>Personal number</td><td>{{claim .AT.Claims "personal_number"}}</td></tr>{{end}}
<tr><td>Email</td><td>{{claim .AT.Claims "email"}}</td></tr>
<tr><td>Roles</td><td>{{range roles (index .AT.Claims "roles")}}<span class="pill role">{{.}}</span>{{end}}</td></tr>
<tr><td>MFA (amr)</td><td>{{claim .AT.Claims "amr"}}</td></tr>
<tr><td>iss</td><td>{{claim .AT.Claims "iss"}}</td></tr>
<tr><td>sub</td><td>{{claim .AT.Claims "sub"}}</td></tr>
<tr><td>client / scope</td><td>{{claim .AT.Claims "azp"}} · {{claim .AT.Claims "scope"}}</td></tr>
</table>
<form method="post" action="/logout" style="margin-top:12px"><input type="hidden" name="csrf" value="{{$c}}"><button class="ghost">Log out</button></form>
</div>
{{if or (ne .S.Mode "impersonation") .S.ActAs}}
<div class="card"><h2>My loan applications</h2>
<form class="inline" method="post" action="/action"><input type="hidden" name="csrf" value="{{$c}}"><input type="hidden" name="op" value="me"><button class="ghost">GET /api/me</button></form>
<form class="inline" method="post" action="/action"><input type="hidden" name="csrf" value="{{$c}}"><input type="hidden" name="op" value="list"><button>List my applications</button></form>
<form method="post" action="/action" style="margin-top:10px"><input type="hidden" name="csrf" value="{{$c}}"><input type="hidden" name="op" value="apply">
<input name="amount" value="50000" size="8"> kr <input name="purpose" value="Consumer loan" size="16"> <button>Apply</button></form>
{{with .S.Last}}<p><b>{{.Method}} {{.Path}}</b> → <span class="status {{if ok .Status}}s-ok{{else}}s-bad{{end}}">{{.Status}}</span> <span class="muted">{{time .At}}</span></p><pre>{{.Body}}</pre>{{end}}
</div>{{end}}
</div>
<div class="grid">
<div class="card"><h2>Access token (sent to the API)</h2><pre>{{.AT.Header}}</pre><br><pre>{{.AT.Payload}}</pre></div>
<div class="card"><h2>ID token (kept in the BFF)</h2><pre>{{.ID.Header}}</pre><br><pre>{{.ID.Payload}}</pre></div>
</div>
{{end}}{{end}}`)

func str(v any) string { s, _ := v.(string); return s }
