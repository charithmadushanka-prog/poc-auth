// e2e drives the running stack like a browser (cookie jar, Keycloak login
// forms, SAML auto-post forms) and checks every demo scenario end to end.
//
//	go run ./cmd/e2e
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	kc       = "http://localhost:8080"
	customer = "http://localhost:3000"
	backoff  = "http://localhost:3001"
	api      = "http://localhost:4000"
)

var failures int

func check(name string, ok bool, detail ...any) {
	if ok {
		fmt.Printf("  \033[32mPASS\033[0m %s\n", name)
		return
	}
	failures++
	fmt.Printf("  \033[31mFAIL\033[0m %s %v\n", name, detail)
}

func section(s string) { fmt.Printf("\n\033[1m%s\033[0m\n", s) }

// ---------- tiny browser ----------

type browser struct{ c *http.Client }

func newBrowser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{c: &http.Client{Jar: jar, Timeout: 20 * time.Second}}
}

func (b *browser) do(method, u string, form url.Values) (string, string, int) {
	var resp *http.Response
	var err error
	if method == "GET" {
		resp, err = b.c.Get(u)
	} else {
		req, _ := http.NewRequest("POST", u, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err = b.c.Do(req)
	}
	if err != nil {
		return "", err.Error(), 0
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.Request.URL.String(), string(body), resp.StatusCode
}

var (
	formRe   = regexp.MustCompile(`(?s)<form[^>]*>`)
	inputRe  = regexp.MustCompile(`(?s)<input[^>]*>`)
	attrRe   = regexp.MustCompile(`(\w[\w-]*)="([^"]*)"`)
	loginIDs = []string{`id="kc-form-login"`, `id="kc-idp-review-profile-form"`, `id="kc-update-profile-form"`}
)

func attrs(tag string) map[string]string {
	m := map[string]string{}
	for _, a := range attrRe.FindAllStringSubmatch(tag, -1) {
		m[a[1]] = html.UnescapeString(a[2])
	}
	return m
}

// firstForm returns the action and all inputs (with values) of the first form.
func firstForm(body string, containing string) (string, url.Values) {
	idx := 0
	if containing != "" {
		idx = strings.Index(body, containing)
		idx = strings.LastIndex(body[:idx], "<form")
	}
	rest := body[idx:]
	tag := formRe.FindString(rest)
	end := strings.Index(rest, "</form>")
	if end < 0 {
		end = len(rest)
	}
	vals := url.Values{}
	for _, in := range inputRe.FindAllString(rest[:end], -1) {
		a := attrs(in)
		if a["name"] != "" && a["type"] != "submit" {
			vals.Set(a["name"], a["value"])
		}
	}
	return attrs(tag)["action"], vals
}

// login follows the whole chain: BFF -> broker -> mock upstream login form ->
// (SAML POST forms) -> broker first-login review -> BFF callback.
func (b *browser) login(start, user, pass string) (string, string) {
	u, body, _ := b.do("GET", start, nil)
	for i := 0; i < 10; i++ {
		switch {
		case strings.Contains(body, `name="SAMLRequest"`) || strings.Contains(body, `name="SAMLResponse"`):
			action, vals := firstForm(body, "")
			u, body, _ = b.do("POST", action, vals)
		case strings.Contains(body, loginIDs[0]):
			action, vals := firstForm(body, loginIDs[0])
			vals.Set("username", user)
			vals.Set("password", pass)
			u, body, _ = b.do("POST", action, vals)
		case strings.Contains(body, loginIDs[1]) || strings.Contains(body, loginIDs[2]):
			marker := loginIDs[1]
			if !strings.Contains(body, marker) {
				marker = loginIDs[2]
			}
			action, vals := firstForm(body, marker)
			u, body, _ = b.do("POST", action, vals)
		default:
			return u, body
		}
	}
	return u, body
}

// autoPost submits the self-posting forms Keycloak uses for SAML (what a
// browser's JavaScript does on "Redirecting, please wait").
func (b *browser) autoPost(u, body string) (string, string) {
	for i := 0; i < 5 && (strings.Contains(body, `name="SAMLRequest"`) || strings.Contains(body, `name="SAMLResponse"`)); i++ {
		action, vals := firstForm(body, "")
		u, body, _ = b.do("POST", action, vals)
	}
	return u, body
}

type state struct {
	LoggedIn bool           `json:"logged_in"`
	CSRF     string         `json:"csrf"`
	Flash    string         `json:"flash"`
	Mode     string         `json:"mode"`
	ActAs    string         `json:"act_as"`
	Claims   map[string]any `json:"access_token"`
	Last     *struct {
		Status int
		Body   string
	} `json:"last"`
}

func (b *browser) state(base string) state {
	_, body, _ := b.do("GET", base+"/state.json", nil)
	var s state
	_ = json.Unmarshal([]byte(body), &s)
	return s
}

// act presses a button on a BFF page and returns the API result it recorded.
func (b *browser) act(base string, path string, kv ...string) (int, string) {
	st := b.state(base)
	form := url.Values{"csrf": {st.CSRF}}
	for i := 0; i+1 < len(kv); i += 2 {
		form.Set(kv[i], kv[i+1])
	}
	b.do("POST", base+path, form)
	st = b.state(base)
	if st.Last == nil {
		return 0, st.Flash
	}
	return st.Last.Status, st.Last.Body
}

func strs(v any) []string {
	var out []string
	switch x := v.(type) {
	case string:
		out = append(out, x)
	case []any:
		for _, e := range x {
			out = append(out, fmt.Sprint(e))
		}
	}
	return out
}

func has(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// ---------- API helpers ----------

func apiState() map[string]any {
	resp, err := http.Get(api + "/admin/state")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return m
}

func rows(table string) []map[string]any {
	var out []map[string]any
	if arr, ok := apiState()[table].([]any); ok {
		for _, r := range arr {
			out = append(out, r.(map[string]any))
		}
	}
	return out
}

func find(table, key, val string) map[string]any {
	for _, r := range rows(table) {
		if fmt.Sprint(r[key]) == val {
			return r
		}
	}
	return nil
}

func callAPI(token string, path string, hdr ...string) (int, string) {
	req, _ := http.NewRequest("GET", api+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func directGrant(client, user string) string {
	resp, err := http.PostForm(kc+"/realms/staff/protocol/openid-connect/token", url.Values{
		"grant_type": {"password"}, "client_id": {client}, "client_secret": {"test-secret"},
		"username": {user}, "password": {"test"}})
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var t struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&t)
	return t.AccessToken
}

func kcAdminSessions(sub string) int {
	resp, err := http.PostForm(kc+"/realms/master/protocol/openid-connect/token", url.Values{
		"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {"admin"}, "password": {"admin"}})
	if err != nil {
		return -1
	}
	var t struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&t)
	resp.Body.Close()
	req, _ := http.NewRequest("GET", kc+"/admin/realms/staff/users/"+sub+"/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+t.AccessToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	var s []any
	_ = json.NewDecoder(resp.Body).Decode(&s)
	return len(s)
}

// kcAdminUser returns the staff realm user's HTTP status and enabled flag.
func kcAdminUser(sub string) (int, bool) {
	resp, err := http.PostForm(kc+"/realms/master/protocol/openid-connect/token", url.Values{
		"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {"admin"}, "password": {"admin"}})
	if err != nil {
		return 0, false
	}
	var t struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&t)
	resp.Body.Close()
	req, _ := http.NewRequest("GET", kc+"/admin/realms/staff/users/"+sub, nil)
	req.Header.Set("Authorization", "Bearer "+t.AccessToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	var u struct {
		Enabled bool `json:"enabled"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&u)
	return resp.StatusCode, u.Enabled
}

// countStaff returns how many staff_resource rows have the staff ID.
func countStaff(staffID string) int {
	n := 0
	for _, r := range rows("staff_resource") {
		if r["staff_id"] == staffID {
			n++
		}
	}
	return n
}

func b64(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func logHas(pred func(e map[string]any) bool) bool {
	for _, e := range rows("log") {
		if pred(e) {
			return true
		}
	}
	return false
}

// ---------- scenarios ----------

func main() {
	section("1. Customer logs in with BankID (mock) via the customer realm")
	sven := newBrowser()
	u, _ := sven.login(customer+"/login", "sven", "test")
	st := sven.state(customer)
	check("lands back on customer portal", strings.HasPrefix(u, customer), u)
	check("BFF session holds tokens", st.LoggedIn && st.Mode == "customer")
	check("JWT name = Sven Svensson", st.Claims["name"] == "Sven Svensson", st.Claims["name"])
	check("JWT personal_number present", st.Claims["personal_number"] == "199001019999", st.Claims["personal_number"])
	check("JWT roles contain customer", has(strs(st.Claims["roles"]), "customer"), st.Claims["roles"])
	check("JWT amr = bankid (MFA)", has(strs(st.Claims["amr"]), "bankid"), st.Claims["amr"])
	check("JWT aud = entra-origination-api", has(strs(st.Claims["aud"]), "entra-origination-api"), st.Claims["aud"])
	check("JWT iss = customer realm", st.Claims["iss"] == kc+"/realms/customer", st.Claims["iss"])
	code, body := sven.act(customer, "/action", "op", "me")
	check("API /api/me identifies customer", code == 200 && strings.Contains(body, `"kind": "customer"`), code, body)
	code, body = sven.act(customer, "/action", "op", "apply", "amount", "75000", "purpose", "Kitchen")
	check("customer applies for a loan (201)", code == 201, code, body)
	code, body = sven.act(customer, "/action", "op", "list")
	check("customer sees own application", code == 200 && strings.Contains(body, "Kitchen"), code)
	check("customer does not see seeded customer's items", !strings.Contains(body, "seeded"))
	check("customer row created (find or create)", find("customer", "name", "Sven Svensson") != nil)

	section("2. BFF protections")
	_, _, c := sven.do("POST", customer+"/action", url.Values{"op": {"me"}})
	check("POST without CSRF token -> 403", c == 403, c)
	_, _, c = sven.do("POST", customer+"/action", url.Values{"op": {"me"}, "csrf": {"wrong"}})
	check("POST with wrong CSRF token -> 403", c == 403, c)
	cookieVisible := false
	for _, ck := range sven.c.Jar.Cookies(&url.URL{Scheme: "http", Host: "localhost:3000"}) {
		if strings.Count(ck.Value, ".") == 2 {
			cookieVisible = true
		}
	}
	check("no JWT stored in browser cookies", !cookieVisible)

	section("3. Advisor logs in with Google SAML (mock) via the staff realm")
	anna := newBrowser()
	u, _ = anna.login(backoff+"/login", "anna.advisor", "test")
	st = anna.state(backoff)
	check("lands back on backoffice", strings.HasPrefix(u, backoff), u)
	check("JWT name = Anna Advisor", st.Claims["name"] == "Anna Advisor", st.Claims["name"])
	check("JWT staff_id = E1001", st.Claims["staff_id"] == "E1001", st.Claims["staff_id"])
	check("Google group advisors -> role advisor", has(strs(st.Claims["roles"]), "advisor"), st.Claims["roles"])
	check("JWT groups claim carries Google groups", has(strs(st.Claims["groups"]), "advisors"), st.Claims["groups"])
	check("JWT amr = google-2sv (MFA)", has(strs(st.Claims["amr"]), "google-2sv"), st.Claims["amr"])
	check("advisor has no underwriter role", !has(strs(st.Claims["roles"]), "underwriter"))
	code, body = anna.act(backoff, "/action", "op", "me")
	check("API /api/me identifies staff", code == 200 && strings.Contains(body, `"kind": "staff"`), code, body)
	check("staff_resource row created", find("staff_resource", "staff_id", "E1001") != nil)
	code, _ = anna.act(backoff, "/action", "op", "list")
	check("advisor lists work items", code == 200, code)
	seeded := ""
	svenItem := ""
	for _, it := range rows("work_item") {
		if strings.Contains(fmt.Sprint(it["purpose"]), "Car loan") {
			seeded = fmt.Sprint(it["id"])
		}
		if it["purpose"] == "Kitchen" {
			svenItem = fmt.Sprint(it["id"])
		}
	}
	code, _ = anna.act(backoff, "/action", "op", "claim", "id", seeded)
	check("advisor claims a work item", code == 200, code)
	code, body = anna.act(backoff, "/action", "op", "approve", "id", seeded)
	check("advisor cannot approve (403 underwriter required)", code == 403 && strings.Contains(body, "underwriter"), code, body)
	code, _ = anna.act(backoff, "/action", "op", "available")
	check("advisor sets availability", code == 200, code)

	section("4. Underwriter claims and approves")
	ulf := newBrowser()
	ulf.login(backoff+"/login", "ulf.underwriter", "test")
	st = ulf.state(backoff)
	r := strs(st.Claims["roles"])
	check("ulf has roles advisor + underwriter", has(r, "advisor") && has(r, "underwriter"), r)
	code, body = ulf.act(backoff, "/action", "op", "claim", "id", seeded)
	check("cannot claim a case claimed by Anna (409)", code == 409, code, body)
	code, _ = ulf.act(backoff, "/action", "op", "claim", "id", svenItem)
	check("underwriter claims Sven's application", code == 200, code)
	code, body = ulf.act(backoff, "/action", "op", "approve", "id", svenItem)
	check("underwriter approves (case_approval)", code == 200 && strings.Contains(body, "Ulf Underwriter"), code, body)
	check("case_approval row written", find("case_approval", "work_item_id", svenItem) != nil)

	section("5. Staff impersonates a customer in the customer portal (TEST ONLY)")
	sara := newBrowser()
	u, page := sara.login(customer+"/impersonate/login", "sara.support", "test")
	st = sara.state(customer)
	check("support signs in on customer portal via staff realm", st.LoggedIn && st.Mode == "impersonation", u)
	check("impersonation token has scope impersonate-customer", strings.Contains(fmt.Sprint(st.Claims["scope"]), "impersonate-customer"), st.Claims["scope"])
	check("customer picker lists Sven", strings.Contains(page, "Sven Svensson"))
	code, body = sara.act(customer, "/impersonate/select", "personal_number", "199001019999", "name", "Sven Svensson")
	check("support acts as Sven and sees his applications", code == 200 && strings.Contains(body, "Kitchen"), code, body)
	code, body = sara.act(customer, "/action", "op", "apply", "amount", "12000", "purpose", "Applied by support")
	check("application created while impersonating", code == 201 && strings.Contains(body, "impersonating Sven Svensson"), code, body)
	code, body = sara.act(customer, "/action", "op", "me")
	check("API /api/me shows acting_as Sven", code == 200 && strings.Contains(body, "acting_as"), code)
	check("support_grant recorded Sara -> Sven", find("support_grant", "staff_name", "Sara Support") != nil)
	check("people log shows STAFF AS CUSTOMER entry", logHas(func(e map[string]any) bool {
		return e["kind"] == "impersonation" && e["name"] == "Sara Support" && e["act_as_name"] == "Sven Svensson"
	}))

	anna2 := newBrowser()
	_, page = anna2.login(customer+"/impersonate/login", "anna.advisor", "test")
	check("advisor's customer picker is refused (role support required)", strings.Contains(page, "role support required"))
	code, body = anna2.act(customer, "/impersonate/select", "personal_number", "199001019999", "name", "Sven Svensson")
	check("advisor cannot impersonate (403)", code == 403 && strings.Contains(body, "support"), code, body)

	saraBO := newBrowser()
	saraBO.login(backoff+"/login", "sara.support", "test")
	code, _ = saraBO.act(backoff, "/action", "op", "list")
	check("support can view work items in backoffice", code == 200, code)
	code, _ = saraBO.act(backoff, "/action", "op", "claim", "id", seeded)
	check("support cannot claim (403)", code == 403, code)

	section("6. HR leaver: deactivate (never delete), revoke in real time")
	lars := newBrowser()
	lars.login(backoff+"/login", "lars.leaver", "test")
	st = lars.state(backoff)
	larsSub := fmt.Sprint(st.Claims["sub"])
	larsExp := time.Unix(int64(st.Claims["exp"].(float64)), 0)
	code, _ = lars.act(backoff, "/action", "op", "me")
	check("lars works normally before", code == 200, code)
	var larsItem string
	for _, it := range rows("work_item") {
		if it["status"] == "submitted" {
			larsItem = fmt.Sprint(it["id"])
			break
		}
	}
	code, _ = lars.act(backoff, "/action", "op", "claim", "id", larsItem)
	check("lars claims a case", code == 200, code)
	check("lars has a broker session", kcAdminSessions(larsSub) >= 1)
	row := find("staff_resource", "staff_id", "E1004")
	larsID := fmt.Sprint(row["id"])
	logsBefore := len(rows("log"))

	req, _ := http.NewRequest("DELETE", api+"/admin/staff/"+larsID, nil)
	resp, err := http.DefaultClient.Do(req)
	check("deleting a user is refused (405, deactivate instead)", err == nil && resp.StatusCode == 405, resp)
	if err == nil {
		resp.Body.Close()
	}

	resp, err = http.PostForm(api+"/admin/hr/deactivate", url.Values{"id": {larsID}})
	var lr map[string]any
	if err == nil {
		_ = json.NewDecoder(resp.Body).Decode(&lr)
		resp.Body.Close()
	}
	deactivated := time.Now()
	// Real time: the very next call with the same, unexpired JWT is refused.
	code, body = lars.act(backoff, "/action", "op", "me")
	lag := time.Since(deactivated)
	check(fmt.Sprintf("REAL TIME: next call refused %dms after deactivation, JWT still valid for %ds",
		lag.Milliseconds(), int(time.Until(larsExp).Seconds())),
		code == 403 && strings.Contains(body, "deactivated") && time.Now().Before(larsExp), code, body)
	check("broker user disabled (no new login, no refresh)", lr["broker_user_disabled"] == true, lr)
	check("broker sessions ended", lr["broker_sessions_ended"] == true, lr)
	check("Keycloak has 0 sessions for lars", kcAdminSessions(larsSub) == 0)
	check("claimed case released", strings.Contains(fmt.Sprint(lr["released_items"]), larsItem), lr["released_items"])
	check("work item back to submitted", find("work_item", "id", larsItem)["status"] == "submitted")

	kcCode, enabled := kcAdminUser(larsSub)
	check("Keycloak user kept, not deleted (200, enabled=false)", kcCode == 200 && !enabled, kcCode, enabled)
	row = find("staff_resource", "id", larsID)
	check("staff_resource row kept, not deleted", row != nil)
	check("row marked deactivated with timestamp", row != nil && row["active"] == false && row["deactivated_at"] != nil, row)
	hist := fmt.Sprint(row["status_history"])
	check("status history keeps active -> deactivated", strings.Contains(hist, "active") && strings.Contains(hist, "deactivated"), hist)
	check("lars' earlier log entries kept", len(rows("log")) > logsBefore && logHas(func(e map[string]any) bool {
		return e["name"] == "Lars Leaver" && e["status"] == float64(200)
	}))

	lars3 := newBrowser()
	_, page = lars3.login(backoff+"/login", "lars.leaver", "test")
	check("deactivated lars cannot log in again (Keycloak: account disabled)", !lars3.state(backoff).LoggedIn &&
		strings.Contains(strings.ToLower(page), "disabled"), snippet(page))

	resp, _ = http.PostForm(api+"/admin/hr/reactivate", url.Values{"id": {larsID}})
	resp.Body.Close()
	_, enabled = kcAdminUser(larsSub)
	check("reactivation re-enables the same Keycloak user", enabled)
	lars2 := newBrowser()
	lars2.login(backoff+"/login", "lars.leaver", "test")
	code, _ = lars2.act(backoff, "/action", "op", "me")
	check("after reactivation lars can log in again", code == 200, code)
	row = find("staff_resource", "staff_id", "E1004")
	check("same staff_resource row reused (no duplicate)", fmt.Sprint(row["id"]) == larsID && countStaff("E1004") == 1, row["id"])
	h, _ := row["status_history"].([]any)
	last := func(i int) any { return h[len(h)-i].(map[string]any)["status"] }
	check("history ends deactivated -> active (nothing overwritten)", len(h) >= 3 && last(2) == "deactivated" && last(1) == "active", h)

	section("7. Token check middleware rejects bad tokens (direct API calls)")
	code, body = callAPI("", "/api/me")
	check("no token -> 401", code == 401, code, body)
	code, body = callAPI("not.a.jwt", "/api/me")
	check("garbage token -> 401", code == 401, code, body)
	good := directGrant("test-direct", "breakglass")
	parts := strings.Split(good, ".")
	none := b64(map[string]string{"alg": "none", "typ": "at+jwt"}) + "." + parts[1] + "."
	code, body = callAPI(none, "/api/me")
	check("alg none -> 401", code == 401 && strings.Contains(body, "alg"), code, body)
	hs := b64(map[string]string{"alg": "HS256", "typ": "at+jwt", "kid": "x"}) + "." + parts[1] + "." + parts[2]
	code, body = callAPI(hs, "/api/me")
	check("alg HS256 -> 401", code == 401 && strings.Contains(body, "alg"), code, body)
	var claims map[string]any
	pb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(pb, &claims)
	claims["roles"] = []string{"underwriter"}
	tampered := parts[0] + "." + b64(claims) + "." + parts[2]
	code, body = callAPI(tampered, "/api/me")
	check("tampered payload (role escalation) -> 401 bad signature", code == 401 && strings.Contains(body, "signature"), code, body)
	code, body = callAPI(directGrant("test-legacy-typ", "breakglass"), "/api/me")
	check("typ JWT instead of at+jwt -> 401", code == 401 && strings.Contains(body, "typ"), code, body)
	code, body = callAPI(directGrant("test-no-aud", "breakglass"), "/api/me")
	check("missing aud entra-origination-api -> 401", code == 401 && strings.Contains(body, "aud"), code, body)
	code, body = callAPI(good, "/api/me")
	check("valid token but no MFA (local break-glass user) -> 403", code == 403 && strings.Contains(body, "MFA"), code, body)
	exp := directGrant("test-expiring", "breakglass")
	fmt.Println("  ... waiting 8s for a 1s token to expire (API leeway 5s)")
	time.Sleep(8 * time.Second)
	code, body = callAPI(exp, "/api/me")
	check("expired token -> 401", code == 401 && strings.Contains(body, "expired"), code, body)
	code, body = callAPI(good, "/api/me", "X-Act-As-Customer", "199001019999")
	check("impersonation header on a non-impersonation token refused", code == 403, code, body)

	section("8. People log in Entra Origination console")
	check("log shows CUSTOMER Sven Svensson", logHas(func(e map[string]any) bool { return e["kind"] == "customer" && e["name"] == "Sven Svensson" }))
	check("log shows STAFF Anna Advisor with role advisor + group advisors", logHas(func(e map[string]any) bool {
		return e["kind"] == "staff" && e["name"] == "Anna Advisor" && has(strs(e["roles"]), "advisor") && has(strs(e["groups"]), "advisors")
	}))
	check("log masks personal numbers", !strings.Contains(fmt.Sprint(rows("log")), "199001019999"))
	check("log records refusal reasons", logHas(func(e map[string]any) bool {
		return strings.Contains(fmt.Sprint(e["reason"]), "deactivated (HR leaver)")
	}))
	_, cons, _ := newBrowser().do("GET", api+"/", nil)
	check("console page renders", strings.Contains(cons, "People log") && strings.Contains(cons, "STAFF AS CUSTOMER"))

	section("8b. Quick sign-in buttons pre-fill the username on the mock IdP")
	for _, tc := range [][3]string{
		{backoff, "/login?user=ulf.underwriter", "ulf.underwriter"},
		{customer, "/login?user=lisa", "lisa"},
		{customer, "/impersonate/login?user=sara.support", "sara.support"},
	} {
		b := newBrowser()
		u, body, _ := b.do("GET", tc[0]+tc[1], nil)
		u, body = b.autoPost(u, body)
		check(tc[1]+" -> mock login form with username "+tc[2], strings.Contains(body, loginIDs[0]) && strings.Contains(body, `value="`+tc[2]+`"`), u)
	}

	// Optional: the "real Google" identity provider added by cmd/google-saml-setup.
	// E2E_REAL_GOOGLE_USER/PASSWORD = a test account in that Google Workspace
	// (or a mock user when the setup was pointed at the mock's metadata).
	if user := os.Getenv("E2E_REAL_GOOGLE_USER"); user != "" {
		section("8c. Real Google Workspace SAML app (google-real)")
		g := newBrowser()
		u, _ := g.login(backoff+"/login?idp=real&user="+url.QueryEscape(user), user, os.Getenv("E2E_REAL_GOOGLE_PASSWORD"))
		st := g.state(backoff)
		check("login through google-real lands on backoffice", strings.HasPrefix(u, backoff) && st.LoggedIn, u)
		check("staff token has a name and email", st.Claims["name"] != nil && st.Claims["email"] != nil, st.Claims["name"], st.Claims["email"])
		check("staff token has roles from Google groups", len(appRoles(strs(st.Claims["roles"]))) > 0, st.Claims["roles"], st.Claims["groups"])
		code, body := g.act(backoff, "/action", "op", "me")
		check("Origination API accepts the token", code == 200, code, body)
	}

	section("9. Logout")
	u, _, _ = sven.do("POST", customer+"/logout", url.Values{"csrf": {sven.state(customer).CSRF}})
	check("customer logout returns to portal via broker end-session", strings.HasPrefix(u, customer), u)
	check("customer session cleared", !sven.state(customer).LoggedIn)
	_, page, _ = sven.do("GET", customer+"/login", nil)
	check("after logout BankID (mock) asks for credentials again (no silent SSO)", strings.Contains(page, loginIDs[0]))

	logoutTo := func(name string, b *browser, base, user, start string) {
		_, home := b.login(base+start, user, "test")
		check(name+": top bar shows signed-in name + Log out button", strings.Contains(home, `class="userbox"`) && strings.Contains(home, `id="logout"`), snippet(home))
		u, body, _ := b.do("POST", base+"/logout", url.Values{"csrf": {b.state(base).CSRF}})
		u, body = b.autoPost(u, body)
		check(name+": logout lands on the app's own page", strings.HasPrefix(u, base+"/?logged_out=1"), u, snippet(body))
		check(name+": page says signed out of the app by name", strings.Contains(body, "You have signed out of"), snippet(body))
		check(name+": app shows its sign-in screen", strings.Contains(body, "/login\"") || strings.Contains(body, "Log in with BankID"), snippet(body))
		check(name+": session cleared", !b.state(base).LoggedIn)
		check(name+": no Log out button once signed out", !strings.Contains(body, `id="logout"`))
	}
	logoutTo("backoffice (anna)", newBrowser(), backoff, "anna.advisor", "/login")
	logoutTo("customer portal impersonation (sara)", newBrowser(), customer, "sara.support", "/impersonate/login")
	logoutTo("customer portal (lisa)", newBrowser(), customer, "lisa", "/login")

	fmt.Println()
	if failures > 0 {
		fmt.Printf("\033[31m%d check(s) failed\033[0m\n", failures)
		os.Exit(1)
	}
	fmt.Println("\033[32mAll checks passed\033[0m")
}

func snippet(s string) string {
	if os.Getenv("E2E_DEBUG") != "" {
		txt := regexp.MustCompile(`(?s)<script.*?</script>|<style.*?</style>|<[^>]+>`).ReplaceAllString(s, " ")
		return strings.Join(strings.Fields(txt), " ")
	}
	if i := strings.Index(s, "<title>"); i >= 0 {
		s = s[i:]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func appRoles(rs []string) []string {
	var out []string
	for _, r := range rs {
		if r != "offline_access" && r != "uma_authorization" && !strings.HasPrefix(r, "default-roles-") {
			out = append(out, r)
		}
	}
	return out
}
