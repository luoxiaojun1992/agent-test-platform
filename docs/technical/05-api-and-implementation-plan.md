# API and Implementation Plan

## 1. Module Layout

```text
cmd/
├── api/
└── worker/

internal/
├── domain/
├── httpapi/
├── store/mysql/
├── jobs/mysqlqueue/
├── llm/
│   ├── openai/
│   └── ollama/
├── generation/
├── validation/
├── sandbox/
├── agent/
├── toolmatch/
├── evaluation/
└── observability/

contracts/
├── openapi.yaml
└── schemas/

migrations/
└── 0001_initial.sql
```

Domain packages do not import Provider SDK types, HTTP handlers, or SQL driver types.

## 2. API Rules

- JSON only; UTF-8.
- ULID resource IDs.
- RFC 3339 UTC timestamps.
- `Idempotency-Key` required on create/run operations.
- Error body always contains `code`, `message`, `retryable`, and optional `details`.
- List endpoints use cursor pagination; default 50, maximum 200.
- Unknown JSON fields are rejected on write requests.
- API version is `/api/v1`; persisted contract schemas have independent semantic versions.

The normative public API is `contracts/openapi.yaml`. The outbound contract to the Agent under test uses the
Target chat request and SSE event JSON Schemas plus `07-target-sse-contract.md` ("Target" in those file names means the Agent under test).

## 3. Configuration

Platform configuration comes from environment variables read at startup or rarely changed. Compose or Kubernetes injects them:

```text
HTTP_ADDR
PLATFORM_MYSQL_DSN                (platform database; the password stays in the environment)
SECRET_ENCRYPTION_KEY_ID          (id of the key that encrypts new secrets; section 8.1)
SECRET_ENCRYPTION_KEY_IDS         (comma-separated ids the process may read; must include the current id)
ATP_ENCRYPTION_KEY_<ID>           (one per listed id: base64 of 32 random bytes)
API_BEARER_TOKEN                  (pending: platform administrator credential, implementation-readiness Open Decision)
GENERATOR_CONFIG_SHA256           (until the LLM configuration moves to the UI, architecture 16.6)
GENERATION_MODEL_PROVIDER         (until architecture 16.6)
GENERATION_MODEL_BASE_URL         (until architecture 16.6)
GENERATION_MODEL_NAME             (until architecture 16.6)
GENERATION_MODEL_API_TOKEN        (until architecture 16.6)
GENERATION_MODEL_ALIAS            (until architecture 16.6)
GENERATION_CONTEXT_WINDOW         (until architecture 16.6)
GENERATION_MAX_OUTPUT_TOKENS      (until architecture 16.6)
```

Test-environment configuration (validation MySQL instances, Database Profiles, Agents, Agents, LLM providers) and every credential are
entered through the Web UI or the API and stored in Platform MySQL. They change without a restart. Credentials are
never read from environment variables and never fall back to them.

Database connections for validation and for Runs are not environment settings. Each registered validation MySQL (architecture 16.5) keeps its
endpoint, administrator secret reference, TLS mode, and optional CA certificate in Platform MySQL; each Agent's database is a Database Profile referenced by `agent.database_profile_id`.

Configuration validates all selected model capabilities and database separation at startup.
Validation MySQL registration rejects the platform endpoint (architecture 16.5).

Secret namespaces are distinct: `API_BEARER_TOKEN` authenticates QA/Web clients to this platform;
OpenAI/Ollama credentials authenticate Model Gateway egress; each Agent has its own
Agent token reference, and each Agent's database account is its own secret reference. The platform issues no Run-scoped tokens. No namespace may
fall back to another.

## 3.1 Dependency Baseline

| Concern | Selected Approach |
|---|---|
| OpenAI/Ollama wire client | Go `net/http` plus internal versioned DTOs |
| Optional provider proxy | LiteLLM through the same OpenAI-compatible Adapter |
| MySQL | `github.com/go-sql-driver/mysql` through `database/sql` |
| ULID | `github.com/oklog/ulid/v2` |
| JSON Schema 2020-12 | `github.com/santhosh-tekuri/jsonschema/v6` |
| OpenAPI validation | `github.com/getkin/kin-openapi` |
| MySQL AST | Vitess `sqlparser`, conditional on Slice 0 corpus acceptance |
| Decimal canonicalization | Go `math/big`, no floating-point comparison |
| Frontend | React, TypeScript, Vite, React Router, TanStack Query |

The LLM client intentionally uses the standard HTTP library rather than Provider SDK request
types. The supported Chat Completions subset is small, and owning these DTOs prevents SDK release
cadence or Provider-only fields from leaking into domain code. LiteLLM remains a deployment
option when centralized routing or more Providers justify the additional service.

Dependency versions are pinned in `go.mod` and the frontend lockfile during Slice 0 after their
contract tests pass. The SQL parser is not considered selected until it passes all allowed and
forbidden MySQL 8.4 fixtures; parser gaps fail closed.

## 4. API and Worker Process

The same Go module builds two binaries:

- `api`: synchronous validation, CRUD reads, Job creation, status endpoints.
- `worker`: Generation, Execution, Evaluation, and the leftover Run-database sweep (every 10 minutes). Draft purging is a manual command (`cmd/purge-drafts`).

Worker roles are selected by configuration so they can later be scaled independently without
splitting the codebase into services.

The MVP frontend is React + TypeScript + Vite. Platform API authentication is single-tenant
Bearer Token authentication backed by a secret reference; user management and RBAC are not in
MVP. There are no Run-scoped tokens: the platform issues no credential to the Agent under test.

## 5. Vertical Implementation Slices

### Slice 0: Contract Harness

- Load and validate all JSON Schemas.
- Validate OpenAPI syntax.
- Run migration against an empty Platform MySQL.
- Add golden fixtures for canonical SQL results and Tool matching.

Exit: CI catches schema, migration, and contract drift.

### Slice 1: Deterministic Execution Schema

- Create a per-dataset execution schema once while a Dataset is generated (on a validation MySQL), and for each Run a dedicated `run_<ULID>` database
  on the Agent's own database server, loaded by executing the Dataset's own `schema_sql` and `seed_sql`. The platform creates no accounts.
- Use the account configured on the Agent (technical/06 section 3.3) and check it when the Agent is created (connect, create, write, drop a probe database).
- Apply a fixed hand-authored fixture.
- Capture before/after row digests.
- Enforce Post Conditions, invariants, and allowed mutations.

Exit: a deliberately destructive update hard-fails; an Agent database account that cannot create databases or write is refused at creation; a Run for an Agent without a database is
refused; a finished Run leaves no `run_<ULID>` database and the sweep drops a leftover of a finished Run but never an in-flight one.

### Slice 1A: Credentials, Keyring, Validation MySQL Registry, Dataset Bindings

Implementation status: implemented. The keyring and rotation (`cmd/secrets-rewrap`), the credential store, the validation MySQL
registry (`POST`/`GET /api/v1/validation-mysql`, the 校验用 MySQL page), Dataset bindings (`dataset_revision_validation_db`,
`validation_db_ids`), and the Worker's resolution of validation MySQL connections from the registry are in place. `TARGET_MYSQL_ADMIN_DSN` and
`TARGET_MYSQL_CA_FILE` are removed. Registry IDs are lowercase UUIDs; the other IDs stay ULID. (This registry was first called "Target MySQL"; it was renamed in migration 0019
because it belongs to the platform, not to an Agent under test.) Verified against two live
MySQL 8.4 servers: publication of bindings for two validation MySQL instances (`TestPublishGenerationFullPipeline`), and one Dataset
validated and loaded on both servers (`TestDatasetValidatesOnTwoLiveTargets`). The registry and the credential store
are covered by the MySQL 8.4 integration tests.

- Credential store and keyring: `secret.key_id`, `SECRET_ENCRYPTION_KEY_ID`/`_IDS`, `ATP_ENCRYPTION_KEY_<ID>`, and
  `cmd/secrets-rewrap` (technical/06 section 8.1). `PUT`/`GET /api/v1/secrets` and the Credentials page.
- Validation MySQL registry: `validation_mysql` with endpoint, admin secret reference, TLS mode, and optional CA PEM;
  `POST`/`GET /api/v1/validation-mysql`; the 校验用 MySQL page (architecture 16.5).
- Bindings: `dataset_revision_validation_db`; generation requests carry `validation_db_ids`; validation runs on every
  listed instance; publication writes all bindings in one transaction; a failing instance blocks publication.
- Worker: validation MySQL connections come from the registry, cached by ID and admin secret version. The
  `TARGET_MYSQL_ADMIN_DSN` and `TARGET_MYSQL_CA_FILE` settings are removed.
- Runs are separate from this registry: `POST /api/v1/test-runs` takes `{agent_id, test_case_revision_ids[], repetitions}` and writes the Dataset's initial SQL into the Agent's own database (migrations 0016-0018).

Exit: a Dataset Revision bound to two validation MySQL instances creates its execution schema on both and publishes both bindings; a
failure on either blocks publication; registration rejects the Platform MySQL endpoint and an invalid CA PEM;
a key rotation keeps every secret readable without re-entry; integration tests use two instances on MySQL 8.4 and
never drop a database or account they did not create themselves.

### Slice 2: Agent and Tool Evidence

- Implement Agent Adapter and response validation.
- Persist structured Tool Call Records.
- Implement SQL AST normalization and ordered-subsequence matcher.
- Treat every returned Tool Call Record as `AGENT_REPORTED` (technical/04 section 4); the platform does not proxy or reconcile Tool calls.

Exit: correct, missing, reordered, forbidden, and extra-write cases are classified correctly.

### Slice 3: Judge

- Implement normalized Model Gateway.
- Add OpenAI and Ollama adapters plus capability probes.
- Implement Judge schema, retries, scoring, and hard gates.
- Run the calibration corpus for each Judge model alias.

Exit: provider contract tests pass and no hard-failure fixture receives PASS.

### Slice 4: AI Generation

- Implement Scenario and Dataset stages.
- Implement `create_test_case` Tool and repair loop.
- Implement budget-aware internal LLM Session Compaction for multi-step Tool loops.
- Implement Critic and atomic Publisher.
- Enforce exact requested Test Case count.

Exit: one description produces a complete immutable package or publishes nothing.

### Slice 5: API and UI

- Implement Generation Job, asset, run, benchmark, Agent, validation MySQL, model and evaluation endpoints.
- Build and ship the React + TypeScript + Vite Web UI.
- Implement the per-asset generation dialogs, Job progress, package review, run list and detail, run dialog, and result views with loading, empty,
  error, cancellation, and retry states.
- Add Agent comparison over the Runs of selected Test Case Revisions (`GET /api/v1/benchmark`).

Exit: the MVP acceptance journey works without manual asset authoring.

## 6. Required Test Matrix

### Contract

- Every example validates against its JSON Schema.
- Unknown fields, oversized fields, malformed Tool arguments, and duplicate IDs fail.
- OpenAI and Ollama adapters pass identical behavior tests.

### SQL and State

- DDL/DML allowlist and every forbidden statement category.
- Cross-database access and MySQL versioned comments.
- Canonical values: null, decimal, datetime, binary, JSON, Unicode.
- Multi-row ordering and truncation.
- Insert/update/delete within and outside allowed mutation scope.
- Global invariant violation despite a passing target Post Condition.

### Tool Chain

- Exact required sequence.
- Extra allowed read.
- Extra write.
- Missing required step.
- Wrong Tool and wrong argument.
- Tool error and affected-row mismatch.
- Agent-reported Tool calls that disagree with the observed database state.

### Workflow

- Duplicate idempotency key with same and different digest.
- Worker crash before and after side effect.
- Expired lease reclaimed with fencing.
- Cancellation at each pre-publish state.
- Publish transaction rollback.
- Agent timeout followed by cleanup and Judge.
- Judge outage after environment cleanup.
- Oversized Tool results compact without splitting call/result groups or losing evidence digests.
- Missing Tool semantic summaries and still-over-budget sessions fail explicitly.

### Web UI

- Asset generation (scenario, dataset, test cases, with dependency gating) and progress polling work against the versioned API.
- Package, assumptions, Run evidence, deterministic checks, and Judge results are inspectable.
- Cancellation, retryable errors, empty states, and expired authentication are actionable.
- Desktop and mobile layouts have no clipped controls, overlapping text, or inaccessible focus order.

### Evaluation

- Critical state failure with high LLM scores remains FAIL.
- Critical Tool failure with high LLM scores remains FAIL.
- Missing Judge criterion invalidates response and retries.
- Provider fallback creates a distinct Attempt.
- Prompt injection in Agent output and Tool result does not alter Judge instructions.

## 7. Pull Request Gates

Before merging a slice:

- unit and race tests pass;
- schema examples validate;
- migration applies from empty database and rolls forward from previous version;
- no secret appears in snapshots or logs;
- affected architecture and technical docs are updated;
- integration tests never drop databases or accounts they did not create; they use unique names so reruns do not collide.
- the Agent database privilege check and the leftover sweep pass against a real MySQL for any change to Run provisioning or cleanup.
- the browser end-to-end test (`web/e2e`) passes: it generates every asset from an empty platform with the Mock LLM, runs it on a Mock Agent, and evaluates it.

## 8. Implementation Start Decision

The project is ready to begin with Slice 0 and Slice 1. Production implementation of a later
slice starts only after the preceding slice exit criteria pass. The Agent's database account is entered on the Agent in the UI (technical/06 section 3.3). Multiple validation MySQL instances, Dataset bindings, the keyring, and the validation MySQL registry are decided (architecture 16.5) and implemented in Slice 1A, which starts after the Slice 1 exit criteria pass. The platform administrator credential is pending the owner's decision and does not block Slice 0 or Slice 1.
