# Agent Test Platform

Generation-first implementation of an AI Agent Testing and Evaluation Platform.

## Current Scope

The Generation-first MVP accepts one short description, generates versioned Scenario,
MySQL Dataset, and Test Case assets, validates them, executes one Agent request in an
isolated MySQL environment, and evaluates state, Tool Call Chain, and final output with
deterministic checks plus a required LLM Judge.

Technical baseline:

- Go modular monolith with API and worker binaries
- Dedicated Platform MySQL plus a separate, platform-controlled validation MySQL instance that
  hosts per-Dataset-Revision execution schemas and a dedicated database per Test Run (no separate Sandbox MySQL instance)
- OpenAI and Ollama through an internal provider-neutral Model Gateway
- Expected Tool Call Chain in Test Cases and structured Tool Call Records from Agent APIs
- OpenAPI 3.1, JSON Schema Draft 2020-12, and versioned MySQL migrations
- React, TypeScript, Vite, React Router, and TanStack Query Web UI
- Budget-aware internal LLM Tool loops with deterministic Session Compaction

## Documentation

- [Platform architecture](docs/architecture.md)
- [MVP technical design index](docs/technical/README.md)
- [Implementation readiness decision](docs/implementation-readiness.md)
- [Public API contract](contracts/openapi.yaml)
- [Platform MySQL schema migrations](migrations/)

## Repository Layout

- `docs/`: architecture and implementation-level technical design
- `contracts/`: OpenAPI and JSON Schemas
- `cmd/`: API and worker executable entry points
- `internal/`: application modules
- `migrations/`: Platform MySQL migrations
- `deployments/`: deployment configuration
- `web/`: admin console Web UI (Generate Tests, Generation Jobs)

Implementation starts with Slice 0 and Slice 1 in
[the implementation plan](docs/technical/05-api-and-implementation-plan.md).

## API Development

Apply the `migrations/*.sql` files in lexical order to a dedicated MySQL 8.4 database before
starting the API. The current migration runner targets an empty database and is not invoked
during service startup.

Required API environment variables:

```text
HTTP_ADDR=127.0.0.1:8080
PLATFORM_MYSQL_DSN=platform_user:password@tcp(127.0.0.1:3306)/agent_test_platform
API_BEARER_TOKEN=<32-512 character secret>
GENERATOR_CONFIG_SHA256=<64 lowercase hexadecimal characters>
SECRET_ENCRYPTION_KEY_ID=k1
SECRET_ENCRYPTION_KEY_IDS=k1
ATP_ENCRYPTION_KEY_K1=<base64 of 32 random bytes>
```

`GENERATOR_CONFIG_SHA256` is the immutable digest of the resolved Generation model
configuration. It is supplied explicitly until model configuration loading is wired into the
worker slice.

Start the API with `go run ./cmd/api`. Run all automated checks with `go test ./...` and
`go vet ./...`.

The MySQL 8.4 integration tests run only when `ATP_TEST_MYSQL_DSN` names a test server (a DSN without a database name).
`TestDatasetValidatesOnTwoLiveTargets` also needs `ATP_TEST_MYSQL_DSN_B`, a second server. The tests create uniquely
named databases and never drop them, so remove leftover test databases by hand once they are no longer needed.

## Worker Configuration

The worker reads the runtime variables listed in
[the implementation plan](docs/technical/05-api-and-implementation-plan.md) (section 3). Test-environment credentials are stored encrypted in
Platform MySQL. Enter credentials on the Credentials page (or with `PUT /api/v1/secrets/{name}`), and register
validation MySQL instances on the 校验用 MySQL page: name, endpoint, admin username, the name of the admin password
credential, TLS mode, and an optional CA certificate for `VERIFY_IDENTITY`. The worker decrypts secrets with the keyring.

validation MySQL instances are registered in the platform, not in environment variables. Register one in two steps
(the examples use the platform bearer token and an API base URL of your choice):

```bash
export API_URL=http://127.0.0.1:8080

curl -X PUT "$API_URL/api/v1/secrets/target-a-admin-password" \
  -H "Authorization: Bearer $API_BEARER_TOKEN" -H 'Content-Type: application/json' \
  -d '{"value":"<administrator password>"}'

curl -X POST "$API_URL/api/v1/validation-mysql" \
  -H "Authorization: Bearer $API_BEARER_TOKEN" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: register-target-a' \
  -d '{"name":"target-a","host":"target-a.internal","port":3306,"admin_username":"atp_admin",
       "admin_password_secret_name":"target-a-admin-password","tls_mode":"DISABLED_LOCAL_ONLY"}'
```

Each generation request names one to eight registered Targets in `validation_db_ids`. The worker validates the Dataset on
every listed Target, in request order, and publishes all bindings together. The first Target is the primary one: the
Dataset Critic and Test Case generation read from it. The worker opens each Target's administrator connection from the
registry and no longer reads `TARGET_MYSQL_ADMIN_DSN` or `TARGET_MYSQL_CA_FILE`.

### Registering an Agent

An Agent is the entity under test; it records the SSE chat endpoint, the name of the bearer-token secret
(`target_bearer_token_ref`, entered first through `PUT /api/v1/secrets/{name}`), the model, and the Tool Catalog
(required for `DETERMINISTIC_CHAIN`, forbidden for `SESSION_LLM_ONLY`). Endpoints: `POST/GET /api/v1/agents`,
`GET /api/v1/agents/{agentId}`. Writes require `Idempotency-Key`.

### Rotating the encryption key

1. Generate a key with `openssl rand -base64 32`. Add it as `ATP_ENCRYPTION_KEY_<ID>`, add its id to
   `SECRET_ENCRYPTION_KEY_IDS`, and set `SECRET_ENCRYPTION_KEY_ID` to it. Keep the old key listed.
2. Restart the API and the worker.
3. Run `go run ./cmd/secrets-rewrap` with the same `PLATFORM_MYSQL_DSN` and keyring variables. It prints how many
   credentials remain under each key id.
4. When no credential remains under the old id, remove that id from `SECRET_ENCRYPTION_KEY_IDS` and delete its
   variable.

## Validation MySQL Test Objects

The platform creates these databases on validation MySQL through the registered administrator connection of their Target,
and never deletes them automatically:

- `dataset_<ULID>`: the validated execution schema of one Dataset Revision. Each validation attempt,
  including each repair round, creates one.
- `run_<ULID>`: the dedicated database of one Test Run, holding the Dataset Revision's validated
  schema and seed. Each Test Run has its own database, so Runs never share data.

The platform never creates, changes, or drops validation MySQL accounts. Tests follow the same rule: they
never drop a database they created, and they use unique names so reruns do not collide. Test data is
kept for traceability.

Deleting a database is a manual operator action, taken once its Dataset Revision or Test Run no longer
needs the data. The ULID in a database name is the Dataset Revision or
Test Run ID in lowercase. A validation attempt that fails part way can leave its execution schema on the Targets it
loaded before the failure. Nothing references those schemas, so they are deleted the same way. To list the objects:

```sql
SELECT SCHEMA_NAME FROM information_schema.SCHEMATA
WHERE SCHEMA_NAME LIKE 'dataset\_%' OR SCHEMA_NAME LIKE 'run\_%';
```

### Verifying and deleting Target objects by hand

Before deleting an object, confirm that nothing still needs it:

```sql
-- dataset_<ULID>: still the execution schema of a published Dataset Revision?
SELECT id FROM dataset_revision WHERE execution_database = 'dataset_<lowercase ULID>';

-- run_<ULID>: which Test Run created it, and did that Run finish?
SELECT id, status, finished_at FROM test_run WHERE id = '<uppercase ULID>';
```

After checking, an operator removes the object with a Target administrator account:

```sql
DROP DATABASE `dataset_<lowercase ULID>`;
DROP DATABASE `run_<lowercase ULID>`;
```

Run databases are kept after their Run ends, so the data stays available for traceability. Delete a
Run database only when that Run is no longer needed.

### Draft purge (manual)

Expired staging drafts of finished Generation Jobs are deleted only when an operator runs:

```text
PLATFORM_MYSQL_DSN=<platform DSN> go run ./cmd/purge-drafts
```

The command runs once, logs how many artifact and Test Case drafts it deleted, and exits with status 1
on failure. Nothing schedules it.

## Web Development

Use Node.js 22 LTS (minimum supported versions are declared in `web/package.json`). Install and
start the frontend from the repository root:

```text
npm --prefix web install
npm --prefix web run dev
```

The Vite development server listens on `http://127.0.0.1:5173` and proxies `/api` and `/healthz`
to the Go API at `http://127.0.0.1:8080`. The Bearer token is held in browser session storage;
Provider, Agent, and database credentials are typed into the Secrets page once. The browser never stores them,
and the API never returns them.

Run frontend gates with:

```text
npm --prefix web run build
npm --prefix web run lint
npm --prefix web audit
```

Production builds are written to `web/dist`. Serve that directory behind the same origin as the
Go API with SPA fallback to `index.html`, or set `VITE_API_BASE_URL` at build time and configure
the deployment gateway accordingly.

## Docker Compose

The development stack includes Platform MySQL, validation MySQL, API, Generation Worker,
Web UI, an OpenAI-compatible Mock LLM, and a Mock Target implementing Model List, asynchronous
text Knowledge indexing, and SSE Chat.

```text
docker compose --env-file deployments/compose.env.example up --build
```

Open the Web UI at `http://127.0.0.1:3000`. Development endpoints are API `:8080`, Mock LLM
`:8081`, Mock Target `:8082`, Platform MySQL `:3307`, and validation MySQL `:3309`. validation MySQL
hosts both the optional read-only reference-schema fixture and the per-Dataset-Revision
execution schemas and per-Run databases the Worker provisions at runtime.

The example credentials are intentionally local-only. The platform API, Generation LLM, Target
Platform, Platform MySQL, and validation MySQL each use distinct credentials. Replace every example
value through a secret manager outside local development. To switch the Worker to Ollama, set
`GENERATION_MODEL_PROVIDER=ollama`, its `/v1` base URL and model name; the API token may be empty
only for an isolated unauthenticated Ollama endpoint.

An optional local Ollama container is available through the `ollama` Compose profile. It does not
download a model automatically:

```text
docker compose --profile ollama up -d ollama
docker compose exec ollama ollama pull <model>
```

Then point the Worker at `http://ollama:11434/v1`. Pin `OLLAMA_IMAGE` to an approved immutable tag
or digest outside local development.

The initial SQL is mounted into MySQL's `docker-entrypoint-initdb.d` and applies only when the
Platform volume is first created. After an intentional schema reset, use
`docker compose down --volumes` before restarting. This command destroys local Compose data.

### Browsing Scenarios, Datasets and Test Cases

Read-only `GET` endpoints list `/scenarios`, `/datasets` and `/test-cases` (newest first, cursor paged), their `/{id}/revisions`, and single `/scenario-revisions/{id}`, `/dataset-revisions/{id}` and `/test-case-revisions/{id}` revisions.


### Test Runs

The API queues and reads Test Runs: `POST /api/v1/test-runs` with `{agent_id, test_case_revision_ids[], repetitions}` (one QUEUED Run and one `run` queue item per case and repetition, in one transaction; `Idempotency-Key` replays the batch), `GET /test-runs`, `/test-runs/{id}`, `/tool-calls`, `/evaluations`, `POST /test-runs/{id}/cancel`, `GET /evaluations` and `GET /benchmark?test_case_revision_id=...` (repeatable). A Test Case Revision already links its Scenario and Dataset, so there are no Test Suites or Snapshots (removed by migration 0016).

### Agent database and seed data

The Dataset stays on the platform. An **Agent** carries the database that receives test data (host, port, database name, account, password secret name, TLS), configured when the Agent is created and stored as a Database Profile referenced by `agent.database_profile_id`. A Run **requires** it: `POST /test-runs` for an Agent without a database is refused with `409 AGENT_DATABASE_NOT_CONFIGURED`. Before a Run the Dataset's schema and seed data are written into a `run_<ULID>` database on that server and dropped afterwards; credentials are never sent to the Agent, which is only told the database name in the prompt. The Agent's behavior (endpoint, model, Tools) is recorded on the Agent itself so that results stay comparable.

**Validation MySQL** (formerly "Target MySQL"; `/api/v1/validation-mysql`, table `validation_mysql`, migration 0019) is the platform's own server, shown in the UI under 系统 → 校验用 MySQL. It is not part of any Agent: it only executes and validates a Dataset's schema and seed SQL while the Dataset is *generated* (`validation_db_ids` in a generation request). Runs do not reference it (migration 0018 drops `test_run.target_mysql_id`). The Dataset's `schema_sql` / `seed_sql` are the initial SQL: at the start of a Run the platform executes exactly those statements on the Agent's database; nothing is exported from or imported through the platform's database, so there is no cross-engine conversion.

Agent database safeguards:
- **Connectivity check at creation**: `POST /agents` with a `database_profile_id` connects with the stored credentials, creates a throwaway `atp_probe_*` database, writes a row and drops it. An account that cannot connect, create databases or write is refused with `400 AGENT_DATABASE_CHECK_FAILED` and its reason, so the problem shows up when the Agent is configured rather than at the first Run.
- **Leftover sweep**: the worker visits every Agent database server every 10 minutes and drops `run_<ULID>` databases whose Run is known and no longer QUEUED, PREPARING or RUNNING (a crashed worker's remains). Databases of in-flight Runs, of unknown Runs, and anything not named `run_<26 chars>` are never touched.

## Test Run Execution

The same worker binary also runs the Test Run execution stage (internal/jobs/runworker, internal/runexec). It claims queued runs, provisions the dedicated 
un_<ULID> database on the validation MySQL, calls the Agent over SSE (the prompt names the database; no credentials are sent), diffs state, runs deterministic checks and stores them with status EVALUATING. The LLM Judge stage is not implemented yet.

## Test Run Evaluation

When a Run settles into EVALUATING, the worker's evaluation loop (internal/jobs/evalworker) claims the valuation queue item, builds a contract-validated Judge request from the stored evidence (valuation.BuildRequest; nothing is truncated, oversize inputs fail non-retryably), calls the LLM Judge, and aggregates deterministic and LLM scores (70/70 thresholds, deterministic critical failures are a hard gate). The attempt is stored in valuation_attempt; the Run becomes COMPLETED, or EVALUATION_FAILED once attempts are exhausted.

## Evaluation Attempts, Re-evaluation and Judge Models

- Each Evaluation Attempt moves `PENDING` -> `RUNNING` -> `COMPLETED` | `FAILED` | `CANCELLED`
  (`PENDING` again when a retryable failure is rescheduled). A `PENDING` attempt is created when a Run
  settles and carries the Judge model alias.
- Re-evaluation never mutates the original Run: `POST /api/v1/test-runs/{runId}/evaluations` with
  `{"judge_model_alias": "..."}` and an `Idempotency-Key` creates a new Run (202, `run_ids`) that copies the
  execution evidence and Tool Call Records and is judged by the requested alias.
- The worker registers the generation/default Judge (`GENERATION_MODEL_*`) and optionally a second Judge via
  `JUDGE_MODEL_NAME`, `JUDGE_MODEL_ALIAS`, `JUDGE_MODEL_PROVIDER`, `JUDGE_MODEL_BASE_URL`,
  `JUDGE_MODEL_API_TOKEN`; each job is routed by its attempt's alias.
- Target Agents list their Tools through paginated `GET /v1/tools` (`contracts/target-openapi.yaml`).

## Model Selection

Generation and Judge models are registered in `model_config` by the worker at startup (`GENERATION_MODEL_*`, optional
`JUDGE_MODEL_*`). Exactly one model per role is active (migration 0015 enforces this with a unique key).
`GET /api/v1/models` lists them; `PUT /api/v1/models/{role}/active` with `{"alias": "..."}` selects the active
`generation` or `judge` model. Re-evaluation requires a registered Judge alias. Note: the Generation pipeline still
uses the worker's configured Generation alias; the active flag is only recorded, and new evaluations use the active Judge.

## Agent Tool Discovery

`POST /api/v1/agent-tool-discovery` with `{"chat_endpoint", "target_bearer_token_ref"}` calls the Target Agent's paginated
`GET /v1/tools` and returns `{"tools": [...]}` (validated like `tool_catalog`) so testers can pre-fill an Agent's
tool catalog. Failures return 502 `TARGET_TOOL_LIST_FAILED`.

## End-to-end Tests

With `ATP_TEST_MYSQL_DSN` set (for example a throwaway `docker run mysql:8.4`), `go test ./internal/store/mysql/` also runs
`TestEvaluationPipelineWithMockJudgeOnMySQL84` and `TestHTTPAPIToMockJudgeEndToEnd`. They run the real HTTP API, services,
queue, evaluation worker and MySQL, with the Judge served over HTTP by the Mock LLM (`internal/mockllm`).

`TestRunLifecycleWithMockTargetAndJudgeEndToEnd` covers the full Run lifecycle: `POST .../runs` -> run worker (isolated Run
database on real MySQL) -> Mock Target Agent over SSE (`internal/mocktarget`) -> evaluation worker with the Mock LLM Judge ->
`COMPLETED` Run and evaluation via the API, and checks the Run database is dropped.

## Admin Console and UI End-to-end Tests

The web UI (`web/`) is a back-office console (left navigation, tables, modals; Chinese UI): dashboard, test generation, asset library,
test runs (run tests, list, detail, Tool calls, evaluation attempts, cancel, re-evaluate), benchmark, Agent and Agent
Version management (with Tool discovery through the Agent's `GET /v1/tools`), validation MySQL, model selection and credentials.

Model selection: the worker registers `GENERATION_MODEL_*` plus the optional second models `GENERATION_ALT_MODEL_*` and `JUDGE_MODEL_*`.
Generation calls and new evaluations follow the model selected through `PUT /api/v1/models/{role}/active` (the worker re-reads the
selection every few seconds and falls back to `GENERATION_MODEL_ALIAS` when none is usable).

UI end-to-end test (Playwright, needs Docker, Go and Node 20.19+/22.13+):

```sh
bash web/e2e/stack.sh up      # 2 MySQL containers, mock LLM, mock Agent, API, worker, built web UI -> prints READY
cd web && npx playwright test # drives the real UI through credentials, Target, Agent (with its database), run, evaluation, re-evaluation, benchmark
bash web/e2e/stack.sh down
```

Frontend unit tests (Vitest + Testing Library): `cd web && npm test` (pure logic, API layer, shared components, Runs, Run detail and Models pages).
`cmd/worker` has a MySQL 8.4 integration test (`TestGenerationFollowsActiveModelOnMySQL84`) proving a model switch changes which model the Mock LLM receives.

## Test Assets, Dependencies and Running Tests

The menu is organised by what you manage:

- **业务场景 / 数据集 / 测试用例** (Test assets): each kind has its own list (search, details) and its own "generate" dialog, backed by a single-kind
  Generation Job (`POST /api/v1/generation-jobs` with `job_kind`).
  - Scenario: a description only; no dependency.
  - Dataset: **requires** a Scenario revision (ACTIVE or VALIDATED) and at least one validation MySQL.
  - Test cases: **require** a Scenario revision and a Dataset revision generated for that scenario; the dataset picker stays disabled until a
    scenario is chosen and only lists that scenario's datasets.
  - Omitting `job_kind` still produces a full package (scenario -> dataset -> test cases) in one request.
  - The API enforces the same rules (`input_scenario_revision_id`, `input_dataset_revision_id`, schema `generation-request.schema.json`).
- **测试运行** (Test runs): the "运行测试" button opens a dialog with a searchable dropdown of test cases (newest 10 by default, keyword search over id and content),
  plus an Agent and version, and starts the runs directly (no suite or snapshot). Test case rows also have a "运行测试" button. Backed by
  `GET /api/v1/{scenario,dataset,test-case}-revisions?q=&limit=&cursor=` and `POST /api/v1/test-runs`.
- **基准对比**: pick test cases to compare the Agents that ran them.
