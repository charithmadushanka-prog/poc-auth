// google-saml-setup connects a real Google Workspace SAML app to the staff
// realm as a second identity provider ("google-real"), next to the mock.
//
// Keycloak runs in-memory, so run.sh calls this after every start when
// google-saml/google-idp-metadata.xml exists. You can also run it by hand:
//
//	go run ./cmd/google-saml-setup -metadata google-saml/google-idp-metadata.xml
//
// The metadata file is what a Google Workspace admin downloads from
// Admin console > Apps > Web and mobile apps > (custom SAML app) > Download metadata.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var (
	kc       = flag.String("kc", "http://localhost:8080", "Keycloak base URL")
	realm    = flag.String("realm", "staff", "broker realm")
	alias    = flag.String("alias", "google-real", "identity provider alias")
	metadata = flag.String("metadata", "google-saml/google-idp-metadata.xml", "IdP metadata XML downloaded from Google")
	display  = flag.String("display", "Google Workspace (real, TEST)", "display name")

	// Attribute names as configured in the Google SAML app's "Attribute mapping".
	attrFirst  = flag.String("attr-first", "firstName", "SAML attribute for first name")
	attrLast   = flag.String("attr-last", "lastName", "SAML attribute for last name")
	attrEmail  = flag.String("attr-email", "email", "SAML attribute for email")
	attrStaff  = flag.String("attr-staff-id", "staffId", "SAML attribute for staff ID (e.g. Employee ID)")
	attrGroups = flag.String("attr-groups", "groups", "SAML attribute for Google group membership")

	// Google group values that map to app roles. Check the real values in
	// Keycloak (Users > user > Attributes > google_groups) after a first login.
	groupAdvisor     = flag.String("group-advisor", "advisors", "Google group value -> role advisor")
	groupUnderwriter = flag.String("group-underwriter", "underwriters", "Google group value -> role underwriter")
	groupSupport     = flag.String("group-support", "support", "Google group value -> role support")

	// Google does not send amr. Only set this when Workspace enforces 2-step
	// verification for every user who can reach this app.
	assume2SV = flag.Bool("assume-2sv", true, "add amr=google-2sv (Workspace must enforce 2SV)")
)

func main() {
	flag.Parse()
	xml, err := os.ReadFile(*metadata)
	check(err, "read metadata")
	token := adminToken()

	// 1. Let Keycloak parse Google's metadata (SSO URL, signing certificate, entity ID).
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("providerId", "saml")
	fw, _ := mw.CreateFormFile("file", "metadata.xml")
	_, _ = fw.Write(xml)
	_ = mw.Close()
	var imported map[string]any
	call(token, "POST", "/identity-provider/import-config", mw.FormDataContentType(), &body, &imported)
	if imported["singleSignOnServiceUrl"] == nil {
		fail("metadata did not contain a SAML SSO URL: %v", imported)
	}

	// 2. Our settings on top: SP entity ID = staff realm, email NameID, signature checks ON.
	cfg := map[string]any{}
	for k, v := range imported {
		cfg[k] = v
	}
	for k, v := range map[string]string{
		"entityId":                strings.TrimRight(*kc, "/") + "/realms/" + *realm,
		"nameIDPolicyFormat":      "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress",
		"principalType":           "SUBJECT",
		"syncMode":                "FORCE",
		"validateSignature":       "true",
		"postBindingResponse":     "true",
		"wantAuthnRequestsSigned": "false",
		"wantAssertionsEncrypted": "false",
		"loginHint":               "true",
		"backchannelSupported":    "false",
		"allowCreate":             "true",
	} {
		cfg[k] = v
	}
	idp := map[string]any{
		"alias": *alias, "displayName": *display, "providerId": "saml", "enabled": true,
		"trustEmail": true, "firstBrokerLoginFlowAlias": "first broker login", "config": cfg,
	}

	// 3. Replace any previous version, then create.
	callAllow404(token, "DELETE", "/identity-provider/instances/"+*alias)
	b, _ := json.Marshal(idp)
	call(token, "POST", "/identity-provider/instances", "application/json", bytes.NewReader(b), nil)

	// 4. Same mappers as the mock: attributes -> user attributes, groups -> roles.
	attr := func(name, from, to string) map[string]any {
		return mapper(name, "saml-user-attribute-idp-mapper", map[string]string{"attribute.name": from, "user.attribute": to})
	}
	role := func(group, role string) map[string]any {
		return mapper("group "+group+" -> role "+role, "saml-role-idp-mapper",
			map[string]string{"attribute.name": *attrGroups, "attribute.value": group, "role": role})
	}
	mappers := []map[string]any{
		attr("firstName", *attrFirst, "firstName"),
		attr("lastName", *attrLast, "lastName"),
		attr("email", *attrEmail, "email"),
		attr("staffId", *attrStaff, "staff_id"),
		attr("groups", *attrGroups, "google_groups"),
		role(*groupAdvisor, "advisor"),
		role(*groupUnderwriter, "underwriter"),
		role(*groupSupport, "support"),
	}
	if *assume2SV {
		mappers = append(mappers, mapper("amr google-2sv (Workspace enforces 2SV)", "hardcoded-attribute-idp-mapper",
			map[string]string{"attribute": "amr", "attribute.value": "google-2sv"}))
	}
	for _, m := range mappers {
		b, _ := json.Marshal(m)
		call(token, "POST", "/identity-provider/instances/"+*alias+"/mappers", "application/json", bytes.NewReader(b), nil)
	}

	acs := strings.TrimRight(*kc, "/") + "/realms/" + *realm + "/broker/" + *alias + "/endpoint"
	fmt.Printf("Connected identity provider %q in realm %q\n", *alias, *realm)
	fmt.Printf("  Google IdP entity ID : %v\n", imported["idpEntityId"])
	fmt.Printf("  Google SSO URL       : %v\n", imported["singleSignOnServiceUrl"])
	fmt.Printf("  Signature validation : on (certificate from metadata)\n")
	fmt.Printf("  Values for the Google SAML app: ACS URL %s, Entity ID %s\n", acs, cfg["entityId"])
}

func mapper(name, typ string, conf map[string]string) map[string]any {
	conf["syncMode"] = "FORCE"
	return map[string]any{"name": name, "identityProviderAlias": *alias, "identityProviderMapper": typ, "config": conf}
}

var hc = &http.Client{Timeout: 15 * time.Second}

func adminToken() string {
	resp, err := hc.PostForm(*kc+"/realms/master/protocol/openid-connect/token", url.Values{
		"grant_type": {"password"}, "client_id": {"admin-cli"},
		"username": {env("KC_ADMIN_USER", "admin")}, "password": {env("KC_ADMIN_PASSWORD", "admin")}})
	check(err, "admin login")
	defer resp.Body.Close()
	var t struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&t)
	if t.AccessToken == "" {
		fail("admin login failed: status %d", resp.StatusCode)
	}
	return t.AccessToken
}

func call(token, method, path, ctype string, body io.Reader, out any) {
	status, b := do(token, method, path, ctype, body)
	if status >= 300 {
		fail("%s %s: %d %s", method, path, status, b)
	}
	if out != nil {
		check(json.Unmarshal(b, out), "decode "+path)
	}
}

func callAllow404(token, method, path string) {
	if status, b := do(token, method, path, "", nil); status >= 300 && status != 404 {
		fail("%s %s: %d %s", method, path, status, b)
	}
}

func do(token, method, path, ctype string, body io.Reader) (int, []byte) {
	req, _ := http.NewRequest(method, *kc+"/admin/realms/"+*realm+path, body)
	req.Header.Set("Authorization", "Bearer "+token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := hc.Do(req)
	check(err, method+" "+path)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func check(err error, what string) {
	if err != nil {
		fail("%s: %v", what, err)
	}
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "google-saml-setup: "+format+"\n", a...)
	os.Exit(1)
}
