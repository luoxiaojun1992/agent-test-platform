# Target Agent, Tool-Chain, and Evaluation Design

## 1. Single-turn Definition

One Test Run performs exactly one Bearer-authenticated SSE request to the target Agent. The Agent
may execute dependent Tool calls inside that request, up to Test Case and platform limits. This
is single-turn from the user/API perspective and multi-step inside the Agent runtime.

Multi-turn conversation is a Phase 4 direction, not MVP scope: each turn would still be its own
single-turn SSE request under this same contract, individually captured and judged, plus one
additional final Judge Evaluation Attempt over the whole conversation (see `architecture.md`
Section 24).

## 2. Agent Endpoint

Default adapter calls:

```http
POST {agent.chat_endpoint}/v1/chat
Authorization: Bearer <target platform credential>
Idempotency-Key: <run id>
X-ATP-Contract-Version: 1.0
Content-Type: application/json
Accept: text/event-stream
```

Request and events must conform to `target-chat-request.schema.json` and
`target-chat-event.schema.json`. Full framing and ordering rules are defined in
`07-target-sse-contract.md`. A custom Adapter may translate another API, but its normalized event
stream must satisfy the same contract.

The Runner sends one chat request per Run. The platform has no Tool Gateway: the Agent under test decides which Tools exist and whether to call them, and the platform only judges what it reports. The request carries the Tool list the Agent itself published (`GET /v1/tools`, section 2.1) or `null`:

```json
{
  "schema_version": "1.0",
  "run_id": "01K4Z6Y7X8W9V0T1S2R3Q4P5N6",
  "model": "target-agent-v1",
  "message": {
    "id": "msg-01K4Z6Y7X8W9V0T1S2R3Q4P5N6",
    "role": "user",
    "content": "Please cancel order O10001."
  },
  "deadline": "2026-09-11T10:01:00Z",
  "tools": null,
  "knowledge_document_ids": []
}
```

The Agent Bearer token is resolved from the `secret` table, inserted only in the outbound header, and never persisted or logged. The request carries no database
credential and no platform token; the prompt names the Run database ("Use the database named `run_<ULID>` for all data access.") and the Agent uses its own Tools and its own
database access. Observable Tool events are stored as Tool Call Records and are always `AGENT_REPORTED`. How the platform obtains and protects the credentials it does use is defined in
`06-sandbox-and-security.md`.

### 2.1 Tool discovery

The Agent publishes its Tool list through a paginated `GET {agent.chat_endpoint}/v1/tools` (`target-tool-list-response.schema.json`). The platform reads it with
`POST /api/v1/agent-tool-discovery` when a tester creates an Agent Version in the UI ("从 Agent 读取 Tool 列表") and stores the result as the Version's Tool Catalog.
Which Tools exist, and whether the Agent calls any of them, is entirely the Agent's decision.

Canonical Agent capabilities map to Tool Registry names as follows:

| Agent Capability | Tool Registry Name |
|---|---|
| `MYSQL_QUERY` | `mysql.query` |
| `MYSQL_EXECUTE` | `mysql.execute` |

When a Tool Catalog is configured, the request Tool list is the intersection of Agent Version
Tools, Test Case expected Tools, and platform policy. An explicitly empty array means no Tools are
allowed. A missing Tool Catalog produces `tools=null` and selects `SESSION_LLM_ONLY`; it does not
invalidate the Run.

The Runner does not automatically retry an Agent HTTP request after it may have started because
the call can have side effects. Transport failure produces `AGENT_ERROR`; a retry is a new Run
with its own Run database. Repeating the same `run_id` is
allowed only when the Agent contract explicitly guarantees idempotency.

## 3. Actual Tool Call Record

Every call returned by the Agent has:

- monotonically increasing `sequence`, starting at 1;
- unique `call_id` within the Run;
- registry-qualified `tool_name`;
- JSON `arguments`;
- one `result` or `error`;
- duration in milliseconds;
- optional parent call ID.

The Agent cannot set `trace_source`. The Runner marks every returned call `AGENT_REPORTED`; there is no second source to reconcile it with.

Maximum defaults:

| Item | Limit |
|---|---:|
| Calls per Run | 32 |
| Arguments per call | 16 KB |
| Result retained per call | 32 KB |
| Total retained tool evidence | 256 KB |

Oversized results are safely truncated and store `original_bytes`, `sha256`, and
`truncated=true`. Arguments are never silently truncated because they are needed for matching;
an oversized argument makes the Agent response invalid.

The platform does not sit between the Agent and its database, so it cannot limit what a Tool returns to the Agent. It limits only what it **keeps**: an oversized result is
safely truncated for persistence and retains its original size and digest.

## 4. Trace Trust

Every Tool Call Record is `AGENT_REPORTED`: it is what the Agent chose to report. The platform neither proxies nor re-executes Tool calls, so it cannot prove that a reported call
happened or that an unreported one did not. Evidence is therefore weighed accordingly:

- State is verified independently: the platform reads the Run database itself before and after the Agent runs (state diff, postconditions, mutation scope). A claimed write that did not change state, or a change nobody reported, shows up as a deterministic failure.
- Tool-chain scoring (`DETERMINISTIC_CHAIN`) compares the reported calls with the expected chain, and the Judge sees them labelled as reported.
- Benchmark reports and the Judge input must present Tool evidence as Agent-reported, never as verified.
- An Agent whose Tool reports cannot be trusted enough for Tool-level scoring should use `SESSION_LLM_ONLY`.

## 5. Tool Evaluation Modes

`DETERMINISTIC_CHAIN` requires a configured Tool Catalog (read from the Agent's `GET /v1/tools`) and a non-null Expected Tool Call
Chain. The deterministic matcher below applies.

`SESSION_LLM_ONLY` requires `expected_tool_call_chain=null`. Observable SSE Tool events are stored,
redacted, bounded, and included in the Judge session transcript, but no deterministic Tool name,
argument, or sequence score is computed. State, mutation, pre/postcondition, safety, and hard Tool
failures remain deterministic.

For Task cases in `SESSION_LLM_ONLY`, the absent deterministic Tool component is removed and the
40:30 Postcondition/Mutation weights are normalized to `4/7` and `3/7`. The LLM Judge still scores
Tool selection, arguments, sequence, result use, and response quality from the bounded transcript.
No neutral or automatic passing Tool score is inserted.

## 6. Expected Tool Call Chain

MVP supports one linear `ORDERED_SUBSEQUENCE` chain. Required expected steps match the earliest
unmatched actual call after the previous matched step. Extra reads are allowed only when
`allow_extra_read_calls=true`; extra writes always fail unless represented by an expected step
and allowed mutation rule.

Each expected step contains:

- stable step ID;
- exact Tool name;
- `required` and `critical` flags;
- semantic intent for the Judge;
- deterministic argument matcher;
- optional result matcher.

SQL matcher fields:

- operation;
- required and forbidden tables;
- required selected columns;
- required predicates;
- required assignments;
- affected-row range.

Identifiers are compared case-insensitively after AST qualification. Predicates support `=`,
`!=`, `<`, `<=`, `>`, `>=`, `IN`, `IS NULL`, and `IS NOT NULL` in MVP. Unsupported expressions
cannot satisfy a critical deterministic matcher.

`arguments_schema` is standard JSON Schema Draft 2020-12. Validation is against the complete
arguments object: required fields must exist; extra fields are accepted only when that embedded
schema permits them. Every item in `required_predicates` and `required_assignments` must be found
in the normalized AST; additional read predicates are allowed, but additional assignments are
not. `AND` terms are flattened and order-independent. `OR`, subqueries, functions, and computed
expressions cannot satisfy a Critical Matcher in MVP and must instead be represented by a simpler
accepted chain.

## 7. Matching Algorithm

```text
normalize all actual calls
reject duplicate sequence or call IDs
scan all calls for forbidden matches and unapproved writes
for each required expected step in order:
    find earliest unmatched actual call after previous match
    require exact tool name
    apply JSON argument schema
    apply SQL AST constraints when present
    apply result constraints when present
    record match or missing-step violation
check max call count
emit per-step checks, violations, score, and hard-failure flag
```

Tool-chain deterministic score:

$$
ToolScore = 100 \times \frac{\sum matched\_step\_weight}{\sum required\_step\_weight}
$$

When no Required Step supplies a weight, all Required Steps have equal weight. When any Required
Step supplies a weight, every Required Step must supply one and the sum must equal `1.0` within
`1e-6`.

Any critical-step miss, forbidden call, unapproved mutation, trace mismatch on a write, or
required write error sets `critical_failure=true` independently of this score.

## 8. Deterministic Evaluation

Every check has `id`, `category`, `weight`, `critical`, `pass`, `expected`, and `actual`.

Default deterministic weights for Task cases:

| Component | Weight |
|---|---:|
| Preconditions and fixture validity | gate only |
| Postconditions | 40% |
| Allowed state-change diff | 30% |
| Expected tool chain | 30% |

Fixture failure invalidates the Run and is not scored. Component weights are redistributed only
for Answer cases where state checks are intentionally absent. All configured weights must sum
to one within `1e-6`.

## 9. LLM Judge Criteria

Default Task criteria:

| Criterion | Weight |
|---|---:|
| Outcome correctness | 25% |
| Tool selection | 15% |
| Tool arguments | 20% |
| Tool sequence | 10% |
| Use of tool results | 15% |
| Final response completeness | 15% |

The Judge receives expected chain, normalized actual calls, deterministic match report, state
checks, prompt, expected output, and final output. Tool output is allowlisted, redacted, and
bounded. It receives no Agent name, version, hidden reasoning, or credential.

The Judge returns one 0-100 integer per requested criterion and a short reason. Missing,
duplicate, unknown, or out-of-range criteria invalidate the response. The platform computes:

$$
LLMScore = \sum_i weight_i \times criterionScore_i
$$

The platform ignores any model-provided aggregate score or verdict.

## 10. Final Verdict

```text
critical_pass = every critical state, mutation, safety, and tool check passed
judge_pass    = LLMScore >= judge_threshold
final_score   = 0.4 * DeterministicScore + 0.6 * LLMScore
pass          = critical_pass AND judge_pass AND final_score >= final_threshold
```

Defaults: `judge_threshold=70`, `final_threshold=70`.

One failed Critical Check makes `critical_pass=false` and therefore makes the final verdict
`FAIL`, regardless of `LLMScore` or `final_score`. The numeric score is still retained for
diagnostics.

Execution outcomes map as follows:

| Outcome | Deterministic | Judge | Final |
|---|---|---|---|
| `COMPLETED` | normal | required | scoring rules |
| `AGENT_ERROR` | hard fail | required to classify available output/evidence | fail |
| `TIMEOUT` | hard fail | required to classify available output/evidence | fail |
| `CANCELLED` after Agent start | hard fail | required to classify available output/evidence | fail |
| pre-start `INFRA_ERROR` | not scored | not required | excluded |
| `EVALUATION_FAILED` | retained | failed attempt | no verdict |

## 11. Judge Reliability

- Non-streaming JSON Schema response only.
- 90-second timeout.
- Up to two transient retries with Model Gateway backoff.
- Circuit breaker is scoped to concrete provider/model.
- A retry is the same Evaluation Attempt; cross-provider fallback creates a new Attempt.
- Model-reported confidence is stored but never controls pass/fail.

Before a model alias can be used as Judge, it must pass a versioned calibration set with at
least 50 labeled examples, at least 20 hard failures, at least 90% verdict agreement, and zero
false passes on hard failures. OpenAI and Ollama Judge models are calibrated independently.

## 12. Security

Prompt, Agent output, Tool arguments, and Tool results are all untrusted. The Judge prompt uses
explicit data delimiters, contains no Tools, and states that embedded instructions are data.
Secret-like values are redacted before persistence and evaluation. Deterministic safety gates
always take precedence over semantic scores.
