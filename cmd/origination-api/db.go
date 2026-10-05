package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQL-backed business tables (database poc_auth, schema in schema.sql).

//go:embed schema.sql
var schemaSQL string

type store struct{ db *pgxpool.Pool }

// querier is what both the pool and a transaction offer.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var ctx = context.Background()

// newStore creates the database if needed, then applies schema.sql.
func newStore(dsn string) (*store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	var pool *pgxpool.Pool
	for i := 0; ; i++ {
		pool, err = pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			err = pool.Ping(ctx)
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "3D000" { // database does not exist
			pool.Close()
			if err = createDatabase(cfg.ConnConfig); err == nil {
				continue
			}
		}
		if err == nil || i == 20 {
			break
		}
		log.Printf("waiting for postgres: %v", err)
		time.Sleep(time.Second)
	}
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &store{db: pool}, nil
}

func createDatabase(cc *pgx.ConnConfig) error {
	admin := cc.Copy()
	admin.Database = "postgres"
	c, err := pgx.ConnectConfig(ctx, admin)
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	log.Printf("creating database %s", cc.Database)
	_, err = c.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{cc.Database}.Sanitize())
	return err
}

// tx runs fn in a transaction.
func (s *store) tx(fn func(q pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.db, fn)
}

// newID gives the same S-0001 / WI-0002 style IDs as before, from one sequence.
const newID = `$1 || '-' || lpad(nextval('id_seq')::text, 4, '0')`

func (s *store) log(e LogEntry) {
	_, err := s.db.Exec(ctx, `INSERT INTO api_log (method, path, status, kind, name, person_id, roles, groups, client, act_as_name, act_as_id, reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		e.Method, e.Path, e.Status, e.Kind, e.Name, e.PersonID, nz(e.Roles), nz(e.Groups), e.Client, e.ActAsName, e.ActAsID, e.Reason)
	if err != nil {
		log.Printf("api_log: %v", err)
	}
}

func nz(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// ---------- staff_resource ----------

const staffCols = `id, iss, sub, name, staff_id, email, roles, groups, active, inactive_reason, deactivated_at, first_seen, last_seen`

func scanStaff(row pgx.Row) (*StaffResource, error) {
	var r StaffResource
	var deact *time.Time
	err := row.Scan(&r.ID, &r.Iss, &r.Sub, &r.Name, &r.StaffID, &r.Email, &r.Roles, &r.Groups,
		&r.Active, &r.InactiveReason, &deact, &r.FirstSeen, &r.LastSeen)
	if deact != nil {
		r.DeactivatedAt = *deact
	}
	return &r, err
}

func history(q querier, staffID string) []StatusChange {
	out := []StatusChange{}
	rows, err := q.Query(ctx, `SELECT staff_resource_id, at, status, source, reason FROM staff_status_history
		WHERE $1 = '' OR staff_resource_id = $1 ORDER BY id`, staffID)
	if err != nil {
		log.Printf("history: %v", err)
		return out
	}
	for rows.Next() {
		var c StatusChange
		var id string
		if rows.Scan(&id, &c.At, &c.Status, &c.Source, &c.Reason) == nil {
			out = append(out, c)
		}
	}
	return out
}

func addHistory(q querier, staffID, status, source, reason string) error {
	_, err := q.Exec(ctx, `INSERT INTO staff_status_history (staff_resource_id, status, source, reason) VALUES ($1, $2, $3, $4)`,
		staffID, status, source, reason)
	return err
}

// upsertStaff finds the row by iss+sub or creates it. The JWT is the source of
// truth for name and roles, so they are refreshed on every call.
func (s *store) upsertStaff(p *Principal) (*StaffResource, error) {
	var row *StaffResource
	err := s.tx(func(q pgx.Tx) error {
		var err error
		row, err = scanStaff(q.QueryRow(ctx, `UPDATE staff_resource SET name = $3, staff_id = $4, email = $5, roles = $6, groups = $7, last_seen = now()
			WHERE iss = $1 AND sub = $2 RETURNING `+staffCols, p.Iss, p.Sub, p.Name, p.StaffID, p.Email, nz(appRoles(p.Roles)), nz(p.Groups)))
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		row, err = scanStaff(q.QueryRow(ctx, `INSERT INTO staff_resource (id, iss, sub, name, staff_id, email, roles, groups)
			VALUES (`+newID+`, $2, $3, $4, $5, $6, $7, $8) RETURNING `+staffCols,
			"S", p.Iss, p.Sub, p.Name, p.StaffID, p.Email, nz(appRoles(p.Roles)), nz(p.Groups)))
		if err != nil {
			return err
		}
		return addHistory(q, row.ID, "active", "first login (JWT)", "")
	})
	if err != nil {
		return nil, err
	}
	row.History = history(s.db, row.ID)
	return row, nil
}

// deactivate takes effect for the API at once: check() reads active on every call.
func (s *store) deactivate(id, source, reason string) (*StaffResource, []string, error) {
	var row *StaffResource
	released := []string{}
	err := s.tx(func(q pgx.Tx) error {
		var err error
		row, err = scanStaff(q.QueryRow(ctx, `UPDATE staff_resource SET active = false, inactive_reason = $2, deactivated_at = now()
			WHERE id = $1 RETURNING `+staffCols, id, reason))
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("no such staff_resource")
		} else if err != nil {
			return err
		}
		if err := addHistory(q, id, "deactivated", source, reason); err != nil {
			return err
		}
		rows, err := q.Query(ctx, `UPDATE work_item SET status = 'submitted', claimed_by = NULL, claimed_by_name = ''
			WHERE claimed_by = $1 AND status = 'claimed' RETURNING id`, id)
		if err != nil {
			return err
		}
		if released, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		_, err = q.Exec(ctx, `UPDATE availability SET status = 'away', at = now() WHERE staff_resource_id = $1`, id)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	row.History = history(s.db, id)
	return row, released, nil
}

func (s *store) reactivate(id, source string) (*StaffResource, error) {
	var row *StaffResource
	err := s.tx(func(q pgx.Tx) error {
		var err error
		row, err = scanStaff(q.QueryRow(ctx, `UPDATE staff_resource SET active = true, inactive_reason = '', deactivated_at = NULL
			WHERE id = $1 RETURNING `+staffCols, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("no such staff_resource")
		} else if err != nil {
			return err
		}
		return addHistory(q, id, "active", source, "reactivated")
	})
	if err != nil {
		return nil, err
	}
	row.History = history(s.db, id)
	return row, nil
}

// ---------- customer ----------

const customerCols = `id, iss, sub, name, personal_number, first_seen, last_seen`

func scanCustomer(row pgx.Row) (*Customer, error) {
	var c Customer
	err := row.Scan(&c.ID, &c.Iss, &c.Sub, &c.Name, &c.PersonalNumber, &c.FirstSeen, &c.LastSeen)
	return &c, err
}

func (s *store) upsertCustomer(p *Principal) (*Customer, error) {
	c, err := scanCustomer(s.db.QueryRow(ctx, `UPDATE customer SET name = $3, personal_number = $4, last_seen = now()
		WHERE iss = $1 AND sub = $2 RETURNING `+customerCols, p.Iss, p.Sub, p.Name, p.PersonalNumber))
	if errors.Is(err, pgx.ErrNoRows) {
		c, err = scanCustomer(s.db.QueryRow(ctx, `INSERT INTO customer (id, iss, sub, name, personal_number)
			VALUES (`+newID+`, $2, $3, $4, $5)
			ON CONFLICT (iss, sub) DO UPDATE SET name = excluded.name, personal_number = excluded.personal_number, last_seen = now()
			RETURNING `+customerCols, "C", p.Iss, p.Sub, p.Name, p.PersonalNumber))
	}
	return c, err
}

func (s *store) customerByPN(pn string) *Customer {
	c, err := scanCustomer(s.db.QueryRow(ctx, `SELECT `+customerCols+` FROM customer WHERE personal_number = $1 ORDER BY id LIMIT 1`, pn))
	if err != nil {
		return nil
	}
	return c
}

func (s *store) allCustomers() []Customer {
	out := []Customer{}
	rows, err := s.db.Query(ctx, `SELECT `+customerCols+` FROM customer ORDER BY id`)
	if err != nil {
		log.Printf("customers: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		if c, err := scanCustomer(rows); err == nil {
			out = append(out, *c)
		}
	}
	return out
}

func (s *store) grant(st *StaffResource, c *Customer) {
	tag, err := s.db.Exec(ctx, `UPDATE support_grant SET uses = uses + 1, last_used = now()
		WHERE staff_resource_id = $1 AND customer_id = $2`, st.ID, c.ID)
	if err == nil && tag.RowsAffected() == 0 {
		_, err = s.db.Exec(ctx, `INSERT INTO support_grant (id, staff_resource_id, staff_name, customer_id, customer_name)
			VALUES (`+newID+`, $2, $3, $4, $5)
			ON CONFLICT (staff_resource_id, customer_id) DO UPDATE SET uses = support_grant.uses + 1, last_used = now()`,
			"G", st.ID, st.Name, c.ID, c.Name)
	}
	if err != nil {
		log.Printf("support_grant: %v", err)
	}
}

// ---------- work_item / case_approval / availability ----------

const itemCols = `id, customer_id, customer_name, amount, purpose, status, coalesce(claimed_by, ''), claimed_by_name, created_by, created_at`

func scanItem(row pgx.Row) (*WorkItem, error) {
	var it WorkItem
	err := row.Scan(&it.ID, &it.CustomerID, &it.CustomerName, &it.Amount, &it.Purpose, &it.Status,
		&it.ClaimedBy, &it.ClaimedByName, &it.CreatedBy, &it.CreatedAt)
	return &it, err
}

func (s *store) createItem(c *Customer, amount int, purpose, by string) (WorkItem, error) {
	if purpose == "" {
		purpose = "Consumer loan"
	}
	it, err := scanItem(s.db.QueryRow(ctx, `INSERT INTO work_item (id, customer_id, customer_name, amount, purpose, status, created_by)
		VALUES (`+newID+`, $2, $3, $4, $5, 'submitted', $6) RETURNING `+itemCols, "WI", c.ID, c.Name, amount, purpose, by))
	return *it, err
}

func (s *store) items(where string, args ...any) []WorkItem {
	out := []WorkItem{}
	rows, err := s.db.Query(ctx, `SELECT `+itemCols+` FROM work_item `+where+` ORDER BY created_at, id`, args...)
	if err != nil {
		log.Printf("work_item: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		if it, err := scanItem(rows); err == nil {
			out = append(out, *it)
		}
	}
	return out
}

func (s *store) itemsFor(customerID string) []WorkItem {
	return s.items(`WHERE customer_id = $1`, customerID)
}

func (s *store) allItems() []WorkItem { return s.items("") }

func (s *store) claim(id string, st *StaffResource) (WorkItem, error) {
	var it *WorkItem
	err := s.tx(func(q pgx.Tx) error {
		var err error
		it, err = scanItem(q.QueryRow(ctx, `SELECT `+itemCols+` FROM work_item WHERE id = $1 FOR UPDATE`, id))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return errors.New("no such work item")
		case err != nil:
			return err
		case it.Status == "approved":
			return errors.New("already approved")
		case it.ClaimedBy != "" && it.ClaimedBy != st.ID:
			return errors.New("already claimed by " + it.ClaimedByName)
		}
		it, err = scanItem(q.QueryRow(ctx, `UPDATE work_item SET status = 'claimed', claimed_by = $2, claimed_by_name = $3
			WHERE id = $1 RETURNING `+itemCols, id, st.ID, st.Name))
		return err
	})
	if err != nil {
		return WorkItem{}, err
	}
	return *it, nil
}

func (s *store) approve(id string, st *StaffResource) (CaseApproval, error) {
	var ap CaseApproval
	err := s.tx(func(q pgx.Tx) error {
		it, err := scanItem(q.QueryRow(ctx, `SELECT `+itemCols+` FROM work_item WHERE id = $1 FOR UPDATE`, id))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return errors.New("no such work item")
		case err != nil:
			return err
		case it.Status == "approved":
			return errors.New("already approved")
		case it.ClaimedBy != st.ID:
			return errors.New("claim the work item before approving")
		}
		if _, err := q.Exec(ctx, `UPDATE work_item SET status = 'approved' WHERE id = $1`, id); err != nil {
			return err
		}
		return q.QueryRow(ctx, `INSERT INTO case_approval (id, work_item_id, approved_by, approved_by_name)
			VALUES (`+newID+`, $2, $3, $4) RETURNING id, work_item_id, approved_by, approved_by_name, at`,
			"AP", id, st.ID, st.Name).Scan(&ap.ID, &ap.WorkItemID, &ap.ApprovedBy, &ap.ApprovedByName, &ap.At)
	})
	return ap, err
}

func (s *store) setAvailability(st *StaffResource, status string) (Availability, error) {
	var a Availability
	err := s.db.QueryRow(ctx, `INSERT INTO availability (staff_resource_id, staff_name, status) VALUES ($1, $2, $3)
		ON CONFLICT (staff_resource_id) DO UPDATE SET staff_name = excluded.staff_name, status = excluded.status, at = now()
		RETURNING staff_resource_id, staff_name, status, at`, st.ID, st.Name, status).Scan(&a.StaffID, &a.StaffName, &a.Status, &a.At)
	return a, err
}

// ---------- snapshot for the console and /admin/state ----------

type snapshot struct {
	StaffResource []StaffResource `json:"staff_resource"`
	Customers     []Customer      `json:"customer"`
	WorkItems     []WorkItem      `json:"work_item"`
	CaseApprovals []CaseApproval  `json:"case_approval"`
	SupportGrants []SupportGrant  `json:"support_grant"`
	Availability  []Availability  `json:"availability"`
	Log           []LogEntry      `json:"log"` // newest first, last 300
}

// collect runs a query and scans each row with scan.
func collect[T any](q querier, sql string, scan func(pgx.Rows, *T) error) []T {
	out := []T{}
	rows, err := q.Query(ctx, sql)
	if err != nil {
		log.Printf("snapshot: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var v T
		if err := scan(rows, &v); err != nil {
			log.Printf("snapshot scan: %v", err)
			continue
		}
		out = append(out, v)
	}
	return out
}

func (s *store) snapshot() snapshot {
	var sn snapshot
	hist := map[string][]StatusChange{}
	rows, err := s.db.Query(ctx, `SELECT staff_resource_id, at, status, source, reason FROM staff_status_history ORDER BY id`)
	if err == nil {
		for rows.Next() {
			var id string
			var c StatusChange
			if rows.Scan(&id, &c.At, &c.Status, &c.Source, &c.Reason) == nil {
				hist[id] = append(hist[id], c)
			}
		}
		rows.Close()
	}
	sn.StaffResource = collect(s.db, `SELECT `+staffCols+` FROM staff_resource ORDER BY first_seen, id`, func(r pgx.Rows, v *StaffResource) error {
		st, err := scanStaff(r)
		*v = *st
		v.History = hist[v.ID]
		return err
	})
	sn.Customers = s.allCustomers()
	sn.WorkItems = s.allItems()
	sn.CaseApprovals = collect(s.db, `SELECT id, work_item_id, approved_by, approved_by_name, at FROM case_approval ORDER BY at, id`, func(r pgx.Rows, v *CaseApproval) error {
		return r.Scan(&v.ID, &v.WorkItemID, &v.ApprovedBy, &v.ApprovedByName, &v.At)
	})
	sn.SupportGrants = collect(s.db, `SELECT id, staff_resource_id, staff_name, customer_id, customer_name, first_used, last_used, uses FROM support_grant ORDER BY first_used, id`, func(r pgx.Rows, v *SupportGrant) error {
		return r.Scan(&v.ID, &v.StaffID, &v.StaffName, &v.CustomerID, &v.CustomerName, &v.FirstUsed, &v.LastUsed, &v.Uses)
	})
	sn.Availability = collect(s.db, `SELECT staff_resource_id, staff_name, status, at FROM availability ORDER BY at`, func(r pgx.Rows, v *Availability) error {
		return r.Scan(&v.StaffID, &v.StaffName, &v.Status, &v.At)
	})
	sn.Log = collect(s.db, `SELECT at, method, path, status, kind, name, person_id, roles, groups, client, act_as_name, act_as_id, reason
		FROM api_log ORDER BY id DESC LIMIT 300`, func(r pgx.Rows, v *LogEntry) error {
		return r.Scan(&v.At, &v.Method, &v.Path, &v.Status, &v.Kind, &v.Name, &v.PersonID, &v.Roles, &v.Groups, &v.Client, &v.ActAsName, &v.ActAsID, &v.Reason)
	})
	return sn
}
