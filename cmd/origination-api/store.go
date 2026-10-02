package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// In-memory stand-ins for the business tables in the architecture diagram.

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
	FirstSeen      time.Time `json:"first_seen"`
	LastSeen       time.Time `json:"last_seen"`
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

type store struct {
	mu           sync.Mutex
	seq          int
	staff        []*StaffResource
	customers    []*Customer
	items        []*WorkItem
	approvals    []*CaseApproval
	grants       []*SupportGrant
	availability map[string]*Availability
	logs         []LogEntry
}

func newStore() *store {
	s := &store{availability: map[string]*Availability{}}
	seed := &Customer{ID: "C-0001", Iss: "seed", Sub: "seed", Name: "Seed Customer (demo data)", PersonalNumber: "197001019990", FirstSeen: time.Now(), LastSeen: time.Now()}
	s.customers = append(s.customers, seed)
	s.createItemLocked(seed, 150000, "Car loan (seeded)", "seed")
	s.createItemLocked(seed, 60000, "Renovation (seeded)", "seed")
	return s
}

func (s *store) next(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s-%04d", prefix, s.seq)
}

func (s *store) log(e LogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.At = time.Now()
	s.logs = append(s.logs, e)
	if len(s.logs) > 300 {
		s.logs = s.logs[len(s.logs)-300:]
	}
}

func (s *store) upsertStaff(p *Principal) *StaffResource {
	s.mu.Lock()
	defer s.mu.Unlock()
	var row *StaffResource
	for _, r := range s.staff {
		if r.Iss == p.Iss && r.Sub == p.Sub {
			row = r
		}
	}
	if row == nil {
		row = &StaffResource{ID: s.next("S"), Iss: p.Iss, Sub: p.Sub, Active: true, FirstSeen: time.Now()}
		s.staff = append(s.staff, row)
	}
	// The JWT is the source of truth for name and roles; refresh on every call.
	row.Name, row.StaffID, row.Email = p.Name, p.StaffID, p.Email
	row.Roles, row.Groups, row.LastSeen = appRoles(p.Roles), p.Groups, time.Now()
	cp := *row
	return &cp
}

func (s *store) upsertCustomer(p *Principal) *Customer {
	s.mu.Lock()
	defer s.mu.Unlock()
	var row *Customer
	for _, r := range s.customers {
		if r.Iss == p.Iss && r.Sub == p.Sub {
			row = r
		}
	}
	if row == nil {
		row = &Customer{ID: s.next("C"), Iss: p.Iss, Sub: p.Sub, FirstSeen: time.Now()}
		s.customers = append(s.customers, row)
	}
	row.Name, row.PersonalNumber, row.LastSeen = p.Name, p.PersonalNumber, time.Now()
	cp := *row
	return &cp
}

func (s *store) customerByPN(pn string) *Customer {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.customers {
		if c.PersonalNumber == pn {
			cp := *c
			return &cp
		}
	}
	return nil
}

func (s *store) allCustomers() []Customer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Customer{}
	for _, c := range s.customers {
		out = append(out, *c)
	}
	return out
}

func (s *store) grant(st *StaffResource, c *Customer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.grants {
		if g.StaffID == st.ID && g.CustomerID == c.ID {
			g.Uses++
			g.LastUsed = time.Now()
			return
		}
	}
	s.grants = append(s.grants, &SupportGrant{ID: s.next("G"), StaffID: st.ID, StaffName: st.Name,
		CustomerID: c.ID, CustomerName: c.Name, FirstUsed: time.Now(), LastUsed: time.Now(), Uses: 1})
}

func (s *store) createItemLocked(c *Customer, amount int, purpose, by string) WorkItem {
	it := &WorkItem{ID: s.next("WI"), CustomerID: c.ID, CustomerName: c.Name, Amount: amount, Purpose: purpose,
		Status: "submitted", CreatedBy: by, CreatedAt: time.Now()}
	s.items = append(s.items, it)
	return *it
}

func (s *store) createItem(c *Customer, amount int, purpose, by string) WorkItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	if purpose == "" {
		purpose = "Consumer loan"
	}
	return s.createItemLocked(c, amount, purpose, by)
}

func (s *store) itemsFor(customerID string) []WorkItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []WorkItem{}
	for _, it := range s.items {
		if it.CustomerID == customerID {
			out = append(out, *it)
		}
	}
	return out
}

func (s *store) allItems() []WorkItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []WorkItem{}
	for _, it := range s.items {
		out = append(out, *it)
	}
	return out
}

func (s *store) item(id string) *WorkItem {
	for _, it := range s.items {
		if it.ID == id {
			return it
		}
	}
	return nil
}

func (s *store) claim(id string, st *StaffResource) (WorkItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.item(id)
	switch {
	case it == nil:
		return WorkItem{}, errors.New("no such work item")
	case it.Status == "approved":
		return WorkItem{}, errors.New("already approved")
	case it.ClaimedBy != "" && it.ClaimedBy != st.ID:
		return WorkItem{}, errors.New("already claimed by " + it.ClaimedByName)
	}
	it.Status, it.ClaimedBy, it.ClaimedByName = "claimed", st.ID, st.Name
	return *it, nil
}

func (s *store) approve(id string, st *StaffResource) (CaseApproval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.item(id)
	switch {
	case it == nil:
		return CaseApproval{}, errors.New("no such work item")
	case it.Status == "approved":
		return CaseApproval{}, errors.New("already approved")
	case it.ClaimedBy != st.ID:
		return CaseApproval{}, errors.New("claim the work item before approving")
	}
	it.Status = "approved"
	ap := &CaseApproval{ID: s.next("AP"), WorkItemID: it.ID, ApprovedBy: st.ID, ApprovedByName: st.Name, At: time.Now()}
	s.approvals = append(s.approvals, ap)
	return *ap, nil
}

func (s *store) setAvailability(st *StaffResource, status string) Availability {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := &Availability{StaffID: st.ID, StaffName: st.Name, Status: status, At: time.Now()}
	s.availability[st.ID] = a
	return *a
}

type snapshot struct {
	StaffResource []StaffResource `json:"staff_resource"`
	Customers     []Customer      `json:"customer"`
	WorkItems     []WorkItem      `json:"work_item"`
	CaseApprovals []CaseApproval  `json:"case_approval"`
	SupportGrants []SupportGrant  `json:"support_grant"`
	Availability  []Availability  `json:"availability"`
	Log           []LogEntry      `json:"log"` // newest first
}

func (s *store) snapshot() snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sn snapshot
	for _, r := range s.staff {
		sn.StaffResource = append(sn.StaffResource, *r)
	}
	for _, r := range s.customers {
		sn.Customers = append(sn.Customers, *r)
	}
	for _, r := range s.items {
		sn.WorkItems = append(sn.WorkItems, *r)
	}
	for _, r := range s.approvals {
		sn.CaseApprovals = append(sn.CaseApprovals, *r)
	}
	for _, r := range s.grants {
		sn.SupportGrants = append(sn.SupportGrants, *r)
	}
	for _, r := range s.staff {
		if a, ok := s.availability[r.ID]; ok {
			sn.Availability = append(sn.Availability, *a)
		}
	}
	for i := len(s.logs) - 1; i >= 0; i-- {
		sn.Log = append(sn.Log, s.logs[i])
	}
	return sn
}

// ---------- optional directory leaver check (mocked) ----------
//
// In production a scheduled job would ask the Google Directory API (read-only)
// whether staff are suspended/removed. Here the console button plays that role.

type leaverResult struct {
	Staff          string   `json:"staff"`
	ReleasedItems  []string `json:"released_items"`
	SessionsEnded  bool     `json:"broker_sessions_ended"`
	BrokerResponse string   `json:"broker_response,omitempty"`
}

func (s *store) suspend(id, reason string) (*StaffResource, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.staff {
		if r.ID == id {
			r.Active, r.InactiveReason = false, reason
			var released []string
			for _, it := range s.items {
				if it.ClaimedBy == r.ID && it.Status == "claimed" {
					it.Status, it.ClaimedBy, it.ClaimedByName = "submitted", "", ""
					released = append(released, it.ID)
				}
			}
			if a, ok := s.availability[r.ID]; ok {
				a.Status, a.At = "away", time.Now()
			}
			cp := *r
			return &cp, released, nil
		}
	}
	return nil, nil, errors.New("no such staff_resource")
}

func (s *store) reactivate(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.staff {
		if r.ID == id {
			r.Active, r.InactiveReason = true, ""
			return nil
		}
	}
	return errors.New("no such staff_resource")
}

// endBrokerSessions logs the user out of every session in the staff realm, so
// the BFF's next refresh fails and the person has to log in again.
func endBrokerSessions(sub string) (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {conf.adminClient}, "client_secret": {conf.adminSecret}}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.PostForm(conf.kcInternal+"/realms/staff/protocol/openid-connect/token", form)
	if err != nil {
		return "", err
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if tok.AccessToken == "" {
		return "", fmt.Errorf("admin token: status %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("POST", conf.kcInternal+"/admin/realms/staff/users/"+url.PathEscape(sub)+"/logout", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err = c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 204 {
		return "", fmt.Errorf("logout: %d %s", resp.StatusCode, b)
	}
	return "204 user sessions removed", nil
}
