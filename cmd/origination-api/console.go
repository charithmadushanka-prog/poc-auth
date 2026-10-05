package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"pocauth/internal/web"
)

// Demo console: who called the API (customer vs staff vs impersonation), and
// the business tables. Unauthenticated on purpose - local demo only.

func (a *api) state(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.db.snapshot())
}

const hrSource = "HR system (mock)"

// hrDeactivate is the HR leaver hook: deactivate, never delete. Order matters:
// the row first (the API refuses the very next call), then Keycloak.
func (a *api) hrDeactivate(w http.ResponseWriter, r *http.Request) {
	st, released, err := a.db.deactivate(r.FormValue("id"), hrSource, "left the company (HR system, mock)")
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	res := leaverResult{Staff: st.Name, DeactivatedAt: st.DeactivatedAt.Format(time.RFC3339Nano), ReleasedItems: released}
	var broker []string
	if err := setBrokerUserEnabled(st.Sub, false); err != nil {
		broker = append(broker, "disable FAILED: "+err.Error())
	} else {
		res.BrokerUserLocked = true
		broker = append(broker, "user disabled")
	}
	if msg, err := endBrokerSessions(st.Sub); err != nil {
		broker = append(broker, "logout FAILED: "+err.Error())
	} else {
		res.SessionsEnded = true
		broker = append(broker, msg)
	}
	res.BrokerResponse = strings.Join(broker, "; ")
	a.db.log(LogEntry{Method: "JOB", Path: "HR leaver: deactivate", Status: 200, Kind: "system",
		Name: st.Name, PersonID: st.StaffID, Roles: st.Roles, Groups: st.Groups,
		Reason: "deactivated (not deleted); released " + strconv.Itoa(len(released)) + " claimed case(s); broker: " + res.BrokerResponse})
	if r.FormValue("ui") != "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	writeJSON(w, 200, res)
}

func (a *api) hrReactivate(w http.ResponseWriter, r *http.Request) {
	st, err := a.db.reactivate(r.FormValue("id"), hrSource)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	if err := setBrokerUserEnabled(st.Sub, true); err != nil {
		writeJSON(w, 502, map[string]string{"error": "re-enable broker user: " + err.Error()})
		return
	}
	a.db.log(LogEntry{Method: "JOB", Path: "HR: reactivate", Status: 200, Kind: "system",
		Name: st.Name, PersonID: st.StaffID, Roles: st.Roles, Groups: st.Groups, Reason: "reactivated; broker user enabled"})
	if r.FormValue("ui") != "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "reactivated"})
}

// noDelete makes the rule explicit: users are never deleted, only deactivated.
func (a *api) noDelete(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
		"error": "users are never deleted (audit and compliance); use POST /admin/hr/deactivate"})
}

func (a *api) consolePage(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Title": "Entra Origination", "App": "Entra Origination API", "Tag": "core · demo console",
		"Sub": "verifies broker JWTs locally · never calls Google or BankID", "Accent": "#7ee787",
		"S": a.db.snapshot(), "Paused": r.URL.Query().Get("pause") != "",
	}
	_ = consoleTpl.Execute(w, data)
}

var consoleTpl = web.Page(`{{define "content"}}
{{if not .Paused}}<meta http-equiv="refresh" content="4">{{end}}
<div class="card"><h2>People log — who called the API</h2>
<p class="muted" style="margin-top:0">Auto-refresh every 4s {{if .Paused}}(paused · <a class="muted" href="/">resume</a>){{else}}(<a class="muted" href="/?pause=1">pause</a>){{end}}. Personal numbers are masked in the log.</p>
<table><tr><th>Time</th><th>Who</th><th>Identity</th><th>Roles / groups</th><th>Call</th><th>Result</th></tr>
{{range .S.Log}}<tr>
<td>{{time .At}}</td>
<td>{{if eq .Kind "customer"}}<span class="pill" style="border-color:#58a6ff;color:#58a6ff">CUSTOMER</span>
{{else if eq .Kind "staff"}}<span class="pill" style="border-color:#d2a8ff;color:#d2a8ff">STAFF</span>
{{else if eq .Kind "impersonation"}}<span class="pill" style="border-color:#d29922;color:#d29922">STAFF AS CUSTOMER</span>
{{else if eq .Kind "system"}}<span class="pill">SYSTEM</span>
{{else}}<span class="pill" style="border-color:#f85149;color:#f85149">{{.Kind}}</span>{{end}}</td>
<td><b>{{.Name}}</b> <span class="muted">{{.PersonID}}</span>
{{if .ActAsName}}<br>→ acting as <b>{{.ActAsName}}</b> <span class="muted">{{.ActAsID}}</span>{{end}}
{{if .Client}}<br><span class="muted">client {{.Client}}</span>{{end}}</td>
<td>{{range .Roles}}<span class="pill role">{{.}}</span>{{end}}{{range .Groups}}<span class="pill">{{.}}</span>{{end}}</td>
<td><code>{{.Method}} {{.Path}}</code></td>
<td><span class="status {{if ok .Status}}s-ok{{else}}s-bad{{end}}">{{.Status}}</span> {{.Reason}}</td>
</tr>{{else}}<tr><td colspan="6" class="muted">No calls yet. Log in to a portal and press a button.</td></tr>{{end}}
</table></div>

<div class="card"><h2>staff_resource</h2>
<table><tr><th>ID</th><th>Name</th><th>Staff ID</th><th>Roles</th><th>Groups</th><th>Status</th><th>History</th><th>Mock HR system (leaver)</th></tr>
{{range .S.StaffResource}}<tr><td>{{.ID}}</td><td>{{.Name}}<br><span class="muted">{{.Email}}</span></td><td>{{.StaffID}}</td>
<td>{{range .Roles}}<span class="pill role">{{.}}</span>{{end}}</td><td>{{range .Groups}}<span class="pill">{{.}}</span>{{end}}</td>
<td>{{if .Active}}<span class="s-ok status">active</span>{{else}}<span class="s-bad status">deactivated</span><br><span class="muted">{{.InactiveReason}}<br>since {{time .DeactivatedAt}}</span>{{end}}</td>
<td class="muted">{{range .History}}{{time .At}} {{.Status}} · {{.Source}}<br>{{end}}</td>
<td>{{if .Active}}<form class="inline" method="post" action="/admin/hr/deactivate"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="ui" value="1"><button class="danger">Deactivate (HR leaver)</button></form>
{{else}}<form class="inline" method="post" action="/admin/hr/reactivate"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="ui" value="1"><button class="ghost">Reactivate</button></form>{{end}}</td></tr>
{{else}}<tr><td colspan="8" class="muted">Rows are created on first API call (find or create from JWT). They are never deleted.</td></tr>{{end}}</table></div>

<div class="grid">
<div class="card"><h2>customer</h2><table><tr><th>ID</th><th>Name</th><th>Personal number</th></tr>
{{range .S.Customers}}<tr><td>{{.ID}}</td><td>{{.Name}}</td><td>{{.PersonalNumber}}</td></tr>{{end}}</table></div>
<div class="card"><h2>support_grant <span class="muted">(impersonation audit)</span></h2><table><tr><th>Staff</th><th>Customer</th><th>Uses</th><th>Last</th></tr>
{{range .S.SupportGrants}}<tr><td>{{.StaffName}}</td><td>{{.CustomerName}}</td><td>{{.Uses}}</td><td>{{time .LastUsed}}</td></tr>{{else}}<tr><td colspan="4" class="muted">none</td></tr>{{end}}</table></div>
</div>

<div class="card"><h2>work_item</h2><table><tr><th>ID</th><th>Customer</th><th>Amount</th><th>Purpose</th><th>Status</th><th>Claimed by</th><th>Created by</th></tr>
{{range .S.WorkItems}}<tr><td>{{.ID}}</td><td>{{.CustomerName}}</td><td>{{.Amount}} kr</td><td>{{.Purpose}}</td><td>{{.Status}}</td><td>{{.ClaimedByName}}</td><td class="muted">{{.CreatedBy}}</td></tr>{{end}}</table></div>

<div class="grid">
<div class="card"><h2>case_approval</h2><table><tr><th>ID</th><th>Work item</th><th>Approved by</th><th>At</th></tr>
{{range .S.CaseApprovals}}<tr><td>{{.ID}}</td><td>{{.WorkItemID}}</td><td>{{.ApprovedByName}}</td><td>{{time .At}}</td></tr>{{else}}<tr><td colspan="4" class="muted">none</td></tr>{{end}}</table></div>
<div class="card"><h2>availability</h2><table><tr><th>Staff</th><th>Status</th><th>At</th></tr>
{{range .S.Availability}}<tr><td>{{.StaffName}}</td><td>{{.Status}}</td><td>{{time .At}}</td></tr>{{else}}<tr><td colspan="3" class="muted">none</td></tr>{{end}}</table></div>
</div>
{{end}}`)
