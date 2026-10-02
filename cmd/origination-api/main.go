// Entra Origination API (demo).
//
//	1 · Token check middleware - signature via cached JWKS, typ at+jwt, alg,
//	    iss, aud, exp, MFA (amr), scope. No call to Google or BankID.
//	2 · Identify the person - from the JWT only (iss+sub, name, staff ID /
//	    personal number, roles). Find-or-create row; inactive staff refused.
//
// Also serves a demo UI on / with the people log and the business tables, plus
// a mock "directory leaver check" that ends the staff member's broker session.
package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"pocauth/internal/jwtx"
	"pocauth/internal/web"
)

const (
	audience         = "entra-origination-api"
	impersonationAzp = "customer-bff-impersonation"
	impersonateScope = "impersonate-customer"
	originationScope = "origination"
	actAsHeader      = "X-Act-As-Customer"
)

type cfg struct {
	staffIss, customerIss    string
	kcInternal               string
	adminClient, adminSecret string
}

var conf cfg

func main() {
	pub := web.Env("KC_PUBLIC", "http://localhost:8080")
	internal := web.Env("KC_INTERNAL", "http://localhost:8080")
	conf = cfg{
		staffIss:    pub + "/realms/staff",
		customerIss: pub + "/realms/customer",
		kcInternal:  internal,
		adminClient: web.Env("KC_ADMIN_CLIENT", "origination-api-admin"),
		adminSecret: web.Env("KC_ADMIN_SECRET", "origination-admin-secret"),
	}
	verifier := jwtx.NewVerifier([]jwtx.Issuer{
		{Issuer: conf.staffIss, JWKSURL: internal + "/realms/staff/protocol/openid-connect/certs"},
		{Issuer: conf.customerIss, JWKSURL: internal + "/realms/customer/protocol/openid-connect/certs"},
	}, jwtx.Options{ExpectTyp: "at+jwt", Audience: audience, Leeway: leeway()})

	db := newStore()
	a := &api{db: db, v: verifier}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/me", a.authn(a.me))
	mux.HandleFunc("GET /api/customer/applications", a.authn(a.listMyApplications))
	mux.HandleFunc("POST /api/customer/applications", a.authn(a.createApplication))
	mux.HandleFunc("GET /api/staff/work-items", a.authn(a.listWorkItems))
	mux.HandleFunc("POST /api/staff/work-items/{id}/claim", a.authn(a.claim))
	mux.HandleFunc("POST /api/staff/work-items/{id}/approve", a.authn(a.approve))
	mux.HandleFunc("POST /api/staff/availability", a.authn(a.setAvailability))
	mux.HandleFunc("GET /api/staff/customers", a.authn(a.listCustomers))

	// Demo-only console (unauthenticated, bound to localhost by compose).
	mux.HandleFunc("GET /{$}", a.consolePage)
	mux.HandleFunc("GET /admin/state", a.state)
	mux.HandleFunc("POST /admin/directory/suspend", a.directorySuspend)
	mux.HandleFunc("POST /admin/directory/reactivate", a.directoryReactivate)

	addr := web.Env("ADDR", ":4000")
	log.Printf("entra-origination-api on %s (trusting %s, %s)", addr, conf.staffIss, conf.customerIss)
	log.Fatal(http.ListenAndServe(addr, mux))
}

type api struct {
	db *store
	v  *jwtx.Verifier
}

// ---------- middleware ----------

type Principal struct {
	Kind           string   `json:"kind"` // staff | customer
	Iss            string   `json:"iss"`
	Sub            string   `json:"sub"`
	Name           string   `json:"name"`
	Email          string   `json:"email,omitempty"`
	StaffID        string   `json:"staff_id,omitempty"`
	PersonalNumber string   `json:"personal_number,omitempty"`
	Roles          []string `json:"roles"`
	Groups         []string `json:"groups,omitempty"`
	AMR            []string `json:"amr"`
	Scopes         []string `json:"scopes"`
	Client         string   `json:"client"`

	Staff    *StaffResource `json:"-"`
	Customer *Customer      `json:"-"`
	ActingAs *Customer      `json:"acting_as,omitempty"` // impersonation
}

func (p *Principal) has(roles ...string) bool {
	for _, r := range roles {
		if jwtx.Contains(p.Roles, r) {
			return true
		}
	}
	return false
}

type statusWriter struct {
	http.ResponseWriter
	status int
	reason string
}

func (w *statusWriter) WriteHeader(s int) { w.status = s; w.ResponseWriter.WriteHeader(s) }

type handler func(w *statusWriter, r *http.Request, p *Principal)

func (a *api) authn(next handler) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		w := &statusWriter{ResponseWriter: rw, status: 200}
		entry := LogEntry{Method: r.Method, Path: r.URL.Path, Kind: "anonymous"}
		defer func() {
			entry.Status, entry.Reason = w.status, w.reason
			a.db.log(entry)
		}()

		p, status, err := a.check(r)
		if p != nil {
			entry.fill(p)
		}
		if err != nil {
			deny(w, status, err.Error())
			return
		}
		next(w, r, p)
	}
}

func (a *api) check(r *http.Request) (*Principal, int, error) {
	// Step 1: token check.
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || raw == "" {
		return nil, 401, errors.New("missing bearer token")
	}
	tok, err := a.v.Verify(raw)
	if err != nil {
		// Still try to show who it claimed to be in the log.
		if t, derr := jwtx.Decode(raw); derr == nil {
			return principalFrom(t.Claims, conf), 401, err
		}
		return nil, 401, err
	}
	p := principalFrom(tok.Claims, conf)
	if !jwtx.Contains(p.Scopes, originationScope) {
		return p, 403, errors.New("scope origination missing")
	}
	switch p.Kind {
	case "staff":
		if !jwtx.Contains(p.AMR, "google-2sv") {
			return p, 403, errors.New("MFA required: amr lacks google-2sv")
		}
		if p.StaffID == "" {
			return p, 403, errors.New("staff_id claim missing")
		}
	case "customer":
		if !jwtx.Contains(p.AMR, "bankid") {
			return p, 403, errors.New("MFA required: amr lacks bankid")
		}
		if p.PersonalNumber == "" {
			return p, 403, errors.New("personal_number claim missing")
		}
	}

	// Step 2: identify the person (find or create the row).
	if p.Kind == "staff" {
		p.Staff = a.db.upsertStaff(p)
		if !p.Staff.Active {
			return p, 403, errors.New("staff inactive (directory leaver) - refused")
		}
	} else {
		p.Customer = a.db.upsertCustomer(p)
	}

	// Impersonation (TEST ONLY): staff token + header naming the customer.
	if pn := r.Header.Get(actAsHeader); pn != "" {
		switch {
		case p.Kind != "staff":
			return p, 403, errors.New("only staff may impersonate")
		case p.Client != impersonationAzp:
			return p, 403, errors.New("impersonation only via customer-bff-impersonation client")
		case !jwtx.Contains(p.Scopes, impersonateScope):
			return p, 403, errors.New("scope impersonate-customer missing")
		case !p.has("support"):
			return p, 403, errors.New("role support required to impersonate")
		}
		c := a.db.customerByPN(pn)
		if c == nil {
			return p, 404, errors.New("no such customer to impersonate")
		}
		p.ActingAs = c
		a.db.grant(p.Staff, c)
	}
	return p, 200, nil
}

func principalFrom(c map[string]any, conf cfg) *Principal {
	p := &Principal{
		Iss: jwtx.Str(c, "iss"), Sub: jwtx.Str(c, "sub"), Name: jwtx.Str(c, "name"),
		Email: jwtx.Str(c, "email"), StaffID: jwtx.Str(c, "staff_id"),
		PersonalNumber: jwtx.Str(c, "personal_number"),
		Roles:          jwtx.Strings(c["roles"]), Groups: jwtx.Strings(c["groups"]),
		AMR: jwtx.Strings(c["amr"]), Scopes: strings.Fields(jwtx.Str(c, "scope")),
		Client: jwtx.Str(c, "azp"),
	}
	if p.Iss == conf.staffIss {
		p.Kind = "staff"
	} else if p.Iss == conf.customerIss {
		p.Kind = "customer"
	}
	return p
}

func deny(w *statusWriter, status int, reason string) {
	w.reason = reason
	writeJSON(w, status, map[string]string{"error": reason})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------- handlers ----------

func (a *api) me(w *statusWriter, r *http.Request, p *Principal) {
	out := map[string]any{"principal": p}
	if p.Staff != nil {
		out["staff_resource"] = p.Staff
	}
	if p.Customer != nil {
		out["customer"] = p.Customer
	}
	writeJSON(w, 200, out)
}

// customerScope resolves which customer a customer-facing call is about.
func customerScope(w *statusWriter, p *Principal) *Customer {
	if p.ActingAs != nil {
		return p.ActingAs
	}
	if p.Kind == "customer" {
		return p.Customer
	}
	deny(w, 403, "customer endpoint: staff token without impersonation")
	return nil
}

func (a *api) listMyApplications(w *statusWriter, r *http.Request, p *Principal) {
	if c := customerScope(w, p); c != nil {
		writeJSON(w, 200, map[string]any{"customer": c.Name, "applications": a.db.itemsFor(c.ID)})
	}
}

func (a *api) createApplication(w *statusWriter, r *http.Request, p *Principal) {
	c := customerScope(w, p)
	if c == nil {
		return
	}
	var in struct {
		Amount  int    `json:"amount"`
		Purpose string `json:"purpose"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Amount <= 0 {
		deny(w, 400, "amount must be > 0")
		return
	}
	by := c.Name
	if p.ActingAs != nil {
		by = p.Name + " (impersonating " + c.Name + ")"
	}
	writeJSON(w, 201, a.db.createItem(c, in.Amount, in.Purpose, by))
}

func staffOnly(w *statusWriter, p *Principal) bool {
	if p.Kind != "staff" || p.ActingAs != nil {
		deny(w, 403, "staff endpoint")
		return false
	}
	return true
}

func (a *api) listWorkItems(w *statusWriter, r *http.Request, p *Principal) {
	if !staffOnly(w, p) {
		return
	}
	if !p.has("advisor", "underwriter", "support") {
		deny(w, 403, "role advisor/underwriter/support required")
		return
	}
	writeJSON(w, 200, map[string]any{"work_items": a.db.allItems()})
}

func (a *api) claim(w *statusWriter, r *http.Request, p *Principal) {
	if !staffOnly(w, p) {
		return
	}
	if !p.has("advisor", "underwriter") {
		deny(w, 403, "role advisor or underwriter required to claim")
		return
	}
	it, err := a.db.claim(r.PathValue("id"), p.Staff)
	if err != nil {
		deny(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, it)
}

func (a *api) approve(w *statusWriter, r *http.Request, p *Principal) {
	if !staffOnly(w, p) {
		return
	}
	if !p.has("underwriter") {
		deny(w, 403, "role underwriter required to approve")
		return
	}
	ap, err := a.db.approve(r.PathValue("id"), p.Staff)
	if err != nil {
		deny(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, ap)
}

func (a *api) setAvailability(w *statusWriter, r *http.Request, p *Principal) {
	if !staffOnly(w, p) {
		return
	}
	var in struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Status != "available" && in.Status != "away" {
		deny(w, 400, "status must be available|away")
		return
	}
	writeJSON(w, 200, a.db.setAvailability(p.Staff, in.Status))
}

func (a *api) listCustomers(w *statusWriter, r *http.Request, p *Principal) {
	if p.Kind != "staff" || !p.has("support") {
		deny(w, 403, "role support required")
		return
	}
	writeJSON(w, 200, map[string]any{"customers": a.db.allCustomers()})
}

// leeway for exp/nbf clock skew; JWT_LEEWAY lets the e2e test use a small value.
func leeway() time.Duration {
	d, err := time.ParseDuration(web.Env("JWT_LEEWAY", "30s"))
	if err != nil {
		return 30 * time.Second
	}
	return d
}
