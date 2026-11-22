# Storage and Workflow Design

## 1. Database Topology

Use separate MySQL security domains. Platform MySQL is the platform's own; a second, platform-controlled **validation MySQL** serves Dataset generation; and every Agent under test brings its own database server for Runs:

| Name | Purpose | Generated SQL Access |
|---|---|---|
| Platform MySQL | Metadata, immutable revisions, queue, run evidence | Never |
| validation MySQL | Business reference schemas and per-dataset execution schemas, used only while a Dataset is generated | Only AST-validated statements, through the registered validation MySQL (technical/06 section 1.1); the platform creates no accounts |
| Agent database | One per Agent; per-Run `run_<ULID>` databases created from the Dataset's initial SQL | Only AST-validated statements, through the account configured on the Agent (technical/06 section 3.3); the platform creates no accounts |

MVP baseline is MySQL 8.4 LTS with `utf8mb4`, UTC, and strict SQL mode. Platform, validation MySQL and Agent database
credentials are unrelated. Application services never interpolate database,
table, or account names from LLM output.

Every Platform, validation MySQL and Agent database connection executes `SET time_zone = '+00:00'`. Generated Dataset
metadata has `timezone=UTC`; MVP does not support local Dataset timezones. Time-dependent cases
must use literal UTC timestamps supplied in the Dataset and Prompt. Calls to `NOW()`,
`CURRENT_TIMESTAMP`, or equivalent nondeterministic clock functions are rejected in generated
assertions and seed data.

## 2. Identity and Storage

- IDs are application-generated ULIDs stored as `CHAR(26) CHARACTER SET ascii`.
- Timestamps use `TIMESTAMP(6)` in UTC.
- Structured payloads use native `JSON`.
- SQL and long model output use `MEDIUMTEXT` with explicit byte limits at the service boundary.
- Every immutable artifact stores a lowercase hexadecimal SHA-256 digest over canonical bytes.
- MVP stores artifacts in Platform MySQL so package publication can be one transaction.

Canonical JSON uses UTF-8, sorted object keys, no insignificant whitespace, and JSON number
syntax. SQL digests use UTF-8 after LF newline normalization; SQL text itself is not
rewritten before execution.

## 3. Generation Job Kind and Cardinality

A Generation Job has one `job_kind` and creates exactly one kind of output:

| `job_kind` | Output | Required input |
|---|---|---|
| `SCENARIO` | one Scenario Revision | none (from the original description) |
| `DATASET` | one Dataset Revision, under a new-or-existing logical Dataset | `input_scenario_revision_id` (`ACTIVE` or `VALIDATED`) |
| `TEST_CASES` | exactly `request.test_case_count` new logical Test Cases and Revisions | `input_scenario_revision_id` and `input_dataset_revision_id` (both `ACTIVE` or `VALIDATED`) |

MVP does not publish partial output for a single Job. For a `TEST_CASES` Job, if the requested
count is 10 and fewer than 10 unique cases pass validation and critic review after two repair
rounds, the Job becomes `FAILED` and publishes zero logical Test Cases. `SCENARIO`/`DATASET` Jobs
follow the same all-or-nothing rule for their one output Revision.

A **Generation Pipeline** chains a `SCENARIO` Job, then a `DATASET` Job (using the Scenario Job's
published output as `input_scenario_revision_id`), then a `TEST_CASES` Job (using both prior
outputs) to reproduce the previous "one description in, full package out" experience. Each
chained Job still publishes independently the moment it passes its own gate; the Pipeline does
not hold back an earlier Job's already-published output while a later Job is still running. A
`DATASET` or `TEST_CASES` Job can also be submitted standalone (no `pipeline_id`), referencing an
existing published Scenario and/or Dataset Revision from any earlier Pipeline or Job, to add
another Dataset to a Scenario or another batch of Test Cases to a (Scenario, Dataset) pair.

Drafts expire 24 hours after they are written, for debugging. Raw model
output follows the same retention policy. A failed Job can be retried only as a new Job
linked by `retry_of_job_id`.

An operator deletes expired rows by running `go run ./cmd/purge-drafts` with `PLATFORM_MYSQL_DSN`
set. The command runs once and exits. Nothing schedules it: until it runs, expired drafts stay in
staging tables, and reads do not check expiry, so a Job waiting in review can still read its drafts.
It hard-deletes expired rows in batches of 500, and only
for Jobs that reached `COMPLETED`, `FAILED`, or `CANCELLED`. A Job still in `AWAITING_REVIEW` or in
progress keeps its drafts however old they are, because its review or continuation still needs them.
Drafts have no public API apart from the read-only review view of a Job in `AWAITING_REVIEW`
(`GET /generation-jobs/{id}/draft`), and are excluded from long-term logical exports. Platform
MySQL encryption at
rest and encrypted backups are deployment requirements; backups retain expired drafts only for
the configured backup retention period.

Dataset creation is always deadline-bound. AI Dataset generation has a 3-minute Stage timeout, and
AI Dataset validation, including the Semantic Critic review, has a 6-minute Stage timeout. Together
these two stages leave a minute of the 10-minute Generation Job deadline to publish.
Import from a validation MySQL Database Profile has a configurable `snapshot_timeout_seconds` in
`[1,300]`, default 300 seconds. Individual metadata/query statements remain limited to 5 seconds.
Every Generation Job has a 10-minute processing budget. The budget starts when the Job first
advances past acceptance and is fixed in `generation_job.deadline_at`, so a retry after a Lease takeover
keeps the same deadline. The budget is wall-clock time, so time spent waiting in `AWAITING_REVIEW`
counts: a review that finishes after the deadline cannot publish. A
stage runs under the shorter of its Stage timeout and the Job's remaining budget. When the budget is the
limit, the Job fails with `GENERATION_JOB_DEADLINE_EXCEEDED`, which is not retryable. Timeout cancels the
active context, closes the validation MySQL handle, and records
`DATASET_CREATION_TIMEOUT` or `DATASET_VALIDATION_TIMEOUT` (retryable) on a context detached from the stage
deadline; no partial Dataset Revision is
published. Any `VALIDATING_DATASET` error caused by the stage's own deadline records
`DATASET_VALIDATION_TIMEOUT`, and an interrupted load is never recorded as a validation rejection. Validation
MySQL errors that are not timeouts keep recording `VALIDATION_DB_UNAVAILABLE`. A Database Profile import (a Dataset read from an existing database, not an Agent's database)
also reports `DATASET_SNAPSHOT_UNSUPPORTED`, `DATASET_LIMIT_EXCEEDED`, and
`DATASET_SOURCE_CONFIG_INVALID` (not retryable), `DATASET_SOURCE_UNAVAILABLE` (retryable), and
`DATASET_SOURCE_NOT_CONFIGURED` when the Worker has no snapshot source configured.

## 4. Work Queue

`work_item` is the durable queue. `stage_key` is a stable stage name plus semantic attempt
ordinal, for example `VALIDATING_TEST_CASES:repair-1`. Workers use a short transaction at
`READ COMMITTED`:

```sql
START TRANSACTION;

SELECT id, fencing_token
FROM work_item
WHERE queue_name = ?
  AND available_at <= UTC_TIMESTAMP(6)
  AND (
    status = 'READY'
    OR (status = 'LEASED' AND lease_until < UTC_TIMESTAMP(6))
  )
ORDER BY priority DESC, created_at
LIMIT 1
FOR UPDATE SKIP LOCKED;

UPDATE work_item
SET status = 'LEASED',
    lease_owner = ?,
    lease_until = TIMESTAMPADD(SECOND, ?, UTC_TIMESTAMP(6)),
    fencing_token = fencing_token + 1,
    attempt_count = attempt_count + 1,
    updated_at = UTC_TIMESTAMP(6)
WHERE id = ?;

COMMIT;
```

Rules:

- A worker records the returned fencing token with every stage update.
- An update with a stale fencing token affects zero rows and the worker must stop.
- Heartbeat extends a lease only when owner and fencing token still match.
- Default lease is 60 seconds; heartbeat interval is 20 seconds.
- `attempt_count > max_attempts` moves the item to `DEAD` and its aggregate to the
  corresponding failure state.
- Queue retries use `available_at` with exponential backoff and jitter.

## 5. Generation State Transitions

All three `job_kind` values share one transition template; `<KIND>` is `SCENARIO`, `DATASET`, or
`TEST_CASES`, and `VALIDATING_INPUT` branches directly to that kind's `GENERATING_<KIND>`:

| From | Event | To | Side Effect |
|---|---|---|---|
| `PENDING` | input accepted | `VALIDATING_INPUT` | enqueue generation work |
| `VALIDATING_INPUT` | valid | `GENERATING_<KIND>` (per `job_kind`) | persist normalized request; verify referenced input Revisions are `ACTIVE`/`VALIDATED` for `DATASET`/`TEST_CASES` |
| `GENERATING_<KIND>` | draft stored | `VALIDATING_<KIND>` | persist draft |
| `VALIDATING_<KIND>` | deterministic + critic pass | `AWAITING_REVIEW` if `require_manual_review`, else `PUBLISHING` | freeze draft; for `TEST_CASES`, requires exact accepted count |
| `VALIDATING_<KIND>` | retryable rejection (deterministic or critic) | `REPAIRING_<KIND>` | enqueue bounded repair |
| `REPAIRING_<KIND>` | candidate produced | `VALIDATING_<KIND>` | increment repair round |
| `AWAITING_REVIEW` | reviewer approves | `PUBLISHING` | enqueue publisher (the re-queued work item is what a Worker consumes) |
| `AWAITING_REVIEW` | reviewer rejects | `FAILED` | persist review note and `REVIEW_REJECTED` error; fail the Pipeline |
| `PUBLISHING` | transaction commits | `COMPLETED` | expose published IDs |
| Any nonterminal | fatal error, or non-retryable/exhausted rejection | `FAILED` | retain drafts (24-hour `expires_at`); the manual purge removes them |
| Any pre-publish | cancel accepted | `CANCELLED` | stop new work; the manual purge removes drafts |

`VALIDATING_<KIND>` runs deterministic checks first (JSON Schema, SQL AST/execution, Tool
Validator depending on kind), then - only if those pass - one Semantic Critic pass scoped to the
kind's already-established context (Scenario alone; Scenario+Dataset; or Scenario+Dataset+all
Test Cases). Either check failing produces the same `VALIDATING_<KIND>` rejection and follows the
same repair-or-fail path; there is no separate Critic-only status.

Cancellation during an active LLM or SQL call is cooperative. `PUBLISHING` cannot be
cancelled after the transaction begins. `AWAITING_REVIEW` can be cancelled.

Entering `AWAITING_REVIEW` enqueues no work item, so no Worker holds the Job while it waits for a
human; `review_status` becomes `PENDING`. `POST /generation-jobs/{id}/review` is the only way
forward: approval and the `PUBLISHING` work item commit in one transaction. A review binds to the
exact Job ID it names and is never redirected to a later Pipeline stage, so a retried or stale
request cannot approve content the reviewer did not see; repeating a decision the Job already
carries returns it unchanged, and the opposite decision is a `409`.

A Generation Pipeline is not itself a state machine transition target: it is an orchestration
record that, on each chained Job's `COMPLETED`, creates the next Job in the chain (`SCENARIO` ->
`DATASET` -> `TEST_CASES`) with `input_scenario_revision_id`/`input_dataset_revision_id` set from
the prior Job's published output. Any chained Job's `FAILED` marks the Pipeline `FAILED` (and a
cancelled one marks it `CANCELLED`) without automatically retrying; output already published by
earlier Jobs is kept.

## 6. Atomic Publish Transaction

Publishing is scoped to one Job's own output, not to a whole Pipeline. The publisher performs
these steps in one Platform MySQL transaction:

1. Lock `generation_job` and verify state, fencing token, critic result, and (for `TEST_CASES`)
   accepted count.
2. Verify every draft digest again.
3. Insert the logical row(s) this Job's `job_kind` owns (Scenario, Dataset, or 1..N Test Cases),
   reusing an existing logical row when the Job targeted one (for example a new Test Case batch
   under an already-existing Dataset).
4. Insert immutable revision rows and their relationships (`dataset_revision.scenario_revision_id`,
   `test_case_revision.scenario_revision_id`/`dataset_revision_id`).
5. If this Job is the last Job of a Pipeline (its `job_kind` is `TEST_CASES` and `pipeline_id` is
   set), insert the `generation_package` manifest covering all three chained Jobs' output.
6. Change the new revision(s) to `ACTIVE` or `VALIDATED` according to `auto_activate`.
7. Mark the Job `COMPLETED`, store its published IDs and its provenance (`generation_job.provenance_json`:
   the producer record of the Scenario, Dataset, or final Test Case round, plus the accepted Critic
   review when one was configured), and link `published_package_id` when step 5 wrote the package.
8. Commit.

No external object store write occurs inside this transaction in the MVP. A standalone `DATASET`
or `TEST_CASES` Job (no `pipeline_id`) skips step 5 entirely; its output is immediately visible
and queryable through the normal Scenario/Dataset/Test Case listing endpoints without ever having
a `generation_package` row.

## 7. Test Run Workflow

A Run is created directly from a Test Case Revision and an Agent Version (`POST /api/v1/test-runs`); there is no Suite or Snapshot. The Test Case Revision already references its Scenario Revision and Dataset Revision.

1. Claim the Run work item.
2. Load the Test Case Revision, its Dataset Revision (`schema_sql`, `seed_sql`) and the Agent Version, with the Agent's database configuration.
3. Validate the Dataset's SQL against the allowlist, then create a dedicated `run_<ULID>` database on the **Agent's own database server** with the account configured on the Agent, and execute the Dataset's own `schema_sql` and `seed_sql` in it (technical/06 sections 3.2 and 3.3). Nothing is exported from or imported through Platform MySQL.
4. Verify preconditions against the new database.
5. Capture the before-state, then call the Agent once; its prompt names the Run database, and no database credential is sent. Capture its final output and structured Tool records.
6. Capture the after-state, execute postconditions and state-diff checks.
7. Persist all observable evidence in Platform MySQL.
8. Drop `run_<ULID>` in `finally`, close the Agent database connection, and record `environment_cleaned_at`. If the Worker dies first, the leftover sweep drops it (technical/06 section 6).
9. Enqueue an Evaluation work item.
10. Run deterministic matching and the LLM Judge from persisted evidence.

A Run is refused at creation with `409 AGENT_DATABASE_NOT_CONFIGURED` when its Agent has no database configured.

A **valid run** is one for which steps 2-4 passed and the Agent request started. A valid run
always receives an Evaluation Attempt, including `AGENT_ERROR` and `TIMEOUT`. Failures before
the Agent starts are `INVALID_TEST_DATA` or `INFRA_ERROR` and are excluded from Agent pass rate.

### 7.1 Test Run State Transitions

| From | Event | To | Required Action |
|---|---|---|---|
| `QUEUED` | worker claims lease | `PREPARING` | load the Test Case, Dataset and Agent Version |
| `PREPARING` | fixture and preconditions pass | `RUNNING` | call the Agent |
| `PREPARING` | fixture invalid | `INVALID_TEST_DATA` | cleanup, no Judge |
| `PREPARING` | infrastructure fails (retries exhausted) | `COMPLETED` | outcome `INFRA_ERROR`, cleanup, no Judge |
| `RUNNING` | Agent returns/errors/times out | `EVALUATING` | capture evidence, cleanup, enqueue Judge |
| `EVALUATING` | Evaluation succeeds | `COMPLETED` | persist verdict |
| `EVALUATING` | Judge retries exhausted | `EVALUATION_FAILED` | persist failed Attempt, no verdict |
| `QUEUED` or `PREPARING` | cancellation | `CANCELLED` | cleanup, no Judge if Agent never started |
| `RUNNING` | cancellation | `EVALUATING` | cancel request, capture evidence, cleanup, evaluate as cancelled execution |

`COMPLETED`, `INVALID_TEST_DATA`, `EVALUATION_FAILED`, and `CANCELLED` are terminal.

Re-evaluation creates a **new Run**: `POST /test-runs/{id}/evaluations` copies the source Run's execution evidence (outcome, output, state diff, deterministic results, Tool calls) into a new `EVALUATING` Run with a `PENDING` Evaluation Attempt for the chosen Judge model, and leaves the source Run untouched. The Agent is not called again.

Evaluation Attempt states: `PENDING` -> `RUNNING` -> `COMPLETED`, `FAILED` or `CANCELLED`.

### 7.2 Run environment lifecycle

There is no Run-scoped Tool token, grant or Gateway: the platform issues no credential to the Agent. The only credential the Runner uses
for the Run database is the Agent's configured account, resolved from the `secret` table when the Run starts and never written to a log or an Agent request.

- `run_<ULID>` is derived from the Run ID and is never reused. If it already exists when provisioning starts, the Runner drops that leftover (a previous attempt of the same Run, since a Run ID is never reused) and creates it again.
- Cleanup drops only the database it derived itself, with a bounded timeout, and ignores failure; the periodic sweep (technical/06 section 6) is the backstop.
- Dataset execution schemas (`dataset_*`) on validation MySQL are not Run databases and are still removed manually by an operator.

### 7.3 Content integrity

Revisions are immutable and carry a SHA-256 content digest, stored when they are published. A Run reads the Test Case, Dataset and Scenario
Revisions it references by ID at execution time. Because published Revisions cannot change, a Run is reproducible from the pair (Test Case Revision ID, Agent Version ID); no per-batch digest is needed.

## 8. Idempotency

| Operation | Idempotency Key |
|---|---|
| Create Generation Job | tenant + client request key |
| Store generation stage | job + stage + stage attempt |
| `create_test_case` | job + client case ID |
| Publish package | generation job ID |
| Create Runs | request `Idempotency-Key` + request digest (agent version, case revisions, repetitions); one row per case revision and repetition |
| Store Tool Call | run + sequence |
| Create Evaluation | run + attempt number; a re-evaluation is a new Run keyed by its request `Idempotency-Key` |

Same key and same request digest returns the existing resource. Same key with a different
digest returns HTTP `409 IDEMPOTENCY_KEY_REUSED`.

Stored generation output is keyed by job, kind, and repair round, not by stage attempt. A Job Stage
that runs again after a Lease takeover, before its transition was committed, reuses the output
already stored: generation and repair stages report the transition that output produced without
calling the model again, and a validation that finds its Dataset execution schema or accepted Critic review already
stored does not provision the schema or call the Critic again. A TEST_CASES stage is complete only once its `TEST_CASE_MANIFEST` is stored;
candidates from an attempt that never reached the manifest are not listed in it and are never
published.
