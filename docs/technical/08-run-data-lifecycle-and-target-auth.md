# 08 — Run Data Lifecycle, Target Authentication, and Concurrency

Status: Design

This document supersedes the "one database per Run" isolation model (01 section 7,
03) and the generation-time data injection, to adapt the platform to a Target Agent
whose database is a fixed, operator-configured database (identified by a concrete
database name) rather than a pool of per-Run databases. The Target Agent's own code
is unchanged; only the platform and the Target compatibility contract are adjusted.

## 1. Core change: fixed target database, serialized runs

- The Target Agent's database configuration now includes a concrete `database` name,
  not just a host / instance. It is stored with the Agent (the existing
  `agent.database_profile_id` path).
- A Test Run no longer creates a dedicated `run_<ULID>` database. It injects test data
  into the Target's fixed database and removes it afterwards.
- Because the database is shared, one Target Agent must not run concurrent Test Runs.
  Concurrency is prevented with a DB status check plus a Redis distributed lock
  (section 5).

## 2. Test data injection (setup / teardown)

### 2.1 Generation time — platform MySQL

- The generator emits `schema_sql` + `seed_sql` exactly as today.
- The platform executes `seed_sql` against the platform MySQL to validate the data
  format.
- The data produced by that execution is retained in the platform MySQL as the
  "test data template". It is a validation reference only, never an export source.

### 2.2 Run setup — target database

1. **Residual check.** Before injecting, verify the target database has no leftover
   test data. Leftover data indicates a failed teardown or a lost lock and must abort
   the Run (surface as a deterministic failure, not an infrastructure error).
2. **Injection.** Run the original `seed_sql` against the target database. Injection is
   always performed with the original seed SQL; the platform never exports data from
   the platform MySQL template.

### 2.3 Run teardown

- Remove the injected test data from the target database using SQL.
- Teardown runs on the Run completion path (success and failure alike); a failed
  teardown leaves residue that the next setup's residual check will detect.

## 3. Knowledge as test data + delete contract

- Knowledge fixtures are part of the test data, not a separate lifecycle.
- The Target knowledge API gains `DELETE /v1/knowledge/documents/{id}` (the current
  minimum compatibility API only has upload and status).
- Run setup uploads the Test Case's `knowledge_documents` (with a residual check,
  section 2.2) and gates the chat request on `READY` as before; run teardown deletes
  the uploaded documents by id.

## 4. Target authentication: client credentials OAuth

- Add a client credentials OAuth token endpoint to the Target contract.
- The `client_id` / `client_secret` are configured on the Target Agent configuration
  page and stored encrypted (as the existing secret reference mechanism).
- The platform obtains an access token through the client credentials grant before
  calling the Target (`/v1/chat`, `/v1/models`, `/v1/tools`, knowledge endpoints),
  replacing the static bearer token resolution.
- The token is short-lived and is never persisted in Run evidence or logs (same
  rule as `target_bearer_token_ref` today).

## 5. Run concurrency guard

- One Target Agent must not run concurrent Test Runs.
- **DB status check.** A Run is refused (or held) when the Target already has a Run in
  a non-terminal state.
- **Redis distributed lock.** An additional guard scoped per Target Agent. The
  platform gains a Redis dependency (new to the deployment topology).
- The lock and the status check are belt-and-suspenders: the DB check is the source of
  truth; the lock closes the race window between check and enqueue.

## 6. Contract and schema impact

| Area | Change |
|---|---|
| Target knowledge API | add `DELETE /v1/knowledge/documents/{id}` |
| Target auth | add client credentials token endpoint |
| Agent database | require a concrete `database` name in the database profile |
| Platform schema | test data template storage (generation-time validated data) |
| Platform deployment | add Redis |
| Workflow states | Run gains setup/teardown data phases and per-Target concurrency state |

Per the change rule in `README.md`, the Target contract additions (knowledge delete,
token endpoint) and the workflow-state change require a version bump and a migration
or compatibility note. The `run_<ULID>` database wording in 01/03/06 is superseded by
this document.
