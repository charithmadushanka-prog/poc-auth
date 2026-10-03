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
| Keycloak admin panel (login: `admin` / `admin`) | http://localhost:8080/admin/ |

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

### Keycloak admin console

| | |
|---|---|
| URL | http://localhost:8080/admin (`./run.sh` opens it for you) |
| Username | `admin` |
| Password | `admin` |
| Realm to sign in to | `master`, the default. Just enter the username and password. |

**How to log in**
1. Open http://localhost:8080/admin. It redirects to the Keycloak sign-in page titled **"Sign in to your account"** (realm *Keycloak / master*).
2. Enter username `admin`, password `admin`, then **Sign In**.
3. Use the realm dropdown at the top left to switch between the four realms:
   - `staff`: staff broker realm (Google SAML upstream, roles advisor / underwriter / support)
   - `customer`: customer broker realm (BankID upstream)
   - `mock-google`: the fake Google Workspace, with its test staff users and groups
   - `mock-bankid`: the fake BankID, with its test customers

**What to look at**
- **Events** (left menu, in a realm). The **User events** tab has every login, logout, token request and failed attempt (e.g. `LOGIN_ERROR user_not_found`). The **Admin events** tab has the leaver check ending a session. Kept 24 h, until Keycloak restarts.
- **Users**. In `staff`/`customer` these are the people who have logged in through the broker. Open one to see **Attributes** (`staff_id`, `google_groups`, `personal_number`, `amr`), **Role mapping**, **Identity provider links** and **Sessions**.
- **Identity providers** → `google-saml` / `bankid`, then **Mappers**. This is where Google groups become roles and claims are mapped.
- **Clients** → e.g. `backoffice-bff` → **Client scopes** → *Evaluate*. Shows the exact token a user would get.

**Good to know**
- `admin` / `admin` works **only** here. It does not work on the mock BankID or mock Google login screens: those need the demo users above, password `test`. The browser may autofill `admin` on those screens because they run on the same address (`localhost:8080`). Clear it if so.
- The admin login is set in `keycloak/Dockerfile` (`KC_BOOTSTRAP_ADMIN_USERNAME` / `KC_BOOTSTRAP_ADMIN_PASSWORD`). Local demo only.
- Changes you make in the console are lost when Keycloak restarts. To keep a change, put it in `keycloak/realms/*.json`.

### Tips
- **Easiest way to log in:** use the buttons on each portal, e.g. "Log in with BankID as Sven Svensson" or "Ulf Underwriter · underwriter". They fill in the username, so you only type `test`.
- **"Invalid username or password"** almost always means the browser autofilled `admin`. The mock screens run on `localhost:8080`, the same address as the admin console. Clear the field and type the demo username.
- **Impersonation:** first log in once as `sven` or `lisa`, so the customer exists in the API. Then on the customer portal choose **Sign in as Sara Support (allowed)**, and pick the customer.
- **Log out** is at the top right of both portals.

## Real Google Workspace (optional test integration)

Google has no public test identity provider, so testing against real Google needs a **custom SAML app in our Google Workspace**. It sits next to the mock: the Backoffice then shows an extra button, **"Sign in with real Google Workspace (TEST app)"**. The services don't change at all.

### 1. Ask a Google Workspace super admin to create the app
In the Google Admin console, go to **Apps → Web and mobile apps → Add app → Add custom SAML app**.

| Step in Google | Value |
|---|---|
| App name | `Entra auth POC (test)` |
| Google Identity Provider details | **Download metadata** → send us the XML file |
| ACS URL | `http://localhost:8080/realms/staff/broker/google-real/endpoint` |
| Entity ID | `http://localhost:8080/realms/staff` |
| Signed response | ✅ tick it |
| Name ID format / Name ID | `EMAIL` / *Basic Information → Primary email* |

Attribute mapping (Google attribute → app attribute):

| Google directory attribute | App attribute |
|---|---|
| Basic Information → First name | `firstName` |
| Basic Information → Last name | `lastName` |
| Basic Information → Primary email | `email` |
| Employee details → Employee ID | `staffId` |
| **Group membership**: pick the advisor / underwriter / support groups | `groups` |

Then **User access → ON** for a test group or organisational unit only.

> If Google refuses an `http://localhost` ACS URL, Keycloak needs HTTPS for this test (a local certificate or a tunnel). We haven't been able to confirm this without an app.

### 2. Connect it
```bash
cp ~/Downloads/GoogleIDPMetadata.xml google-saml/google-idp-metadata.xml   # git-ignored
./run.sh
```
`run.sh` notices the file and runs `cmd/google-saml-setup`. That tool:
- adds the identity provider `google-real` to the staff realm, using the SSO URL and signing certificate from the metadata, with **signature validation on**
- adds the same mappers as the mock: names, email, staff ID, groups → `google_groups`, and groups → roles `advisor` / `underwriter` / `support`

Run the tool on its own with `go run ./cmd/google-saml-setup -h` to see every flag.

### 3. Check and adjust
- Log in with the new button, then look in Keycloak at **staff → Users → (you) → Attributes**. `google_groups` shows the exact values Google sends. If they differ from `advisors` / `underwriters` / `support`, set them with, for example, `GOOGLE_SAML_FLAGS="-group-advisor=Advisors -group-underwriter=Underwriters" ./run.sh`.
- **MFA:** Google doesn't send `amr`. By default the setup adds `amr=google-2sv`, which is only correct if Workspace **enforces 2-step verification** for these users. Pass `-assume-2sv=false` to see the API refuse the login ("MFA required") instead.
- **Already logged in through the mock with the same email?** Keycloak asks to link the accounts. Use a person who hasn't used the mock, or confirm the link.
- **Automated check:** `E2E_REAL_GOOGLE_USER=you@company E2E_REAL_GOOGLE_PASSWORD=... go run ./cmd/e2e` runs the real-Google login as well. This only works for accounts without an extra Google 2SV prompt, so normally test by hand.

**How this was tested without a Google app:** the setup tool was run against the mock Google's own SAML metadata, with signature validation on and a staff user who had never logged in. The login worked end to end. With a wrong signing certificate it was rejected (`invalid_signature`).

**Note:** with real Google, real staff names and emails pass through the local Keycloak (in memory only). Use test accounts where possible.

## Demo users for automated tests only

These are used by `cmd/e2e` (direct API calls). You don't need them in the UI.

| Realm / client | User | Purpose |
|---|---|---|
| staff realm, local user | `breakglass` / `test` | a valid token without MFA, which the API must refuse |
| mock Google, user | `gina.newhire` / `test` | a staff member who has never logged in, used to test the real-Google path against the mock |
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

- **Google Workspace**: see "Real Google Workspace (optional test integration)" above. A metadata file plus `./run.sh` connects it, with signature validation on.
- **BankID**: in realm `customer`, point IdP `bankid` at Scrive's **test** OIDC endpoints and use Scrive test credentials. Map the personal-number claim Scrive actually issues to `personal_number`.
- Nothing changes in the BFFs or the API. They only see broker JWTs.
