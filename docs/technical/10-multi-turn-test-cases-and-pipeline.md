# Multi-turn Test Cases and a Composable Run Pipeline

## 1. Motivation

09 reduced evaluation to a single LLM Judge over the Target's session context, but kept the run
model **single-turn**: one Test Case has one `prompt`, one `expected_output`, and one
`evaluation_criteria` set; the run stage makes one `POST /v1/chat` call and the judge stage scores
that one exchange once.

Real Agent evaluations are conversational: a task often requires the Agent to answer, receive a
follow-up that depends on its previous answer, and continue. To exercise this the platform needs

1. **multi-turn Test Cases** — the generation LLM emits a sequence of turns, each with its own
   prompt, expectation, and scoring criteria;
2. a **run pipeline that interleaves execution and judging** — `init → turn#1 → judge#1 → turn#2 →
   judge#2 → … → finish` — where the judge's verdict gates progress: only `pass` advances to the next
   turn, otherwise the pipeline stops;
3. a judge that reads **the whole session context from the start up to the current turn**, not just
   the current turn, so follow-up turns are judged against the accumulated conversation;
4. **`init` / `finish` lifecycle nodes** — the test environment (knowledge documents and the fixed
   database) is provisioned once in `init` and torn down once in `finish`, not per turn; and
5. a pipeline that is **composable from nodes with static and runtime-conditional dependencies**
   rather than hard-coded turn loops, and that can **repeat the whole init→…→finish sequence**
   several times within a single Run.

Scope: test platform only. The Target Agent continues to implement the chat SSE and the session
context API through its adapter; the platform defines the contract and drives the pipeline.

## 2. Multi-turn Test Case model

The published Test Case content changes from flat single-turn fields to a `turns` array. Case-level
metadata stays on the case; per-turn data moves into each turn.

```json
{
  "schema_version": "2.0",
  "scenario_revision_id": "01K4Z6Y7X8W9V0T1S2R3Q4P5N6",
  "dataset_revision_id": "01K4Z6Y7X8W9V0T1S2R3Q4P5N7",
  "turns": [
    {
      "turn": 1,
      "prompt": "Analyze order O10001 and report its status.",
      "expected_output": { "type": "SEMANTIC", "value": "The response states order O10001 is PAID." },
      "evaluation_criteria": [
        { "name": "correct_status", "description": "Reports the correct status.", "weight": 0.6 },
        { "name": "clear_answer", "description": "Names the order and its state.", "weight": 0.4 }
      ]
    },
    {
      "turn": 2,
      "prompt": "Now cancel that same order and confirm.",
      "expected_output": { "type": "SEMANTIC", "value": "The response confirms the order was cancelled." },
      "evaluation_criteria": [
        { "name": "cancelled", "description": "Confirms cancellation.", "weight": 0.7 },
        { "name": "correct_order", "description": "Refers to the right order.", "weight": 0.3 }
      ]
    }
  ],
  "knowledge_documents": [],
  "difficulty": "MEDIUM",
  "difficulty_profile": { "data_complexity": 2, "reasoning_complexity": 2, "tool_complexity": 2, "ambiguity": 2, "multi_step": 4, "state_change_complexity": 3, "adversarial_complexity": 1 },
  "generation_assumptions": []
}
```

Rules:

1. `turns` has 1–16 entries; `turn` starts at 1 and strictly increases by 1. A single-turn case is
   the one-element special case, so the model is a superset of 09.
2. Each turn carries its own `prompt`, `expected_output`, and `evaluation_criteria`. The last turn's
   expectation is the final answer; intermediate turns carry the correct intermediate result, which
   is what lets the generator write a follow-up that *depends on* the prior turn.
3. `expected_output` and `evaluation_criteria` reuse the 09 `$defs` (EXACT / SEMANTIC / JSON /
   CONTAINS / CONSTRAINT, and the weighted criterion). Criteria weights must sum to 1 per turn.
4. `knowledge_documents` (≤8), `difficulty`, `difficulty_profile`, and `generation_assumptions`
   stay case-level. Knowledge is uploaded once in the `init` node and shared by every turn of every
   repetition; its document IDs are the teardown handles stored in the pipeline context.
5. `client_case_id` remains the generator's stable case identity; `job_id`,
   `scenario_revision_id`, and `dataset_revision_id` are unchanged.

### 2.1 Generator contract

`create-test-case.schema.json` (the generator Tool input) and `test-case.schema.json` (published
content) both move to `schema_version` `2.0` with the `turns` array. The generator system prompt
now instructs the model to emit 1–16 turns, where turn N+1's prompt references the outcome expected
in turn N, and to keep every turn self-scorable (its own `expected_output` + `evaluation_criteria`).
The `turns` count, `knowledge_documents` count (≤8), and the schema's bounds are all enforced at
generation time (contract validation + programmatic checks) so oversized test data never reaches a
Run.

## 3. Run pipeline: nodes, edges, conditions, config, context, result

The run is no longer a hard-coded `execute → judge` pair. It is a small DAG whose nodes are
executable units and whose edges carry static or runtime conditions. The pipeline engine walks the
DAG; the node *types* are pluggable so the engine is not specific to turns.

### 3.1 Pipeline definition

```json
{
  "schema_version": "1.0",
  "config": { "repetitions": 3 },
  "nodes": [
    { "id": "init",    "type": "init",    "params": {} },
    { "id": "turn-1",  "type": "turn",    "params": { "turn": 1 } },
    { "id": "judge-1", "type": "judge",   "params": { "turn": 1 } },
    { "id": "turn-2",  "type": "turn",    "params": { "turn": 2 } },
    { "id": "judge-2", "type": "judge",   "params": { "turn": 2 } },
    { "id": "finish",  "type": "finish",  "params": {} }
  ],
  "edges": [
    { "from": "init",    "to": "turn-1" },
    { "from": "turn-1",  "to": "judge-1" },
    { "from": "judge-1", "to": "turn-2", "when": { "field": "pass", "equals": true } },
    { "from": "judge-1", "to": "finish", "when": { "field": "pass", "equals": false } },
    { "from": "turn-2",  "to": "judge-2" },
    { "from": "judge-2", "to": "finish" }
  ],
  "context": {
    "iteration": 1,
    "round_passed": true,
    "knowledge_document_ids": [],
    "database_tables": [],
    "iteration_scores": []
  },
  "result": []
}
```

- **Node** — `id` is unique within the pipeline; `type` selects a registered executor (`init`,
  `turn`, `judge`, `finish` for v1); `params` is a free-form object carrying executor-specific
  parameters (`{ "turn": 1 }` for the `turn` and `judge` executors, empty for `init`/`finish`).
- **Edge** — `from → to`. An edge without `when` is a static dependency: `to` runs once `from`
  completes successfully. An edge with `when` is a runtime condition evaluated against `from`'s
  output; `to` runs only when the condition holds.
- **Condition** — a minimal predicate for v1: `{ "field": <output field>, "equals": <value> }`,
  evaluated against the `from` node's output object. This is enough for turn gating and the fail
  path into `finish`; richer expressions are a later extension, not a schema break (the `when`
  shape is versioned).
- **Config** — `repetitions`: how many times the whole `init → … → finish` sequence runs inside this
  single Run. Bounded 1–10.
- **Context** — mutable per-Run runtime state (see §3.2).
- **Result** — the append-only record of every node execution (see §3.3), used to render the report.

### 3.2 Pipeline context

The `context` object is the Run's mutable runtime state, persisted inside `pipeline_json` (the
platform database). It is deliberately small — its teardown handles are bounded so the pipeline
record does not balloon.

| field | meaning |
|---|---|
| `iteration` | 1-based index of the current repetition. Starts at 1; incremented after a `finish` that may continue. |
| `round_passed` | whether every `judge` in the current iteration passed (still `true` until a judge fails). |
| `knowledge_document_ids` | the Target document IDs provisioned by `init`, used by `finish` to delete them (≤8). |
| `database_tables` | the fixed-database table names provisioned by `init`, used by `finish` to drop them (≤20). |
| `iteration_scores` | the `final_score` of the last judge of each iteration that reached it, for the final average. |

The teardown handles (`knowledge_document_ids`, `database_tables`) are written by `init` and cleared
by `finish` **only when the teardown succeeds**; a failed teardown keeps them so the next
iteration's `init` can locate and clean the residual data. `iteration_scores` grows at most one float
per iteration (≤10), and `result` (§3.3) is the bounded report.

### 3.3 Node outputs and results

Every node records a **structured output** that later conditions and the final verdict read:

| node | output fields |
|---|---|
| `init` | `status`, `knowledge_document_ids`, `database_tables` |
| `turn` | `status` (`COMPLETED` / `TIMEOUT` / `AGENT_ERROR` / `CANCELLED`), `session_id`, `final_output`, `context_watermark` |
| `judge` | `pass` (`bool`), `llm_score`, `final_score` |
| `finish` | `status` |

Every node also appends one short **result record** to `result` for the report. A record carries
only `iteration`, `node_id`, `type`, `status`, `started_at`, `finished_at`, and `error` (truncated)
— plus, for `judge`, `pass` and `final_score`. A bypassed node (after a failure) records a minimal
`status = "SKIP"` with no timestamps or scores. The result never stores the test prompt, the LLM
output, or the judge's reasoning, so the report stays small and free of sensitive payloads. `error`
is capped to a short length and truncated when necessary; timestamps use second precision and
scores round to two decimals to keep every record short.

### 3.4 Execution semantics

The engine maintains, per Run, the completed nodes and their outputs. After a node completes it
evaluates each outgoing edge; every enabled `to` node whose static dependencies are all satisfied is
eligible. Because turn gating is expressed as `judge-N → turn-(N+1) when pass`, and a failed judge
routes to `finish` through the `when pass == false` edge, a failing judge enables no further turn
and the iteration **stops** at `finish`.

One iteration is the full linear sequence `init → turn-1 → judge-1 → … → turn-N → judge-N → finish`.
Whatever node fails, the remaining `turn`/`judge` nodes are recorded as `SKIP`, and **`finish`
always runs** as the guaranteed teardown. After `finish`:

- if the iteration passed every judge **and** `iteration < repetitions`, the engine increments
  `iteration`, resets `round_passed`, and re-runs from `init` (the next iteration);
- otherwise the Run is terminal.

The terminal verdict:

- Each iteration's score is the `final_score` of the **last judge node in the pipeline** (the judge
  of the final turn). An iteration that failed before reaching the last judge — or an interrupted
  iteration that never ran — scores 0.
- `final_score` = the arithmetic mean of every iteration's score **over the fixed `repetitions`
  count**, including the zero-scoring ones.
- `pass` = `true` iff every configured iteration reached its last judge and every judge passed
  (i.e. `iteration_scores` holds `repetitions` entries and `round_passed` is true). A node failure
  or a failed judge stops the pipeline immediately and does **not** advance to the next iteration.

No parallel execution is supported in v1: the engine walks the DAG one node at a time, and the DAG
derived from `turns` is linear.

### 3.5 Pipeline generation (v1) and extensibility

For v1 the platform **derives** the pipeline from the Test Case's `turns` and the requested
`repetitions`: nodes `init, turn-1, judge-1, turn-2, judge-2, …, turn-N, judge-N, finish`; static
edges `init → turn-1`, `turn-N → judge-N`, `judge-N → finish` (the last judge always reaches
finish); conditional edges `judge-K → turn-(K+1) when pass == true` and `judge-K → finish when pass
== false`. The derivation is a pure function of `turns` and `repetitions`, not a hard-coded loop: the
executor consumes a generic DAG, so the turn/judge interleave is data, not control flow.

Node types are registered (`init`, `turn`, `judge`, `finish` for v1). The pipeline **data structure
is intentionally generic and multi-node**: `nodes` is a collection of typed nodes and `edges` a set
of dependency relations, so it can express any DAG without a schema change. `type` is an open string
resolved through the node registry, not a closed enum baked into the DAG. There is **no manual
pipeline authoring in v1**: the platform derives the pipeline from `turns` and the user never sees
the DAG. Adding a node type later only registers a new executor; the engine and persistence are
unchanged. Parallel node execution is out of scope for v1.

### 3.6 Pipeline storage and validation

The pipeline DAG is a first-class, schema-validated JSON document, not a Go-only structure. It is
stored as a JSON string, and every write/read path validates it against `pipeline.schema.json`.

**Storage.** `test_run` gains `pipeline_json` (the DAG + `config` + `context` + `result`). The
pipeline is generated when the Run is created — the user clicks run — from the Test Case's `turns`
and the requested `repetitions`, and is stored on the Run record. The DAG (`nodes`/`edges`/`config`)
is the immutable execution plan for that Run; `context` and `result` are the Run's live progress,
rewritten in place as nodes complete. The Test Case stores only `turns`; the pipeline is the Run's
derived execution plan and progress record.

**Schema.** `pipeline.schema.json` constrains the generic shape — `schema_version`, `config`
(`repetitions` 1–10), `nodes` (1–64, each with a unique `id`, a `type` matching the node-name
pattern, and a free-form `params` object), `edges` (≤128, each `from`/`to` plus an optional `when`
predicate), `context`, and `result`. `params` is deliberately an open object: `type` is
registry-resolved, so the DAG schema validates the common shape while each node executor validates
its own `params` with a type-specific schema.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://agent-test-platform.local/schemas/pipeline.schema.json",
  "title": "Run Pipeline Definition",
  "type": "object",
  "additionalProperties": false,
  "required": ["schema_version", "config", "nodes", "edges", "context", "result"],
  "properties": {
    "schema_version": {"const": "1.0"},
    "config": {
      "type": "object",
      "additionalProperties": false,
      "required": ["repetitions"],
      "properties": {
        "repetitions": {"type": "integer", "minimum": 1, "maximum": 10}
      }
    },
    "nodes": {"type": "array", "minItems": 1, "maxItems": 64, "items": {"$ref": "#/$defs/node"}},
    "edges": {"type": "array", "maxItems": 128, "items": {"$ref": "#/$defs/edge"}},
    "context": {
      "type": "object",
      "additionalProperties": false,
      "required": ["iteration", "round_passed", "knowledge_document_ids", "database_tables", "iteration_scores"],
      "properties": {
        "iteration": {"type": "integer", "minimum": 1},
        "round_passed": {"type": "boolean"},
        "knowledge_document_ids": {"type": "array", "maxItems": 8, "items": {"type": "string"}},
        "database_tables": {"type": "array", "maxItems": 20, "items": {"type": "string"}},
        "iteration_scores": {"type": "array", "maxItems": 10, "items": {"type": "number"}}
      }
    },
    "result": {"type": "array", "maxItems": 1280, "items": {"$ref": "#/$defs/nodeResult"}}
  },
  "$defs": {
    "node": {
      "type": "object",
      "additionalProperties": false,
      "required": ["id", "type", "params"],
      "properties": {
        "id": {"type": "string", "pattern": "^[a-z][a-z0-9_.-]{0,127}$"},
        "type": {"type": "string", "pattern": "^[a-z][a-z0-9_.-]{0,127}$"},
        "params": {"type": "object"}
      }
    },
    "edge": {
      "type": "object",
      "additionalProperties": false,
      "required": ["from", "to"],
      "properties": {
        "from": {"type": "string", "pattern": "^[a-z][a-z0-9_.-]{0,127}$"},
        "to": {"type": "string", "pattern": "^[a-z][a-z0-9_.-]{0,127}$"},
        "when": {"$ref": "#/$defs/condition"}
      }
    },
    "condition": {
      "type": "object",
      "additionalProperties": false,
      "required": ["field", "equals"],
      "properties": {
        "field": {"type": "string", "minLength": 1, "maxLength": 64},
        "equals": {}
      }
    },
    "nodeResult": {
      "type": "object",
      "additionalProperties": false,
      "required": ["iteration", "node_id", "type", "status"],
      "properties": {
        "iteration": {"type": "integer", "minimum": 1},
        "node_id": {"type": "string", "pattern": "^[a-z][a-z0-9_.-]{0,127}$"},
        "type": {"type": "string", "pattern": "^[a-z][a-z0-9_.-]{0,127}$"},
        "status": {"type": "string", "minLength": 1, "maxLength": 32},
        "pass": {"type": "boolean"},
        "final_score": {"type": "number"},
        "started_at": {"type": "string", "format": "date-time"},
        "finished_at": {"type": "string", "format": "date-time"},
        "error": {"type": "string", "maxLength": 512}
      }
    }
  }
}
```

**Validation boundary.** All three paths go through the same contract validator:

1. **Generate** — derive the pipeline from `turns` + `repetitions`, then
   `ValidateJSON("pipeline.schema.json", json)` before persisting (validate-by-construction). A DAG
   that fails the schema is rejected and never stored.
2. **Update** — every node completion rewrites `context`/`result` and re-validates before
   persisting, so `pipeline_json` is always a conforming document.
3. **Read** — load `pipeline_json` and re-`ValidateJSON` before unmarshalling into the Go
   `Pipeline` type, so a corrupt or schema-drifted row fails fast instead of mis-executing.

Beyond what a JSON schema can express, programmatic checks run on generate and again on read: node
ids are unique, `from`/`to` reference existing node ids, the graph is acyclic, every `type` is
registered, each node's `params` passes its executor's type-specific schema, and the node set matches
`turns` (one `init`, one `finish`, and a `turn`/`judge` pair per turn). The engine therefore only
ever executes a validated DAG.

## 4. Session context API adjustments

The 09 context API is session-scoped, but two multi-turn requirements change its rules.

1. **Non-terminal sessions are readable.** 09 returned `409 SESSION_NOT_TERMINAL` for a live
   session. A multi-turn judge must run *between* turns, while the session is still open. The
   endpoint now returns the context produced so far (a point-in-time snapshot) at any time; the
   `409` is removed. The platform controls scope through the watermark below, not through terminal
   status.
2. **`until_sequence` bounds the read.** `GET /v1/sessions/{id}/context?cursor=…&limit=…&until_sequence=…`
   returns only items with `sequence ≤ until_sequence`. `turn` records `context_watermark` (the last
   sequence after its turn), and `judge-N` reads with `until_sequence = watermark of turn N`, so a
   judge always sees the session **from the start up to its turn** and never leaks later turns.

```http
GET {target_base_url}/v1/sessions/{sessionId}/context?until_sequence=12&limit=100
Authorization: Bearer <target token>
X-ATP-Contract-Version: 2.0
```

The flat, strictly-increasing `sequence`, the `MESSAGE` / `TOOL_CALL` / `TOOL_RESULT` item types,
pagination, and the 100 000-item ceiling are unchanged from 09.

### 4.1 Continuing a session (chat)

`target-chat-request` gains an optional `session_id`. Turn 1 omits it; turns 2+ send the
`session_id` returned by turn 1's `response.completed`, continuing the same session. `run_id` stays
the per-request idempotency key.

## 5. Lifecycle, run, and judge stages (per node)

The run worker no longer executes "the whole case" in one chat call; it executes **one node** at a
time. The three run-queue node types are:

1. **`init`** — provisions the fixed database and uploads the case's knowledge documents (08 §3,
   §5). If the previous iteration's teardown failed, its residual handles are still in `context`, so
   `init` cleans those up first; when there are no handles it never wipes the database wholesale
   (it cannot locate the test data). It returns the new teardown handles (`knowledge_document_ids`,
   `database_tables`), which the engine writes into `context` so `finish` can tear them down. No
   chat call is made.
2. **`turn`** — executes one turn. Turn 1 sends `POST /v1/chat` without `session_id`; turn N>1 sends
   the stored `session_id`. The message is built per turn from that turn's `prompt` plus the
   database-name suffix (08); the `model`, deadline, and knowledge ids are the same for every turn.
   Each turn persists its `final_output` and `context_watermark`. `final_output` is display-only; the
   judge reads the context API.
3. **`finish`** — tears down the test environment: deletes the knowledge documents named in
   `context.knowledge_document_ids` and drops the tables named in `context.database_tables`. It then
   drives the repetition loop (§3.4). If the teardown fails, the handles are **kept** in `context`
   so the next iteration's `init` can clean the residual data; a successful teardown clears them.

The judge worker executes **one judge node** per turn:

1. It reads the context with `until_sequence = watermark(turn N)`, i.e. the whole session from the
   first message to the end of turn N — including every prior turn's user and assistant messages.
2. It builds the judge request from that turn's `prompt` / `expected_output` / `evaluation_criteria`,
   the accumulated `session_context`, the turn's `execution_outcome`, and two new fields
   `turn` and `total_turns` so the Judge knows it is scoring turn N of M, not a standalone exchange.
3. The LLM Judge scores the turn's criteria; `pass = llm_score ≥ judge_threshold` (default 70), as in
   09. There is still no deterministic gate.
4. The verdict is written to an `evaluation_attempt` tagged with `turn_no`.
5. The engine then evaluates the turn's outgoing edge: `pass` → the next turn (or `finish` for the
   last turn); `fail` → `finish`, skipping the remaining `turn`/`judge` nodes (`SKIP`).

```text
init ─▶ turn-1 ─▶ judge-1 ──pass──▶ turn-2 ─▶ judge-2 ──pass──▶ finish ──(iteration < repetitions)──▶ init …
              │                │
              └─fail──▶ finish ┘─fail──▶ finish
                       (no further iteration)
```

## 6. Data model and migration

Breaking change, `contract_version` `1.0 → 2.0` on the Target contract and the platform schemas it
touches. `migrations/0022_multi_turn_pipeline.sql`:

- `test_case_revision.content_json` — now `schema_version` `2.0` with `turns` (existing rows are
  back-filled into a single-element `turns` array during migration, or re-generated).
- `test_run` — add `pipeline_json` (the DAG + `config` + `context` + `result`, §3.6) and
  `turns_total`. Remove the redundant `current_turn_no`/`pipeline_sha256` columns added by the
  earlier draft (progress lives in `context`/`result`).
- New schema `pipeline.schema.json` — the DAG shape plus `config`/`context`/`result`.
- New table `test_run_turn`:
  - `id`, `test_run_id`, `iteration` (1-based, which repetition this turn belongs to),
    `turn_no` (1-based, unique per run+iteration),
  - `prompt`, `status` (`PENDING` / `RUNNING` / `JUDGING` / `PASSED` / `FAILED` / `ERROR` /
    `CANCELLED`),
  - `final_output`, `output_sha256`, `context_watermark` (nullable),
  - `llm_score`, `final_score`, `pass` (nullable), `error_json` (nullable),
  - `started_at`, `finished_at`.
- `evaluation_attempt` — add nullable `turn_no` so each judge attempt is attributable to a turn.
- `target-chat-request.schema.json` — add optional `session_id`.
- `target-session-context-response.schema.json` — accept `until_sequence` (optional) and drop the
  terminal-only `409`.
- `judge-request.schema.json` — add `turn` and `total_turns`.

## 7. Scheduling

The pipeline DAG is generated and schema-validated when the Run is created (the user clicks run) and
stored on the Run record; the worker then executes its nodes through the two existing queues:

1. `run` queue carries `init`, `turn`, and `finish` nodes; the run worker executes one node and, on
   settle, evaluates the node's outgoing edges and enqueues the next node (`init → turn-1`,
   `turn → judge`, `finish → init` of the next iteration, or terminal).
2. `evaluation` queue carries `judge` nodes; the judge worker executes one judge and, on settle,
   evaluates the gate and enqueues the next turn (on `run`), or `finish`.

The `work_item.stage_key` identifies the node, prefixed with the iteration so the same node id can
run again in a later iteration without colliding under `uq_work_item_stage` (e.g. `1:init`,
`1:turn-1`, `1:judge-1`, `1:finish`, `2:init`, …). The pipeline state lives in `test_run` +
`test_run_turn`; the engine only needs the node id, the pipeline definition, and the node outputs,
all persisted. A node is re-enqueued on retry exactly like today's work items (lease + fencing +
attempt count), so a mid-pipeline crash or retry resumes from the current node without re-running
earlier turns. The `cancel_requested_at` flag and the per-Agent concurrency guard (08 §5) are
unchanged.

## 8. Compatibility and scope

- Single-turn cases remain valid: a one-element `turns` yields the
  `init → turn-1 → judge-1 → finish` pipeline, identical in behavior to 09 when `repetitions = 1`.
  This keeps `Reevaluate` and `Benchmark` working: a multi-turn run still exposes one terminal
  `final_score` / `pass` on `test_run` (the §3.4 aggregation), so benchmark aggregation needs no
  change.
- The old `repetitions` request field (which created N independent Runs) is removed: a single Run
  now repeats internally via `pipeline.config.repetitions`, and the platform never runs the same
  case concurrently or in parallel. The `uq_test_run_identity` unique key keeps `repetition_no = 1`.
- `Reevaluate` re-judges using the stored `session_id` and per-turn watermarks with a chosen Judge
  alias; it re-runs the `judge` nodes only (execution is not repeated).
- Supersedes 09 §10: multi-turn is no longer "future", it is this spec. 09's single-turn prose stays
  valid as the one-turn case.
- Out of scope for v1: node types beyond `init`/`turn`/`judge`/`finish` (the generic DAG anticipates
  them, but only these executors ship), parallel node execution, and turn-level retry isolation.
  Manual pipeline authoring is deliberately **not** planned — the pipeline is always derived from
  `turns` + `repetitions`; the multi-node data structure keeps the engine general, it does not
  expose a pipeline editor.
