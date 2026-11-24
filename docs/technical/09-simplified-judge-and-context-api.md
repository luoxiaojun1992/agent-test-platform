# Simplified Judge and Session Context API

## 1. Motivation

The platform currently couples execution and evaluation through four intersecting modes:
`agent_mode` (`ANSWER` | `TASK`) crossed with `tool_evaluation_mode`
(`DETERMINISTIC_CHAIN` | `SESSION_LLM_ONLY`). Each mode carries a different set of
deterministic checks (precondition, postcondition, state diff, expected tool chain) plus an LLM
Judge. For the actual requirement — **judge whether the Agent under test's LLM output and its
Tool calls match expectation** — this mode matrix is over-engineered.

This spec collapses the platform to a single evaluation model and moves the *source of truth* for
judging from the platform's SSE-collected copy to the Target Agent's authoritative session
context, fetched after the session ends through a paginated API.

Scope: test platform only. The Target Agent implements the context API through its adapter (out of
scope here); the platform defines the contract and the judge consumer.

## 2. Target Architecture: run / judge separation

Execution and evaluation become two independent stages:

| Stage | Data source | Purpose | Timing |
|---|---|---|---|
| **run** | `POST /v1/chat` SSE stream | execute the test case, show progress (output + Tool calls) live | real-time |
| **judge** | `GET /v1/sessions/{id}/context` (paginated) | pull the authoritative LLM context + Tool calls, judge against expectation | after session ends, async |

The judge no longer reads the platform's `tool_call_record` snapshot. It pulls the complete
context from the Target, which is more complete than the SSE-reported events (it includes the full
LLM message history, not only `final_output` and reported Tool calls).

```text
Run stage (real-time)
  test platform ──POST /v1/chat (SSE)──▶ Target Agent
                      ◀── response.created → deltas/tool_call/tool_result → response.completed(session_id)
  → persist session_id + final_output (for display); no state checks

Judge stage (async, after session ends)
  test platform ──GET /v1/sessions/{session_id}/context?cursor=…──▶ Target Agent
                      ◀── paginated context items (message / tool_call / tool_result)
  → build judge request → LLM Judge → verdict
```

## 3. Session Context API (new contract)

The Target exposes a paginated, cursor-based endpoint over one session's complete LLM context.
This is the judge's data source.

```http
GET {target_base_url}/v1/sessions/{sessionId}/context?cursor=<opaque>&limit=<n>
Authorization: Bearer <target token>
X-ATP-Contract-Version: 1.0
Accept: application/json
```

Query parameters:

- `cursor`: opaque pagination cursor, omitted for the first page.
- `limit`: page size, default 100, maximum 500.

Response `200`:

```json
{
  "schema_version": "1.0",
  "session_id": "01K4Z6Y7X8W9V0T1S2R3Q4P5N6",
  "items": [
    { "sequence": 1, "type": "MESSAGE", "role": "user", "content": "Analyze order O10001.", "created_at": "2026-09-15T07:00:00Z" },
    { "sequence": 2, "type": "TOOL_CALL", "tool_name": "sql_executor", "arguments": { "query": "SELECT …" }, "created_at": "2026-09-15T07:00:01Z" },
    { "sequence": 3, "type": "TOOL_RESULT", "tool_name": "sql_executor", "status": "SUCCESS", "result": { "rows": [ … ] }, "created_at": "2026-09-15T07:00:02Z" },
    { "sequence": 4, "type": "MESSAGE", "role": "assistant", "content": "Order O10001 is PAID.", "created_at": "2026-09-15T07:00:03Z" }
  ],
  "next_cursor": null
}
```

Rules:

1. Items are a flat, time-ordered sequence; `sequence` strictly increases by 1 across pages.
2. Item types:
   - `MESSAGE` — one LLM message with `role` (`user` | `assistant` | `system` | `tool`) and `content`.
   - `TOOL_CALL` — one Tool invocation with `tool_name` and `arguments`.
   - `TOOL_RESULT` — one Tool outcome with `tool_name`, `status` (`SUCCESS` | `ERROR`) and `result` or `error`.
3. `next_cursor` is `null` on the last page. The platform walks pages until `null`, enforcing a hard
   ceiling (default 100 000 items) to bound the judge input.
4. `tool_name` is an arbitrary string (`^[a-z][a-z0-9_.-]{0,127}$`); `arguments` is an arbitrary
   JSON object. The platform does not interpret either — it only forwards them to the Judge.
5. The context is immutable once the session is terminal. A non-terminal session returns `409`
   `SESSION_NOT_TERMINAL`; a missing session returns `404` `SESSION_NOT_FOUND`.
6. Timeout: 30 seconds per page. Authentication uses the same Bearer (or client-credentials token)
   as chat; `X-ATP-Contract-Version` is required.

## 4. SSE Contract Adjustments

The SSE contract (07) stays in place for the run stage; it is the real-time display path, not the
judge data source. Two adjustments:

1. `response.completed` gains a required `session_id` field — the Target's own session identifier
   that the judge stage will query. The platform's `run_id` remains the transport idempotency key
   and is not the session id.
2. The `mode` field on `target-chat-request` is removed (see section 7); the request carries no
   evaluation-mode discriminator.

```json
{ "schema_version": "1.0", "event_id": "evt-0004", "run_id": "…", "sequence": 4,
  "type": "response.completed", "created_at": "…", "status": "COMPLETED",
  "session_id": "01K4Z6Y7X8W9V0T1S2R3Q4P5N6", "final_output": "…", "usage": { … } }
```

## 5. Run Stage

The run stage executes the test case and shows progress; it performs no scoring and no state
verification.

1. `POST /v1/chat` with one user message (single-turn). SSE events stream back for live display.
2. On `response.completed`, the platform persists `session_id` and `final_output` on `test_run`.
   `final_output` is display-only; the judge uses the context API.
3. No `precondition`, `postcondition`, `state diff`, or `expected tool chain` is computed or stored.
4. Run terminal states: `COMPLETED` (agent finished and reported a `session_id`), `AGENT_ERROR`,
   `TIMEOUT`, `CANCELLED`. `INFRA_ERROR` remains for pre-execution failures (e.g. the agent is
   unreachable). Knowledge upload still happens here (08 section 3) when the case carries
   `knowledge_documents`.

## 6. Judge Stage

The judge runs asynchronously after the session ends, reusing the existing evaluation queue.

1. The evaluation worker reads `session_id` from `test_run`.
2. It pages through `GET /v1/sessions/{session_id}/context` until `next_cursor` is `null`, collecting
   the full context within the item ceiling.
3. It builds the judge request from:
   - the context items (messages + Tool calls + Tool results), forwarded verbatim;
   - `expected_output` and `evaluation_criteria` from the Test Case;
   - the run's `execution_outcome`.
4. The LLM Judge scores the `evaluation_criteria` and returns a verdict. No deterministic gate or
   tool-chain match is computed; the judge is the sole scorer.
5. `final_score = LLMScore`; `pass = LLMScore >= judge_threshold`. The threshold defaults to 70.
6. Timeout, retry, calibration, and circuit-breaker rules from 04 section 11 are unchanged.

## 7. Removal of Evaluation Modes

The following concepts and their storage are deleted. This is a breaking, cross-cutting change.

| Removed | Where it lives today |
|---|---|
| `agent_mode` (`ANSWER`/`TASK`) | `generation.Candidate.AgentMode`, `generation/request.go`, `runexec/executor.go` (`buildRequest`, `buildChecks`), `contracts/schemas/create-test-case.schema.json`, `generation-request.schema.json`, `target-chat-request.schema.json` (`mode`) |
| `tool_evaluation_mode` (`DETERMINISTIC_CHAIN`/`SESSION_LLM_ONLY`) | `agentregistry` (`ModeDeterministicChain`/`ModeSessionLLMOnly`, `AgentInput`/`Agent`), `agent.tool_evaluation_mode` column, `generation.Candidate.ToolEvaluationMode`, `runexec/executor.go` (`buildChecks`), `contracts/schemas/agent-request.schema.json` |
| `expected_tool_call_chain` + `toolmatch` package | `generation.Candidate.ExpectedToolCallChain`, `generation/testcase.go` (`compileToolChain`), `internal/toolmatch/*`, `contracts/schemas/create-test-case.schema.json` |
| `preconditions` / `postconditions` (SQL assertions) | `generation.Candidate`, `evaluation/assertion.go`, `runexec/executor.go` (`runAssertions`), `test_run.precondition_json`/`postcondition_json` |
| `allowed_state_changes` / state diff | `generation.Candidate`, `sandbox/statediff.go`, `runexec/executor.go` (`CaptureState`/`DiffStates`/`EvaluateMutations`), `test_run.state_diff_json` |
| `deterministic_json` / `deterministic_score` | `test_run.deterministic_json`/`deterministic_score`, `runexec/executor.go` (`buildChecks`), `evaluation/request.go` (`deterministic_results`, `tool_chain_match`) |

Retained (still needed):

- `evaluation_criteria` — the judge's scoring dimensions (weighted).
- `expected_output` — the expectation the judge compares against.
- `knowledge_documents` — knowledge fixtures (uploaded in the run stage, 08).
- `final_output` — display-only on `test_run`.
- **Tool catalog on the Agent** — retained, but its purpose changes (see section 8). It is no
  longer a deterministic matcher input; it informs Test Case generation.

## 8. Tool Catalog: generation-time reference

The Agent keeps a Tool Catalog: the list of Tools the Agent under test supports, each with
`name`, `description`, and `input_schema`. Its purpose shifts from deterministic chain matching to
**informing Test Case generation**.

During Test Case generation, the platform passes the Agent's Tool Catalog to the generation LLM
alongside the scenario. The LLM uses it to:

1. write a `prompt` that naturally exercises the relevant Tool(s) for the scenario;
2. write `evaluation_criteria` that judge whether the Agent called the right Tool with sensible
   arguments and produced the right answer — the criteria are semantic (e.g. "the Agent queried the
   order table and reported the correct status"), not deterministic AST matchers.

The catalog is advisory: the Agent may call any Tool it wants at run time; the judge compares the
reported Tool calls against the semantic criteria. `ToolDefinition{name, description, input_schema}`
is unchanged from today and maps 1:1 to a data-agent ADK tool spec (see section 9).

## 9. data-agent adapter feasibility

The contract is shaped so a future data-agent adapter can implement it without contortions. The
mapping is:

| Contract element | data-agent counterpart |
|---|---|
| `chat_endpoint` + `POST /v1/chat` | data-agent `POST /api/v1/chat` (adapter rewrites the path) |
| `session_id` | `chat.Session.ID` (a string) |
| Tool `name` / `description` / `input_schema` | ADK `toolSpec` `name` / `description` / Go struct `jsonschema` tags (e.g. `sql_executor` with `query`/`params`) |
| context `MESSAGE` item | one session message (`role` = user/assistant/system/tool) |
| context `TOOL_CALL` / `TOOL_RESULT` item | one ADK function-tool invocation and its result, read from the session event/record store |
| client-credentials token | data-agent token endpoint (added on the data-agent side, out of scope here) |

The adapter reads the session's persisted message + tool-call history from data-agent's store
(MongoDB) and pages it out through the context endpoint; the platform-side consumer is agnostic to
where that history lives. Authentication between the platform and the adapter uses client
credentials (08 section 4).

## 10. Single-turn to Multi-turn

Single-turn is the current scope; the design must not preclude multi-turn.

- **Single-turn**: one `POST /v1/chat` per run; `response.completed` returns a new `session_id`.
- **Multi-turn (future)**: `target-chat-request` gains an optional `session_id` to continue an
  existing session; successive turns carry the same `session_id`. The context API is already
  session-scoped, so judging a multi-turn session needs no contract change — the flat item sequence
  simply spans multiple turns.

The `session_id` on `test_run` is therefore the single association point between a run and the
judge data source, which is exactly what multi-turn needs.

## 11. Migration and Compatibility

Breaking changes to persisted JSON and columns require a version bump and a migration or
compatibility note (README change rule).

- `contract_version` bump: `1.0` → `2.0` on the target contract and the platform API where the
  removed fields were required.
- `target-chat-request.schema.json`: remove `mode`; add optional `session_id`.
- `target-chat-event.schema.json`: `response.completed` adds `session_id`.
- New schema `target-session-context-response.schema.json` for the context API.
- `migrations/0021_simplify_evaluation.sql`: drop `agent.tool_evaluation_mode`; drop
  `test_run.precondition_json`, `postcondition_json`, `state_diff_json`, `deterministic_json`,
  `deterministic_score`; add `test_run.session_id`. `agent.tool_catalog_json` is **kept**.
- The `toolmatch` package and `evaluation/assertion.go` SQL assertion executor are deleted.
- `04-agent-tool-evaluation.md` and `07-target-sse-contract.md` are superseded in part by this
  document; their mode and deterministic-scoring prose is obsolete.
