# Sandbox and Security Design

## 1. Security Domains

Platform MySQL and validation MySQL are the only two MySQL security domains. They are separate
instances or isolated container pools. validation MySQL is always platform-controlled test/staging
infrastructure - the platform never points generated SQL or a Target Agent at a real production
database. Platform credentials are never mounted into the Worker's validation MySQL or Agent database connections, or sent to the Agent.

Inside validation MySQL, each Dataset Revision gets one validated execution schema, created once when
the Dataset Revision is generated/validated. Validation reads and loads it; no Run runs against it.
Each Test Run gets its own database instead (`run_<ULID>`, section 3.2), on the **Agent's own database
server**: the Worker executes the Dataset Revision's own `schema_sql` and `seed_sql` in it. Runs of different Test Cases, and repeated Runs
of one Test Case, therefore never share data, and no reset is needed. The platform creates no MySQL
accounts: generation-time validation uses the validation MySQL administrator connection (section 1.1), and Runs
use the account a tester entered on the Agent (section 3.3). Dataset execution schemas (`dataset_*`) on validation MySQL are deleted manually by an operator;
Run databases are dropped by the Worker after the Run and by a periodic sweep for leftovers (section 6). Generated validation SQL can only address the execution schema of its own
Dataset Revision, and a Run's Agent and postcondition SQL can only address that Run's database. None
of them can address Platform MySQL or another Run's database.

A Database Profile optionally names a durable reference schema inside validation MySQL - for example
a hand-loaded sample business schema a team wants to reuse across many Generation Jobs. MVP
supports only MySQL. Future engines implement the same Profile and Snapshot Adapter contracts
without leaking driver types into domain packages. One Database Profile may back many Dataset
Revisions over time.

When `generate_dataset=false`, the Snapshot Adapter reads the Database Profile's reference schema
with read-only credentials and creates an immutable Dataset Revision, which is then materialized
into its own execution schema the same way an AI-generated Dataset Revision is. Generated SQL and
the Target Agent never execute against the reference schema itself: validation runs only against the
per-dataset execution schema, and each Run runs only against a Run database materialized from it.

The adapter reads the reference database inside one read-only, repeatable-read transaction on a
single connection, with a per-statement limit of 5 seconds and the profile's
`snapshot_timeout_seconds` over the whole read. Schema is produced from `SHOW CREATE TABLE` with
table options removed and tables ordered by foreign-key dependency. Rows are copied as literal
INSERT statements in primary-key order, binary columns as hexadecimal literals. Reference schemas
that use partitions, generated columns, CHECK constraints, full-text or spatial indexes, spatial
columns, tables without a primary key, cross-database or self-referencing foreign keys, or more than
20 tables or 10,000 rows fail with an explicit error and are never rewritten.

The model that completes the business fields of a snapshot Dataset (constraints, difficulty profile,
coverage tags) receives table, column, and key structure only. The Dataset-stage Semantic Critic gets
no rows in its prompt: it reads them page by page from the execution schema with `read_dataset_rows`,
under the read budget and compaction rules in technical/02 section 8.1 and technical/03 section 9.
Snapshot rows are synthetic test data. They go only to the model roles the platform configures for
generation and review, either a third-party API or a local Ollama, and never into a prompt for the
Agent under test, which reaches only its own Run database, through the credential described in section 3.3. If the
Critic's context budget is exceeded, dataset validation fails with `CONTEXT_BUDGET_EXCEEDED`; rows
are never truncated.

Credentials:

| Credential | Scope | Purpose |
|---|---|---|
| Validation MySQL administrator (one per registered validation MySQL, section 1.1) | that instance; the admin password is held as a secret | create per-dataset execution schemas while a Dataset is generated; apply and validate schema and seed to them. The platform never creates, alters, or drops accounts |
| Agent database account | that Agent's database server | create, load and drop `run_<ULID>` for the Agent's Runs; entered by a tester on the Agent and stored encrypted (sections 3.3 and 8.1); never sent to the Agent |

### 1.1 Multiple validation MySQL instances

Each registered validation MySQL (`validation_mysql`, architecture 16.5) is an independent platform-controlled MySQL instance with its own
administrator account. It is used only while a Dataset is **generated**. The administrator password is a secret entered on the Credentials page; the platform reads
it only to open connections:

- A Dataset Revision may be bound to one or more validation MySQL instances. Each binding (`dataset_revision_validation_db`) records that
  revision's execution schema on one instance. Validation provisions and checks the revision on every instance listed in
  the generation request, and publication writes all bindings in one transaction. The content is identical on every
  instance. Bindings are fixed at publication.
- A Run never uses a validation MySQL: it executes the Dataset's SQL on the Agent's own database (section 3.2), and instances never share data.
- The platform never sends database credentials to the Agent. It seeds the Run database with the account configured on the Agent and names the Run database in the prompt.
- Registration rejects the Platform MySQL endpoint (`VALIDATION_DB_SAME_AS_PLATFORM`), and the Worker checks again when it
  connects. Platform credentials are never offered to a validation MySQL.
- `VERIFY_IDENTITY` uses the CA certificate stored with the validation MySQL in Platform MySQL. The certificate is public
  material, not a secret. `DISABLED_LOCAL_ONLY` stays limited to isolated local networks, as section 4 requires.

## 2. Identifier Construction

Database names are constructed from the Dataset Revision ULID or the Run ULID. Application code derives
them, never from a test case name, prompt text, or other model output:

```text
database     = dataset_<lowercase Dataset Revision ULID>
run_database = run_<lowercase Run ULID>
```

The implementation still quotes identifiers with the MySQL driver-safe identifier function and
rejects anything that fails the fixed regex before issuing administrative SQL. Each Run ID names its own
database, so every Run - including repeated Runs of one Test Case - has its own database, and Runs never
share data or collide on names. No account name is derived, because the platform creates no accounts.

## 3. Provisioning

### 3.1 Dataset provisioning (once per Dataset Revision)

When a Dataset Revision is generated/validated, the Worker creates its execution schema through the
validation MySQL administrator connection, applies `schema.sql` and `seed.sql`, and checks the
constraints. This happens once per Dataset Revision, for validation. Later Runs do not reuse it: each
Run gets its own database (section 3.2).

```sql
CREATE DATABASE `<database>`
  CHARACTER SET utf8mb4
  COLLATE utf8mb4_0900_ai_ci;
```

The Worker applies only SQL already accepted by the AST Validator. If a step after `CREATE DATABASE`
fails or is interrupted, the database stays in validation MySQL for manual deletion (section 6). A retry of
a stage that already stored its execution record reuses that schema instead of creating a new one.

### 3.2 Run provisioning (every Test Run)

Each Test Run gets a dedicated database, created from the Dataset Revision's validated schema and
seed statements. In order, the Worker:

1. creates `run_<ULID>` (`CREATE DATABASE`, `utf8mb4`);
2. executes the Dataset's `schema_sql` and then `seed_sql` in it, through the account configured on the Agent
   (section 3.3), on the Agent's own database server.

The Runner first drops any database with the Run's derived name, because a Run ID is never reused and an existing one can only be a leftover of an earlier attempt of the same Run. Provisioning itself still refuses an existing database without any change. A retry never reuses a Run ID: it is a new Run with its own ID and
database, and a Run whose database already exists ends as `INFRA_ERROR` (technical/01 section 7.1).
Runs of the same Test Case never share a database, because each Run ID names its own. A failure after
step 1 is cleaned up in `finally` (and by the leftover sweep if the Worker dies).

### 3.3 Agent database account

The platform sends no database credential to the Agent. The Agent under test uses its own database access through its own Tools; the platform only tells it the Run database name in the prompt text.

The account the **platform** uses is the one configured on the Agent (`agent.database_profile_id`). It is unique to that Agent and is used only to prepare data: create `run_<ULID>`, execute the Dataset's `schema_sql` and `seed_sql`, read the state before and after the Run, and drop the database. It therefore needs `CREATE`, `DROP` and DML on `run_%` databases. The platform never creates accounts and never grants privileges.

- Creating an Agent with a database runs a probe with this account (connect, `CREATE DATABASE atp_probe_*`, create a table, insert a row, drop the database). A failure refuses the Agent with `AGENT_DATABASE_CHECK_FAILED` and the reason.
- The probe and the Run use only names the platform derives itself (`atp_probe_<id>`, `run_<ULID>`); the cleanup sweep drops only names matching `run_<26 chars>` whose Run is no longer active.
- Any account the Agent's own Tools use is configured by the tester outside the platform. Isolation between Runs rests on each Run ID naming its own database and the Runner giving the Agent only that name; the test data is virtual, so the exposure is test integrity rather than disclosure.

## 4. Tool Transport

There is no platform Tool Gateway and no Run Tool token. The Agent under test uses its own Tools and its own database access, reports what it did as structured Tool Call
Records (`AGENT_REPORTED`, technical/04 section 4), and receives only the Run database **name** in the prompt. The platform verifies the outcome independently by reading the Run
database itself: before/after state diff, postconditions, mutation scope.

`DISABLED_LOCAL_ONLY` TLS mode is allowed only when the platform and the database server share the same loopback or isolated container network. Any routed environment requires
`VERIFY_IDENTITY`.

## 5. Statement Safety

Statements the platform itself runs are always validated before they run, with the SQL allowlist (architecture 11.2):

- Dataset `schema_sql`: `CREATE TABLE` and `CREATE INDEX` only; every table has an explicit primary key; no triggers, views, routines, events, partitions or cross-database names.
- Dataset `seed_sql`: explicit-column `INSERT` with literal values only, bounded rows per statement and in total, no nondeterministic functions.
- Assertions (preconditions and postconditions): exactly one read-only `SELECT` each, with a deterministic `ORDER BY` when more than one row can come back, a bounded canonical result and a statement timeout.
- Identifiers are never built from model output: database names are derived from ULIDs (section 2), and the cleanup sweep and the privilege probe only drop names they can match with a strict pattern.

Nothing the Agent sends to its own database passes through the platform, so the platform cannot reject or limit those statements; the Mutation Scope check on the resulting state is the control.

## 6. Cleanup

Per-Run cleanup is idempotent and removes only what the platform created:

1. Close the Runner's database pools for the Run.
2. Drop `run_<ULID>` on the Agent's database server.
3. Persist `environment_cleaned_at`.

A cleanup failure leaves the database for the leftover sweep, which retries it every 10 minutes.

The platform never creates or drops accounts. It drops only databases it can name itself: `run_<ULID>` after a Run, `atp_probe_<id>` after
the Agent database check, and leftover `run_<26 chars>` databases of Runs that are no longer QUEUED, PREPARING or RUNNING (the Worker sweeps every
Agent database server every 10 minutes). Databases of in-flight or unknown Runs, and anything with another name, are never touched. Dataset execution
schemas (`dataset_*`) on validation MySQL are still deleted manually by an operator. Automated tests that use a validation MySQL use unique names so that reruns do not collide.

## 7. Network Policy

- Platform MySQL accepts traffic only from API/Worker network identities.
- validation MySQL accepts administrative traffic only from the Worker.
- Each Agent database server accepts the platform's Worker account only from the Worker network, and the Agent's own account only from the Agent's network.
- validation MySQL has no route to Platform MySQL. It is platform-controlled test infrastructure and
  is never a production database or reachable from a production network.
- Ollama is reachable only from the Model Gateway process.
- OpenAI egress is restricted to the configured HTTPS endpoint.

## 8. Secrets and Logs

Persist only secret names and encrypted values. Test-environment credentials are stored encrypted and
written write-only (section 8.1). The only secrets read from the environment are the platform database DSN and the
encryption keys (architecture 16.4); Compose or Kubernetes injects them from their secret stores. Values are never
logged or returned by the API. HTTP request logging must redact
`Authorization`, `access_token`, `password`, Provider API keys, Agent credentials, and common
secret patterns inside generated data. Structured Tool evidence is redacted before storage and
again before Judge input.

### 8.1 Stored Test-Environment Credentials and Key Rotation

Testers enter credentials on the Secrets page or with `PUT /api/v1/secrets/{name}`. They are stored in the `secret`
table (migration `0007_secret.sql`):

- Encryption: AES-256-GCM. The current key id comes from `SECRET_ENCRYPTION_KEY_ID`. The process can read every key
  listed in `SECRET_ENCRYPTION_KEY_IDS`; each key is the base64 of 32 random bytes in `ATP_ENCRYPTION_KEY_<ID>` (ID
  upper-cased, `-` replaced by `_`). Keys are never stored in the database, logged, or returned.
- Key ids match `^[a-z0-9][a-z0-9-]{0,31}$`. The list must include the current id, must not repeat an id, and holds
  at most 8 ids. Startup fails when a listed key is missing or malformed; the error names the variable, never the value.
- Each write draws a new random 12-byte nonce and records `secret.key_id`. The secret name is bound as additional
  authenticated data, so a ciphertext cannot be moved to another name.
- Rotation: add a key, make it the current id, keep the old id in the list, restart, then run
  `go run ./cmd/secrets-rewrap` (it reads `PLATFORM_MYSQL_DSN` and the keyring). The command re-encrypts every secret
  whose `key_id` is not current, using a compare-and-set update so a concurrent write is never overwritten. It prints
  how many secrets remain under each key id. Remove an old key only after its count is zero. Rotation changes no value
  and does not require re-entry.
- Names match `^[a-z0-9][a-z0-9-]{0,127}$`. Values are 1 to 4096 bytes, must not be blank, and are stored exactly
  as entered.
- Write-only: `PUT /api/v1/secrets/{name}` replaces the value and returns metadata only (name and timestamps).
  `GET /api/v1/secrets` lists metadata only. No endpoint returns a value, and no error message, log line, or
  validation message contains one.
- Only the Worker decrypts a value, and only when it needs it, for example to open a Database Profile connection.
  A decryption failure ends the work with a non-retryable configuration error that names the secret but not its
  value.
- Other tables store the secret name only: `database_profile.password_secret_ref`,
  `validation_mysql.admin_password_secret_name`, `model_config.credential_ref`, `agent_version.target_bearer_token_ref`,
  and the Agent database credential references.
- Only the platform database DSN and the encryption keys are environment variables. Platform access credentials are
  pending the owner's decision (implementation-readiness Open Decision); LLM provider tokens are secrets entered in
  the UI (architecture 16.6).
- Deleting a secret is outside the MVP.

## 9. Slice 1 Security Tests

- Generated identifiers cannot escape templates.
- Provisioning issues only `CREATE DATABASE` and the validated schema and seed statements; its tests
  reject any `CREATE USER`, `GRANT` or `DROP USER`. `DROP DATABASE` is issued only by Run cleanup, the leftover sweep and the privilege probe, each on a name matching a strict pattern.
- Provisioning refuses an existing Run database and leaves it unchanged.
- Agent database privilege probe (section 3.3): an administrator account passes and leaves no probe database; a read-only account is refused with a reason that names what it cannot do.
- SQL allowlist tests: multi-statement and versioned-comment bypass attempts are rejected before execution.
- Credential values never appear in Platform rows, application logs, or failure snapshots.
- Secret API responses never contain a plaintext value. A stored ciphertext contains no plaintext, and a ciphertext
  moved to another name or opened with another key fails.
- Rotation: a secret written under key A stays readable after key B becomes current; `secrets-rewrap` moves it to B
  with a compare-and-set update; a missing key id fails without revealing the value.
- A Run for an Agent without a configured database is refused at creation (`AGENT_DATABASE_NOT_CONFIGURED`) and nothing is queued.
- A failed load is cleaned up in `finally`; the leftover sweep drops only databases of Runs that are no longer QUEUED, PREPARING or RUNNING and ignores in-flight, unknown and differently named databases.
- Run databases are isolated: loading one Run never writes to another Run's database (MySQL 8.4).
- Cleanup is safe to run repeatedly.
