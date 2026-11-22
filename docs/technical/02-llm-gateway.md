# LLM Gateway Design

## 1. Decision

Implement a provider-neutral gateway inside the Go backend with two adapters:

- `OpenAIAdapter`
- `OllamaAdapter`

Both use the non-streaming OpenAI-compatible `POST /v1/chat/completions` wire subset. Domain
services depend only on the internal types below. LiteLLM may later be configured as another
OpenAI-compatible endpoint, but is not an MVP runtime dependency.

This choice keeps one deployment stack while retaining explicit handling for provider and
model capability differences.

## 2. Internal Interface

```go
type ModelGateway interface {
    Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
    Capabilities(ctx context.Context, model ModelRef) (ModelCapabilities, error)
    CountTokens(ctx context.Context, model ModelRef, input TokenInput) (int, error)
}

type ModelRef struct {
    Provider string
    Model    string
}

type CompletionRequest struct {
    RequestID       string
    Model           ModelRef
    Messages        []Message
    Tools           []ToolDefinition
    ToolChoice      ToolChoice
    ResponseSchema  json.RawMessage
    MaxOutputTokens int
    Temperature     float64
    Seed            *int64
}

type CompletionResponse struct {
    ProviderRequestID string
    Model             ModelRef
    StopReason        string
    Content           string
    ToolCalls         []ModelToolCall
    Usage             TokenUsage
    RawDigest         string
}
```

`CompletionRequest` and `CompletionResponse` are internal versioned contracts. Provider raw
responses may be retained for 24 hours for diagnostics but cannot be consumed by domain code.

## 3. Common Wire Subset

| Capability | Common Contract | Policy |
|---|---|---|
| Endpoint | `/v1/chat/completions` | Non-streaming only |
| Messages | system, user, assistant, tool | Text content only in MVP |
| Tools | function name, description, JSON Schema parameters | Required for generators |
| Tool choice | `auto`, `required`, or named function | Probe per model |
| Tool results | tool message linked by call ID | Max loop count enforced |
| Structured output | JSON Schema where supported | Otherwise force a schema Tool |
| Usage | input/output/total tokens | Nullable when Provider omits it |
| Parallel calls | normalized to ordered list | Disabled by default |

OpenAI-specific Responses API fields and Ollama-native `/api/chat` fields do not enter the
internal contract.

## 4. Provider Configuration

```yaml
models:
  generation-primary:
    provider: openai
    model: ${OPENAI_GENERATION_MODEL}
    base_url: https://api.openai.com/v1
    credential_ref: openai-generation-token   # Secret name entered in the UI (architecture 16.6)
    required_capabilities: [tools, named_tool_choice]

  judge-primary:
    provider: openai
    model: ${OPENAI_JUDGE_MODEL}
    base_url: https://api.openai.com/v1
    credential_ref: openai-judge-token        # Secret name entered in the UI (architecture 16.6)
    required_capabilities: [json_schema]

  local-generation:
    provider: ollama
    model: ${OLLAMA_GENERATION_MODEL}
    base_url: http://ollama:11434/v1
    api_key_ref: none
    required_capabilities: [tools]
```

Credentials are resolved by the adapter and never persisted in a request, manifest, or log.
Model aliases resolve to concrete provider/model/config digests before a Job or Evaluation
Attempt starts.

## 5. Capability Profile and Probe

Capabilities are model-specific, not merely provider-specific:

```go
type ModelCapabilities struct {
    ToolCalling       bool
    NamedToolChoice   bool
    ParallelToolCalls bool
    JSONSchemaOutput  bool
    Usage             bool
    ContextWindow     int
    MaxOutputTokens   int
    TokenizerID       string
}
```

At startup and whenever configuration changes, run three non-production probes:

1. A short text completion.
2. A forced call to a one-field test Tool.
3. A response conforming to a small JSON Schema.

Store probe time, result, latency, actual model identifier, and failure reason. A Role cannot
select a model that failed one of its required probes. Ollama model replacement under the same
tag changes the configuration digest and requires a new probe.

A Model Config is enabled only after all required Capability Probes pass. A config assigned the
Judge Role additionally requires a `PASSED` row in `judge_calibration` for the active calibration
version. The API rejects an unprobed or uncalibrated model alias before creating a Job or
Evaluation Attempt.

## 6. Structured Output Policy

Order of preference:

1. Native `response_format.type=json_schema`.
2. A single named Tool whose argument schema is the desired output schema.
3. Reject the model for that Role.

Plain JSON mode plus best-effort parsing is not sufficient for persisted assets or Judge
results.

## 7. Tool Loop

The Generation Orchestrator owns the loop:

1. Send messages and available Tool schemas.
2. Receive zero or more tool calls.
3. Validate Tool name and argument bytes before JSON decoding.
4. Execute each Tool in returned order; parallel execution is disabled in MVP.
5. Append the assistant tool-call message and one tool-result message per call.
6. Recalculate the context budget.
7. Continue until final content, terminal Tool result, or a hard limit.

Limits are checked before every provider request and Tool execution. `finish_reason=length`,
missing required Tool call, duplicate call ID, or malformed arguments is a structured failure,
not a signal to parse partial text.

This is one logical Generation interaction initiated by one QA request, not necessarily one
physical Provider HTTP request. OpenAI-compatible Tool Calling requires another completion request
after each Tool Result. No further QA/user message is accepted; only the Orchestrator appends
assistant Tool Calls and platform Tool Results. All Provider requests remain inside the same
bounded Generation stage/session.

OpenAI configurations require an API key Secret Ref. Ollama may omit a token on an isolated local
endpoint, but remote Ollama or an authenticated proxy uses its own optional credential. Provider
credentials are never reused as the platform API or Target Platform Bearer token.

## 8. Context Budget

The Budget Manager accounts for canonical serialized bytes using the configured tokenizer:

```text
input = system + messages + tool definitions + artifact excerpts + repair errors
required = input + reserved output + safety margin
```

Defaults:

- Safety margin: max(5% of context window, 1,024 tokens).
- Reserved output: Role-specific and at least 1,024 tokens.
- Repair budget: part of the Job budget, not a fresh unlimited budget.
- Unknown tokenizer: use the versioned conservative estimator
  `ceil(UTF8Bytes / 2) + 16 * messageCount + 32 * toolCount`, and limit usable context to
  70% of the model-declared window. Reject configuration if even this metadata is unknown.

No component may truncate silently. Dataset excerpts are selected deterministically by tables
referenced by the Scenario and already accepted Test Cases.

### 8.1 Session Compaction

The Orchestrator owns an ephemeral Session for each Generation stage. Before every model call it
builds the candidate request and estimates tokens. If the request exceeds the usable input budget,
it compacts the oldest closed Tool interaction groups until the request fits.

A Tool interaction group is one assistant message containing Tool calls followed by every Tool
result for those calls. Groups with unresolved calls are never compacted. The following anchors
are immutable:

- all System messages;
- the original stage User message;
- the platform-derived state ledger containing accepted artifact IDs/digests, remaining work,
  validator codes, and repair round;
- the newest `KeepRecentGroups` closed interaction groups, kept verbatim. A recent closed group may be
  compacted only when it alone (the base request plus that group) would exceed the budget and a safe
  typed summary exists; otherwise closed groups compact oldest first. Unresolved groups remain immutable.

Each compacted group is replaced by a versioned structured record containing:

```text
source_digest
assistant_content_digest
tool_call_id
tool_name
arguments_digest
result_digest
result_status
original_result_bytes
bounded_semantic_summary
```

The Tool executor must produce `bounded_semantic_summary` from its typed result. A large result
without that summary is not eligible for compaction. The Session then fails explicitly with
`CONTEXT_BUDGET_EXCEEDED`; it must not take a byte prefix or suffix as a substitute. Full Tool
results remain in Tool Evidence storage and are not copied into the compaction record.

Compacted records are kept verbatim only up to `MaxRetainedRecords`, newest first. Older records fold,
oldest first, into one rollup record that is part of the state ledger anchor: `schema_version`,
`folded_groups`, a `chain_digest` covering every folded source digest in order, the first and last
Tool call IDs, and an optional `state` object produced by a caller-supplied, deterministic state fold
(for example, the row ranges a Dataset Critic has read). The rollup is injected with the envelope. A
rollup larger than `MaxRollupBytes`, or a failing state fold, fails with `CONTEXT_BUDGET_EXCEEDED`; it
is never truncated. The envelope is therefore bounded by `MaxRollupBytes` plus `MaxRetainedRecords`
records however many Tool groups a Session completes.

The compacted envelope is injected as an explicitly labeled, untrusted `user` data message. It
must never be promoted to a `system` message, because Tool output may contain prompt injection.

Compaction is deterministic for the same messages, summaries, policy version, and state ledger.
The active prompt contains only observable messages and structured Tool results. Hidden reasoning
and Chain-of-Thought are never requested, compacted, or persisted. A Compaction Report records
estimated tokens before and after, compacted group count, policy version, and source digests for
24-hour diagnostics. This report is not evaluation evidence and cannot override deterministic
validation.

Required tests cover a single oversized Tool result, multiple sequential Tool groups, an
unresolved Tool call, a missing semantic summary, deterministic replay, and a request that remains
over budget after all eligible groups are compacted. They also cover a recent group that fits alone
(never compacted), oldest-first compaction, rollup folding that stays bounded and deterministic, a
state fold that rejects oversized or failing state, and token usage carried through a Tool loop.

## 9. Timeouts and Retry

| Call | Timeout | Retry |
|---|---:|---|
| OpenAI completion | 60 seconds | Up to 2 transient retries |
| Ollama completion | 180 seconds | Up to 1 transient retry |
| Capability probe | 30 seconds | No automatic retry during request path |
| Whole Generation Job | 10 minutes | Stage policy applies |
| Judge Attempt | 90 seconds | Up to 2 transient retries |

Retryable conditions: connection reset, timeout before response body, HTTP 408, 429, and 5xx.
Non-retryable conditions: authentication, invalid request/schema, context overflow, content
policy rejection, and malformed successful response. Backoff defaults to 250 ms then 1 second
with full jitter and respects `Retry-After`.

A circuit opens for 30 seconds after five consecutive transient failures for one concrete
provider/model. There is no silent cross-provider fallback. An explicitly configured fallback
creates a new Generation Stage Attempt or Evaluation Attempt and records both failures.

## 10. LiteLLM Option

LiteLLM is appropriate when provider count, centralized quota, or cross-language clients grow.
It can expose an OpenAI-compatible proxy for both OpenAI and Ollama. If enabled:

- Treat it as transport/routing infrastructure, not the domain abstraction.
- Keep internal request/response types and capability probes.
- Do not enable its optional database-backed features unless separately approved.
- Record the upstream provider/model, not only the LiteLLM alias.

For the two-provider MVP, direct adapters are the selected implementation.

## 11. Contract Tests

Each adapter must pass the same suite:

- text completion;
- one required Tool call;
- two sequential Tool calls with results;
- malformed Tool arguments;
- schema-conformant Judge output;
- output-token exhaustion;
- timeout and cancellation;
- usage present and absent;
- model capability mismatch;
- no silent Provider fallback.
