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
2. a **run pipeline that interleaves execution and judging** — `turn#1 → judge#1 → turn#2 →
   judge#2 → …` — where the judge's verdict gates progress: only `pass` advances to the next turn,
   otherwise the pipeline stops;
3. a judge that reads **the whole session context from the start up to the current turn**, not just
   the current turn, so follow-up turns are judged against the accumulated conversation; and
4. a pipeline that is **composable from nodes with static and runtime-conditional dependencies**
   rather than hard-coded turn loops.

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
4. `knowledge_documents`, `difficulty`, `difficulty_profile`, and `generation_assumptions` stay
   case-level. Knowledge is uploaded once in the run stage and shared by every turn.
5. `client_case_id` remains the generator's stable case identity; `job_id`,
   `scenario_revision_id`, and `dataset_revision_id` are unchanged.

### 2.1 Generator contract

`create-test-case.schema.json` (the generator Tool input) and `test-case.schema.json` (published
content) both move to `schema_version` `2.0` with the `turns` array. The generator system prompt
now instructs the model to emit 1–16 turns, where turn N+1's prompt references the outcome expected
in turn N, and to keep every turn self-scorable (its own `expected_output` + `evaluation_criteria`).

## 3. Run pipeline: nodes, edges, conditions

The run is no longer a hard-coded two-step `execute → judge`. It is a small DAG whose nodes are
executable units and whose edges carry static or runtime conditions. The pipeline engine walks the
DAG; the node *types* are pluggable so the engine is not specific to turns.

### 3.1 Pipeline definition

```json
{
  "nodes": [
    { "id": "turn-1", "type": "turn", "params": { "turn": 1 } },
    { "id": "judge-1", "type": "judge", "params": { "turn": 1 } },
    { "id": "turn-2", "type": "turn", "params": { "turn": 2 } },
    { "id": "judge-2", "type": "judge", "params": { "turn": 2 } }
  ],
  "edges": [
    { "from": "turn-1",  "to": "judge-1" },
    { "from": "judge-1", "to": "turn-2", "when": { "field": "pass", "equals": true } },
    { "from": "turn-2",  "to": "judge-2" }
  ]
}
```

- **Node** — `id` is unique within the pipeline; `type` selects a registered executor; `params` is a
  free-form object carrying the executor-specific parameters (`{ "turn": 1 }` for the `turn` and
  `judge` executors).
- **Edge** — `from → to`. An edge without `when` is a static dependency: `to` runs once `from`
  completes successfully. An edge with `when` is a runtime condition evaluated against `from`'s
  output; `to` runs only when the condition holds.
- **Condition** — a minimal predicate for v1: `{ "field": <output field>, "equals": <value> }`,
  evaluated against the `from` node's output object. This is enough for turn gating; richer
  expressions are a later extension, not a schema break (the `when` shape is versioned).

### 3.2 Node outputs

Every node records a structured output that later conditions and the final verdict read:

| node | output |
|---|---|
| `turn` | `status` (`COMPLETED` / `TIMEOUT` / `AGENT_ERROR` / `CANCELLED`), `session_id`, `final_output`, `context_watermark` (highest context `sequence` after this turn) |
| `judge` | `pass` (`bool`), `llm_score`, `final_score`, `criteria` results |

### 3.3 Execution semantics

The engine maintains, per run, the completed nodes and their outputs. After a node completes it
evaluates each outgoing edge; every enabled `to` node whose static dependencies are all satisfied is
eligible. Because turn gating is expressed as `judge-N → turn-(N+1) when pass`, a failing judge
enables no further node and the pipeline **stops** — that turn's failure becomes the run's terminal
outcome.

- All turns pass → the pipeline runs to completion → `pass = true`.
- A turn fails or errors → no further node is enabled → the pipeline stops → `pass = false`.
- The run's terminal `pass` and `final_score` come from the **last turn that reached a judge
  verdict** (the final turn on success, the failing turn on a stopped pipeline). This is deliberate:
  every judge reads the whole session context from the start up to its turn, so the last judge has
  already seen the complete conversation and its score is the overall score — no averaging across
  turns is needed.

### 3.4 Pipeline generation (v1) and extensibility

For v1 the platform **derives** the pipeline from the Test Case's `turns` — nodes
`turn-1, judge-1, turn-2, judge-2, …`, static edges `turn-N → judge-N`, and conditional edges
`judge-N → turn-(N+1) when pass`. The derivation is a pure function of `turns`, not a hard-coded
loop: the executor consumes a generic DAG, so the turn/judge interleave is data, not control flow.

Node types are registered (`turn`, `judge` for v1). The pipeline **data structure is intentionally
generic and multi-node**: `nodes` is a collection of typed nodes and `edges` a set of dependency
relations, so it can express any DAG — more than two node kinds, branches, several judges, or future
node types — without a schema change. `type` is an open string resolved through the node registry,
not a closed enum baked into the DAG. There is **no manual pipeline authoring in v1**: the platform
derives the pipeline from `turns` and the user never sees the DAG. Adding a node type later (a
delay, a fixed message, a second judge for a different alias) only registers a new executor; the
engine and persistence are unchanged.

### 3.5 Pipeline storage and validation

The pipeline DAG is a first-class, schema-validated JSON document, not a Go-only structure. It is
stored as a JSON string with a content digest, and every write/read path validates it against
`pipeline.schema.json`.

**Storage.** `test_run` gains `pipeline_json` (the DAG) and `pipeline_sha256` (its digest). The
pipeline is generated when the Run is created — the user clicks run — from the Test Case's `turns`
at that moment, and is stored on the Run record as a point-in-time snapshot of the execution plan.
Each Run therefore carries its own pipeline; the Test Case stores only `turns`, and the pipeline is
the Run's derived execution plan, immutable for the life of that Run.

**Schema.** `pipeline.schema.json` constrains the generic shape — `schema_version`, `nodes` (1–64,
each with a unique `id` and a `type` matching the node-name pattern, plus a free-form `params`
object), and `edges` (≤128, each `from`/`to` plus an optional `when` predicate). `params` is
deliberately an open object: `type` is registry-resolved, so the DAG schema validates the common
shape while each node executor validates its own `params` with a type-specific schema.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://agent-test-platform.local/schemas/pipeline.schema.json",
  "title": "Run Pipeline Definition",
  "type": "object",
  "additionalProperties": false,
  "required": ["schema_version", "nodes", "edges"],
  "properties": {
    "schema_version": {"const": "1.0"},
    "nodes": {"type": "array", "minItems": 1, "maxItems": 64, "items": {"$ref": "#/$defs/node"}},
    "edges": {"type": "array", "maxItems": 128, "items": {"$ref": "#/$defs/edge"}}
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
    }
  }
}
```

**Validation boundary.** All three paths go through the same contract validator:

1. **Generate** — derive the pipeline from `turns`, then `ValidateJSON("pipeline.schema.json", json)`
   before persisting (validate-by-construction). A DAG that fails the schema is rejected and never
   stored.
2. **Update** — any later regeneration/rewrite re-validates before persisting, so `pipeline_json`
   is always a conforming document.
3. **Read** — load `pipeline_json` and re-`ValidateJSON` before unmarshalling into the Go `Pipeline`
   type, so a corrupt or schema-drifted row fails fast instead of mis-executing.

Beyond what a JSON schema can express, programmatic checks run on generate and again on read: node
ids are unique, `from`/`to` reference existing node ids, the graph is acyclic, every `type` is
registered, and each node's `params` passes its executor's type-specific schema. The engine
therefore only ever executes a validated DAG.

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

## 5. Run stage (per turn)

The run worker no longer executes "the whole case" in one chat call; it executes **one turn node**.

1. Turn 1: `POST /v1/chat` without `session_id`; `response.completed` yields `session_id` (stored on
   `test_run`) and `final_output`.
2. Turn N>1: `POST /v1/chat` with the stored `session_id`; the Target continues the same session.
3. Each turn persists its own `final_output` and `context_watermark` on the turn record. `final_output`
   is display-only; the judge reads the context API.
4. The message is built per turn from that turn's `prompt` plus the database-name suffix (08); the
   `model`, deadline, and knowledge ids are the same for every turn.
5. Knowledge upload (08 §3) still happens once, before turn 1, and is torn down after the pipeline
   ends. The fixed database is provisioned once before turn 1 and cleaned after the pipeline.

## 6. Judge stage (per turn, gated)

The judge worker executes **one judge node** for its turn.

1. It reads the context with `until_sequence = watermark(turn N)`, i.e. the whole session from the
   first message to the end of turn N — including every prior turn's user and assistant messages.
2. It builds the judge request from that turn's `prompt` / `expected_output` / `evaluation_criteria`,
   the accumulated `session_context`, the turn's `execution_outcome`, and two new fields
   `turn` and `total_turns` so the Judge knows it is scoring turn N of M, not a standalone exchange.
3. The LLM Judge scores the turn's criteria; `pass = llm_score ≥ judge_threshold` (default 70), as in
   09. There is still no deterministic gate.
4. The verdict is written to the turn record and to an `evaluation_attempt` tagged with `turn_no`.
5. The engine then evaluates the turn's outgoing edge: `pass` → enqueue the next turn; `fail` (or no
   further turn) → the run is settled terminal.

```text
turn-1 ─▶ judge-1 ──pass──▶ turn-2 ─▶ judge-2 ──pass──▶ … ──▶ completed
              │
              └─fail (or error)──▶ pipeline stops, run terminal (pass = false)
```

## 7. Data model and migration

Breaking change, `contract_version` `1.0 → 2.0` on the Target contract and the platform schemas it
touches. `migrations/0022_multi_turn_pipeline.sql`:

- `test_case_revision.content_json` — now `schema_version` `2.0` with `turns` (existing rows are
  back-filled into a single-element `turns` array during migration, or re-generated).
- `test_run` — add `pipeline_json` + `pipeline_sha256` (the execution-plan snapshot, §3.5),
  `current_turn_no`, and `turns_total`.
- New schema `pipeline.schema.json` — the DAG shape (nodes / edges / `when` predicate).
- New table `test_run_turn`:
  - `id`, `test_run_id`, `turn_no` (1-based, unique per run),
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

## 8. Scheduling

The pipeline DAG is generated and schema-validated when the Run is created (the user clicks run) and
stored on the Run record; the worker then executes its nodes through the two existing queues, which
map cleanly onto the two node types:

1. `run` queue carries `turn` nodes; the run worker executes one turn and, on settle, enqueues the
   turn's `judge` node on the `evaluation` queue.
2. `evaluation` queue carries `judge` nodes; the judge worker executes one judge and, on settle,
   evaluates the gate and either enqueues the next `turn` node on the `run` queue or settles the run
   terminal.

The pipeline state lives in `test_run` + `test_run_turn`; the engine only needs the node id, the
pipeline definition (stored on `test_run`), and the node outputs, all persisted. A node is
re-enqueued on retry exactly like today's work items (lease + fencing + attempt count), so a
mid-pipeline crash or retry resumes from the current node without re-running earlier turns. The
`cancel_requested_at` flag and the per-Agent concurrency guard (08 §5) are unchanged.

## 9. Compatibility and scope

- Single-turn cases remain valid: a one-element `turns` yields the `turn-1 → judge-1` pipeline,
  identical in behavior to 09. This keeps `Reevaluate` and `Benchmark` working: a multi-turn run
  still exposes one terminal `final_score` / `pass` on `test_run` (the last-turn score in §3.3), so
  benchmark aggregation needs no change.
- `Reevaluate` re-judges using the stored `session_id` and per-turn watermarks with a chosen Judge
  alias; it re-runs the `judge` nodes only (execution is not repeated).
- Supersedes 09 §10: multi-turn is no longer "future", it is this spec. 09's single-turn prose stays
  valid as the one-turn case.
- Out of scope for v1: node types beyond `turn`/`judge` (the generic DAG anticipates them, but only
  these two executors ship), turn-level retry isolation, and multi-turn benchmark sub-scores (e.g.
  "pass rate at turn k"). Manual pipeline authoring is deliberately **not** planned — the pipeline
  is always derived from `turns`; the multi-node data structure keeps the engine general, it does
  not expose a pipeline editor.
