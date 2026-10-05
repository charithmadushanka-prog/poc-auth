package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Row types for the business tables in the architecture diagram (stored in
// PostgreSQL, see db.go and schema.sql).

type StaffResource struct {
	ID             string    `json:"id"`
	Iss            string    `json:"iss"`
	Sub            string    `json:"sub"`
	Name           string    `json:"name"`
	StaffID        string    `json:"staff_id"`
	Email          string    `json:"email"`
	Roles          []string  `json:"roles"`
	Groups         []string  `json:"groups"`
	Active         bool      `json:"active"`
	InactiveReason string    `json:"inactive_reason,omitempty"`
	DeactivatedAt  time.Time `json:"deactivated_at,omitzero"`
	FirstSeen      time.Time `json:"first_seen"`
	LastSeen       time.Time `json:"last_seen"`
	// Append-only. Rows are never deleted: a leaver is deactivated, so their
	// approvals, claims and log entries keep pointing at a real person.
	History []StatusChange `json:"status_history"`
}

type StatusChange struct {
	At     time.Time `json:"at"`
	Status string    `json:"status"` // active | deactivated
	Source string    `json:"source"`
	Reason string    `json:"reason,omitempty"`
}

type Customer struct {
	ID             string    `json:"id"`
	Iss            string    `json:"iss"`
	Sub            string    `json:"sub"`
	Name           string    `json:"name"`
	PersonalNumber string    `json:"personal_number"`
	FirstSeen      time.Time `json:"first_seen"`
	LastSeen       time.Time `json:"last_seen"`
}

type WorkItem struct {
	ID            string    `json:"id"`
	CustomerID    string    `json:"customer_id"`
	CustomerName  string    `json:"customer_name"`
	Amount        int       `json:"amount"`
	Purpose       string    `json:"purpose"`
	Status        string    `json:"status"` // submitted | claimed | approved
	ClaimedBy     string    `json:"claimed_by,omitempty"`
	ClaimedByName string    `json:"claimed_by_name,omitempty"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
}

type CaseApproval struct {
	ID             string    `json:"id"`
	WorkItemID     string    `json:"work_item_id"`
	ApprovedBy     string    `json:"approved_by"`
	ApprovedByName string    `json:"approved_by_name"`
	At             time.Time `json:"at"`
}

// SupportGrant records that a staff member acted as a customer (impersonation).
type SupportGrant struct {
	ID           string    `json:"id"`
	StaffID      string    `json:"staff_resource_id"`
	StaffName    string    `json:"staff_name"`
	CustomerID   string    `json:"customer_id"`
	CustomerName string    `json:"customer_name"`
	FirstUsed    time.Time `json:"first_used"`
	LastUsed     time.Time `json:"last_used"`
	Uses         int       `json:"uses"`
}

type Availability struct {
	StaffID   string    `json:"staff_resource_id"`
	StaffName string    `json:"staff_name"`
	Status    string    `json:"status"`
	At        time.Time `json:"at"`
}

type LogEntry struct {
	At        time.Time `json:"at"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Status    int       `json:"status"`
	Kind      string    `json:"kind"` // staff | customer | impersonation | anonymous | system
	Name      string    `json:"name"`
	PersonID  string    `json:"person_id"` // staff ID or masked personal number
	Roles     []string  `json:"roles,omitempty"`
	Groups    []string  `json:"groups,omitempty"`
	Client    string    `json:"client,omitempty"`
	ActAsName string    `json:"act_as_name,omitempty"`
	ActAsID   string    `json:"act_as_id,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

func (e *LogEntry) fill(p *Principal) {
	e.Kind, e.Name, e.Roles, e.Groups, e.Client = p.Kind, p.Name, appRoles(p.Roles), p.Groups, p.Client
	if e.Kind == "" {
		e.Kind = "untrusted"
	}
	e.PersonID = p.StaffID
	if p.Kind == "customer" {
		e.PersonID = mask(p.PersonalNumber)
	}
	if p.ActingAs != nil {
		e.Kind, e.ActAsName, e.ActAsID = "impersonation", p.ActingAs.Name, mask(p.ActingAs.PersonalNumber)
	}
}

// appRoles hides Keycloak's built-in roles so the log shows business roles only.
func appRoles(rs []string) []string {
	var out []string
	for _, r := range rs {
		if r != "offline_access" && r != "uma_authorization" && !strings.HasPrefix(r, "default-roles-") {
			out = append(out, r)
		}
	}
	return out
}

// mask keeps personal numbers out of the access log (data minimisation).
func mask(pn string) string {
	if len(pn) < 8 {
		return pn
	}
	return pn[:8] + "-****"
}

// ---------- HR leaver: deactivate, never delete (mocked) ----------
//
// In production the HR system (or a scheduled sync from it) calls this when a
// person leaves. Here the console button plays that role. Nothing is deleted:
// the staff_resource row and the Keycloak user stay, marked deactivated.

type leaverResult struct {
	Staff            string   `json:"staff"`
	DeactivatedAt    string   `json:"deactivated_at"`
	ReleasedItems    []string `json:"released_items"`
	BrokerUserLocked bool     `json:"broker_user_disabled"`
	SessionsEnded    bool     `json:"broker_sessions_ended"`
	BrokerResponse   string   `json:"broker_response,omitempty"`
}

// kcAdmin calls the staff realm admin API with the origination-api-admin
// service account (realm-management: manage-users, view-users).
func kcAdmin(method, path string, body any) (int, []byte, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {conf.adminClient}, "client_secret": {conf.adminSecret}}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.PostForm(conf.kcInternal+"/realms/staff/protocol/openid-connect/token", form)
	if err != nil {
		return 0, nil, err
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if tok.AccessToken == "" {
		return 0, nil, fmt.Errorf("admin token: status %d", resp.StatusCode)
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, conf.kcInternal+"/admin/realms/staff"+path, rd)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err = c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

// setBrokerUserEnabled disables (or re-enables) the staff realm user. A
// disabled user can't log in again and can't refresh tokens. The user is never deleted.
func setBrokerUserEnabled(sub string, enabled bool) error {
	code, b, err := kcAdmin("PUT", "/users/"+url.PathEscape(sub), map[string]bool{"enabled": enabled})
	if err == nil && code != 204 {
		err = fmt.Errorf("update user: %d %s", code, b)
	}
	return err
}

// endBrokerSessions logs the user out of every session in the staff realm, so
// the BFF's next refresh fails and the person has to log in again.
func endBrokerSessions(sub string) (string, error) {
	code, b, err := kcAdmin("POST", "/users/"+url.PathEscape(sub)+"/logout", nil)
	if err != nil {
		return "", err
	}
	if code != 204 {
		return "", fmt.Errorf("logout: %d %s", code, b)
	}
	return "204 user sessions removed", nil
}
