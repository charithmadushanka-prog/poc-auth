# poc-auth — local demo of the Entra auth architecture

Runs the whole login setup on your machine with mocks: Keycloak as the **auth broker**, mock upstream IdPs for **Google Workspace (SAML)** and **BankID via Scrive (OIDC)**, two Go **BFFs** with small UIs, and the **Entra Origination API** with a console that shows who is calling it.

> Local demo only (TEST). In-memory data, HTTP, fixed test secrets, an unauthenticated demo console. Nothing here is fit for production.

## Quick start

**Prerequisites:** Docker Desktop running. Go 1.24+ is optional (only used for the tests).

```bash
cd poc-auth
./run.sh
```

The first run takes about 1–2 minutes because it builds the images. The script:
1. builds and starts Keycloak and the three Go services
2. waits until they are all healthy
3. opens these pages in your browser:

| What | URL |
|---|---|
| Customer portal | http://localhost:3000 |
| Backoffice portal (staff) | http://localhost:3001 |
| Entra Origination console (people log) | http://localhost:4000 |
| Keycloak admin, events page | http://localhost:8080/admin/master/console/#/staff/events |

Other commands:

```bash
./run.sh test     # start, run the 94 end-to-end checks, then open the UIs
./run.sh logs     # follow logs of all services
./run.sh down     # stop and remove everything
```

Keycloak and the API keep everything in memory, so every restart begins from a clean state.

## Usernames and passwords

**All demo users have the password `test`.** They are fictitious test users.

### Customers: mock BankID screen (Customer portal, http://localhost:3000)

| Username | Password | Signed in as |
|---|---|---|
| `sven` | `test` | Sven Svensson, personal number 199001019999 |
| `lisa` | `test` | Lisa Larsson, personal number 198505059998 |

### Staff: mock Google Workspace screen (Backoffice, http://localhost:3001)

| Username | Password | Name / staff ID | Google group → role | What they can do |
|---|---|---|---|---|
| `anna.advisor` | `test` | Anna Advisor, E1001 | advisors → `advisor` | view and claim cases, can't approve |
| `ulf.underwriter` | `test` | Ulf Underwriter, E1002 | advisors + underwriters → `advisor`, `underwriter` | claim and approve cases |
| `sara.support` | `test` | Sara Support, E1003 | support → `support` | view cases; impersonate customers in the customer portal |
| `lars.leaver` | `test` | Lars Leaver, E1004 | advisors → `advisor` | use for the leaver demo (suspend in the console) |

### Keycloak admin console (http://localhost:8080/admin)

| Username | Password | Note |
|---|---|---|
| `admin` | `admin` | **Only** for the admin console. It does not work on the mock BankID or Google screens. |

### Tips
- **Easiest way to log in:** use the buttons on each portal, e.g. "Log in with BankID as Sven Svensson" or "Ulf Underwriter · underwriter". They fill in the username, so you only type `test`.
- **"Invalid username or password"** almost always means the browser autofilled `admin`. The mock screens run on `localhost:8080`, the same address as the admin console. Clear the field and type the demo username.
- **Impersonation:** first log in once as `sven` or `lisa`, so the customer exists in the API. Then on the customer portal choose **Sign in as Sara Support (allowed)**, and pick the customer.
- **Log out** is at the top right of both portals.

## Demo users for automated tests only

These are used by `cmd/e2e` (direct API calls). You don't need them in the UI.

| Realm / client | User | Purpose |
|---|---|---|
| staff realm, local user | `breakglass` / `test` | a valid token without MFA, which the API must refuse |
| clients `test-direct`, `test-legacy-typ`, `test-no-aud`, `test-expiring` | secret `test-secret` | produce tokens with a wrong `typ`, missing `aud`, or quick expiry |

## How it maps to the diagram

```
Customer browser ─cookie─▶ Customer BFF ──OIDC code+PKCE──▶ Keycloak realm "customer" ──OIDC──▶ realm "mock-bankid"   (stands in for BankID via Scrive)
Staff browser    ─cookie─▶ Backoffice BFF ─OIDC code+PKCE─▶ Keycloak realm "staff"    ──SAML──▶ realm "mock-google"   (stands in for Google Workspace)
Both BFFs ──API call + JWT──▶ Entra Origination API ──(JWKS, cached)──▶ Keycloak
Origination leaver check ──admin API: end user sessions──▶ Keycloak realm "staff"
```

| Diagram box | Where |
|---|---|
| Auth broker, two realms | `keycloak/realms/staff-realm.json`, `customer-realm.json` |
| Google Workspace SAML: groups, name, staff ID | `keycloak/realms/mock-google-realm.json`. Groups `advisors` / `underwriters` / `support` become roles `advisor` / `underwriter` / `support` (SAML attribute→role mappers). `staffId`→`staff_id` claim, groups→`groups` claim |
| BankID via Scrive | `keycloak/realms/mock-bankid-realm.json`: `personal_number` claim, `amr=bankid` |
| Mock login (upstream) | the two `mock-*` realms (the test IdPs) |
| Customer BFF / Backoffice BFF: holds tokens, cookie + CSRF | `cmd/customer-bff`, `cmd/backoffice-bff`, `internal/web`, `internal/oidc`. Tokens stay server-side; the browser gets only an HttpOnly session cookie; every POST checks a session CSRF token plus `Origin` |
| 1 · Token check middleware | `internal/jwtx` + `check()` in `cmd/origination-api/main.go`: RS256 signature against cached JWKS (refetched on unknown `kid`), `typ` = `at+jwt`, alg allowlist (no `none`, no HS*), `iss`, `aud` = `entra-origination-api`, `exp`/`nbf`, scope `origination`, MFA via `amr` (`bankid` / `google-2sv`). It never calls Google or BankID |
| 2 · Identify the person | same file: from the JWT (`iss`+`sub`, name, `staff_id` / `personal_number`, roles), find-or-create a `staff_resource` / `customer` row; inactive staff are refused |
| Business tables | `cmd/origination-api/store.go` (in-memory): `staff_resource`, `work_item`, `case_approval`, `support_grant`, `availability` (+ `customer`) |
| Optional directory leaver check | console button **Suspend in directory**: marks the person inactive, releases their claimed cases, and ends their Keycloak sessions through the admin API. There's no Google Directory API call; the button stands in for it |

## Impersonation (customer portal, TEST only)

On the customer portal, choose **Sign in as staff to impersonate**. This logs in through the **staff** realm with a separate client, `customer-bff-impersonation`, which adds scope `impersonate-customer`. Next you pick a customer. After that the BFF calls the API with the **staff** JWT plus the header `X-Act-As-Customer: <personal number>`.

The API allows this only when all of the following hold:
- the token is a staff token
- `azp` = `customer-bff-impersonation`
- the scope `impersonate-customer` is present
- the role `support` is present

Every call is logged as **STAFF AS CUSTOMER** and counted in `support_grant`. The token's subject stays the staff member, so the audit trail always names the real person.

The picker shows only customers the API has already seen. After a restart, log in once as `sven` or `lisa` first.

## Demo script (≈5 min)

1. Customer portal → log in as `sven` → **Apply**. The console shows a **CUSTOMER** row with the masked personal number.
2. Backoffice → `anna.advisor` → **Claim** works, **Approve** is refused (403, needs underwriter). The console shows **STAFF** with role and group.
3. Log out, then sign in as `ulf.underwriter` → claim Sven's case → **Approve**. A row appears in `case_approval`.
4. Customer portal → **Sign in as staff to impersonate** → `sara.support` → **Act as** Sven → apply. The console shows **STAFF AS CUSTOMER**. Try the same with `anna.advisor`: refused.
5. Backoffice as `lars.leaver` → claim a case. Console → **Suspend in directory**. The case is released, Keycloak sessions end, and Lars's next click is refused (`inactive`) even though his JWT hasn't expired.

## What the e2e test covers

`go run ./cmd/e2e` (stack must be running) drives the real redirects, Keycloak forms and SAML auto-post forms with a cookie-jar client:
- customer and staff claims
- CSRF
- role rules (claim / approve / view)
- impersonation allowed and refused
- leaver flow, including the Keycloak session count dropping to 0
- token rejections: no token, garbage, `alg:none`, `HS256`, tampered payload, `typ: JWT`, missing `aud`, no MFA, expired
- people-log content and masking
- logout from all three apps (incl. SAML single logout) lands back on each app's sign-in screen

## Going from mocks to real test environments

- **Google Workspace**: in realm `staff`, point IdP `google-saml` at the Google SAML app's SSO URL and certificate. Turn on `validateSignature`. Map Google's group attribute to the same role mappers.
- **BankID**: in realm `customer`, point IdP `bankid` at Scrive's **test** OIDC endpoints and use Scrive test credentials. Map the personal-number claim Scrive actually issues to `personal_number`.
- Nothing changes in the BFFs or the API. They only see broker JWTs.

## Compliance notes (for review, not legal advice)

- **Impersonation** means staff access customer data. It must not exist in production unless compliance approves it: purpose limitation and logging under GDPR, plus access control under FFFS 2014:1 and DORA (EU 2022/2554) ICT access management. The demo already keeps the staff identity in the token and records every use.
- **Personal numbers** are personal data. The people log masks them; the test persons here are fictitious.
- **Leaver handling**: ending sessions and refusing inactive staff supports DORA access-revocation expectations. The real Google Directory sync would need its own read-only service account and a DPIA check.
- The demo makes **no credit decisions**. If scoring or risk logic is added later, it is likely high-risk AI under EU AI Act Annex III §5 and needs CTO + compliance review.
