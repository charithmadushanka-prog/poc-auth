-- Business tables for the Entra Origination API (POC). Applied on every start;
-- every statement is idempotent. Database: poc_auth.

CREATE SEQUENCE IF NOT EXISTS id_seq;

CREATE TABLE IF NOT EXISTS staff_resource (
    id              text PRIMARY KEY,
    iss             text NOT NULL,
    sub             text NOT NULL,
    name            text NOT NULL DEFAULT '',
    staff_id        text NOT NULL DEFAULT '',
    email           text NOT NULL DEFAULT '',
    roles           text[] NOT NULL DEFAULT '{}',
    groups          text[] NOT NULL DEFAULT '{}',
    active          boolean NOT NULL DEFAULT true,
    inactive_reason text NOT NULL DEFAULT '',
    deactivated_at  timestamptz,
    first_seen      timestamptz NOT NULL DEFAULT now(),
    last_seen       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (iss, sub)
);

-- Append-only: every activation and deactivation, with who/what caused it.
CREATE TABLE IF NOT EXISTS staff_status_history (
    id                bigserial PRIMARY KEY,
    staff_resource_id text NOT NULL REFERENCES staff_resource(id),
    at                timestamptz NOT NULL DEFAULT now(),
    status            text NOT NULL CHECK (status IN ('active', 'deactivated')),
    source            text NOT NULL,
    reason            text NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS customer (
    id              text PRIMARY KEY,
    iss             text NOT NULL,
    sub             text NOT NULL,
    name            text NOT NULL DEFAULT '',
    personal_number text NOT NULL DEFAULT '',
    first_seen      timestamptz NOT NULL DEFAULT now(),
    last_seen       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (iss, sub)
);

CREATE TABLE IF NOT EXISTS work_item (
    id              text PRIMARY KEY,
    customer_id     text NOT NULL REFERENCES customer(id),
    customer_name   text NOT NULL,
    amount          integer NOT NULL CHECK (amount > 0),
    purpose         text NOT NULL,
    status          text NOT NULL CHECK (status IN ('submitted', 'claimed', 'approved')),
    claimed_by      text REFERENCES staff_resource(id),
    claimed_by_name text NOT NULL DEFAULT '',
    created_by      text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS case_approval (
    id               text PRIMARY KEY,
    work_item_id     text NOT NULL REFERENCES work_item(id),
    approved_by      text NOT NULL REFERENCES staff_resource(id),
    approved_by_name text NOT NULL,
    at               timestamptz NOT NULL DEFAULT now()
);

-- Impersonation audit: which staff member acted as which customer.
CREATE TABLE IF NOT EXISTS support_grant (
    id                text PRIMARY KEY,
    staff_resource_id text NOT NULL REFERENCES staff_resource(id),
    staff_name        text NOT NULL,
    customer_id       text NOT NULL REFERENCES customer(id),
    customer_name     text NOT NULL,
    first_used        timestamptz NOT NULL DEFAULT now(),
    last_used         timestamptz NOT NULL DEFAULT now(),
    uses              integer NOT NULL DEFAULT 1,
    UNIQUE (staff_resource_id, customer_id)
);

CREATE TABLE IF NOT EXISTS availability (
    staff_resource_id text PRIMARY KEY REFERENCES staff_resource(id),
    staff_name        text NOT NULL,
    status            text NOT NULL CHECK (status IN ('available', 'away')),
    at                timestamptz NOT NULL DEFAULT now()
);

-- People log: every API call. Personal numbers are stored masked.
CREATE TABLE IF NOT EXISTS api_log (
    id          bigserial PRIMARY KEY,
    at          timestamptz NOT NULL DEFAULT now(),
    method      text NOT NULL,
    path        text NOT NULL,
    status      integer NOT NULL,
    kind        text NOT NULL,
    name        text NOT NULL DEFAULT '',
    person_id   text NOT NULL DEFAULT '',
    roles       text[] NOT NULL DEFAULT '{}',
    groups      text[] NOT NULL DEFAULT '{}',
    client      text NOT NULL DEFAULT '',
    act_as_name text NOT NULL DEFAULT '',
    act_as_id   text NOT NULL DEFAULT '',
    reason      text NOT NULL DEFAULT ''
);

-- Never delete people; never rewrite history or the log (audit and compliance).
CREATE OR REPLACE FUNCTION refuse_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% on % is not allowed: %', TG_OP, TG_TABLE_NAME, TG_ARGV[0];
END $$;

CREATE OR REPLACE TRIGGER staff_resource_no_delete BEFORE DELETE ON staff_resource
    FOR EACH ROW EXECUTE FUNCTION refuse_change('users are never deleted; deactivate instead');
CREATE OR REPLACE TRIGGER customer_no_delete BEFORE DELETE ON customer
    FOR EACH ROW EXECUTE FUNCTION refuse_change('customers are never deleted');
CREATE OR REPLACE TRIGGER staff_status_history_append_only BEFORE UPDATE OR DELETE ON staff_status_history
    FOR EACH ROW EXECUTE FUNCTION refuse_change('status history is append-only');
CREATE OR REPLACE TRIGGER api_log_append_only BEFORE UPDATE OR DELETE ON api_log
    FOR EACH ROW EXECUTE FUNCTION refuse_change('the people log is append-only');

-- Demo seed (once).
INSERT INTO customer (id, iss, sub, name, personal_number)
VALUES ('C-0001', 'seed', 'seed', 'Seed Customer (demo data)', '197001019990')
ON CONFLICT DO NOTHING;
INSERT INTO work_item (id, customer_id, customer_name, amount, purpose, status, created_by)
VALUES ('WI-0002', 'C-0001', 'Seed Customer (demo data)', 150000, 'Car loan (seeded)', 'submitted', 'seed'),
       ('WI-0003', 'C-0001', 'Seed Customer (demo data)', 60000, 'Renovation (seeded)', 'submitted', 'seed')
ON CONFLICT DO NOTHING;
SELECT setval('id_seq', greatest(3, (SELECT last_value FROM id_seq)));
