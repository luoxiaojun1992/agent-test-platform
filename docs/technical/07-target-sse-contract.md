# Target Platform SSE Contract

## 1. Scope

A Test Run sends exactly one user message to a configured Target Agent. The target
platform processes that message, including any internal Tool calls, and streams all observable
session events over one HTTP response. The MVP does not require a separate Chat History API.

Future multi-turn support (Phase 4, not MVP) is expected to open one such SSE request per turn
rather than keep one connection open across turns; see `architecture.md` Section 24 for the
per-turn plus final Judge evaluation direction.

The stream is execution evidence, not hidden reasoning. Targets must never emit Chain-of-Thought,
private scratchpads, credentials, or internal logs.

An Agent pins one `target_model` returned by the authenticated Model List API. The Runner
places that exact model ID in every Chat request; changing the model creates a new Agent.

## 2. Authentication and Endpoint

```http
POST {chat_endpoint}/v1/chat
Authorization: Bearer <target platform token>
Idempotency-Key: <run ULID>
X-ATP-Contract-Version: 1.0
Accept: text/event-stream
Content-Type: application/json; charset=utf-8
Cache-Control: no-cache
```

`target_bearer_token_ref` belongs to the Target Agent. It is distinct from the platform
API Bearer Token, OpenAI API key, optional Ollama token, and run-scoped database/Tool credentials.
The resolved target token is used only in the outbound `Authorization` header and is never stored
in Run evidence or logs.

A successful target responds with HTTP `200` and `Content-Type: text/event-stream`. Authentication,
contract, and pre-execution errors use a non-2xx JSON error response. Once HTTP `200` has begun,
errors are terminal `response.error` events.

### 2.1 Model List

```http
GET {target_base_url}/v1/models
Authorization: Bearer <target platform token>
X-ATP-Contract-Version: 1.0
Accept: application/json
```

The response validates against `target-model-list-response.schema.json`. The platform calls this
endpoint when creating or validating an Agent, then pins one returned model ID as
`target_model`. A Test Run must not silently switch to another model if the pinned model disappears.
The request timeout is 10 seconds and may be shortened by the caller deadline.

### 2.2 Text Knowledge Indexing

Knowledge is optional. The compatibility API accepts text in JSON only; multipart files, binary
documents, URLs, and archives are outside the MVP contract.

Each Test Case contains zero or more immutable `knowledge_documents` text fixtures with a name,
text, digest, and optional metadata. The Runner uploads those fixtures during Run preparation;
generated knowledge text never comes from browser state or a previous Run.

```http
POST {target_base_url}/v1/knowledge/documents
Authorization: Bearer <target platform token>
Idempotency-Key: <knowledge request ULID>
X-ATP-Contract-Version: 1.0
Content-Type: application/json; charset=utf-8
Accept: application/json
```

The request validates against `target-knowledge-upload-request.schema.json` and includes the text,
its SHA-256, an absolute `indexing_deadline`, and `expires_at`. The HTTP upload timeout is 30
seconds. Success returns `202`, a document ID, status URL, and optional `Retry-After` header. A
timeout before a response is safe to retry with the same idempotency key and request digest.

Index status is queried with the same Bearer token:

```http
GET {target_base_url}/v1/knowledge/documents/{documentId}
Authorization: Bearer <target platform token>
X-ATP-Contract-Version: 1.0
Accept: application/json
```

Statuses are `QUEUED`, `INDEXING`, `READY`, and `FAILED`. The platform waits at most 10 minutes and
never beyond the request `indexing_deadline`; polling uses `Retry-After` when it is 1-60 seconds,
otherwise the configured 2-second interval. `FAILED` yields `KNOWLEDGE_INDEX_FAILED`; exceeding
either deadline yields `KNOWLEDGE_INDEX_TIMEOUT`. Caller cancellation stops polling immediately.

The Runner sends a Chat request only after every referenced document is `READY` and its digest
matches the upload. Documents carry a required expiry so test content is eventually removed even
though delete is not part of the minimum compatibility API.

Before uploading, the pinned Target model must advertise `KNOWLEDGE` in the Model List response.
Missing capability fails Run preparation; the platform does not silently run the case without its
knowledge fixtures.

## 3. Request

The normative schema is `contracts/schemas/target-chat-request.schema.json`.

```json
{
  "schema_version": "1.0",
  "run_id": "01K4Z6Y7X8W9V0T1S2R3Q4P5N6",
  "model": "mock-agent-v1",
  "mode": "TASK",
  "message": {
    "id": "msg-01K4Z6Y7X8W9V0T1S2R3Q4P5N6",
    "role": "user",
    "content": "Please cancel order O10001."
  },
  "deadline": "2026-09-15T07:01:00Z",
  "tools": null,
  "knowledge_document_ids": []
}
```

The request carries no database connection details. The Agent already has its own database access. The platform seeds the Run database with its own administrator account and tells the Agent the Run database name inside the user prompt text (`message.content`), never through an API field. `tools` has two modes:

- array: the Target Agent has a Tool Catalog. Only listed Tools are allowed and the Test
  Case uses deterministic expected-chain evaluation;
- `null`: no Tool Catalog is configured. The platform does not apply deterministic Tool name,
  argument, or ordering rules. Observable Tool events may still be streamed and are supplied to
  the LLM Judge as bounded, redacted session evidence.

An empty Tool array explicitly means no Tools are allowed; it is different from `null`.
Every ID in `knowledge_document_ids` must be `READY` before Chat starts. An empty array means the
case does not use Target knowledge.

## 4. SSE Framing

Each event is a standard SSE block. `event` equals `data.type`, `id` equals `data.event_id`, and
`data` is exactly one complete JSON object:

```text
event: response.output_text.delta
id: evt-0002
data: {"schema_version":"1.0","event_id":"evt-0002","run_id":"...","sequence":2,"type":"response.output_text.delta","created_at":"2026-09-15T07:00:01Z","delta":"Order"}

```

Targets may send `: ping` comment blocks at least every 15 seconds while idle. The platform ignores
comments. `retry:` fields are ignored because the Runner never automatically reconnects or replays
a Run after execution may have started.

Every event must validate against `contracts/schemas/target-chat-event.schema.json`. Limits:

- one event JSON payload: 256 KB;
- full SSE response retained for parsing: 2 MB;
- output text: 16,000 characters;
- Tool calls: 32;
- Tool arguments: 16 KB per call;
- retained Tool result: 32 KB per call and 256 KB total evidence.

## 5. Event State Machine

```text
response.created
  -> response.output_text.delta | response.tool_call | response.tool_result (zero or more)
  -> response.completed | response.error
```

Rules:

1. `response.created` is first and appears once.
2. `sequence` starts at 1 and increases by exactly one.
3. `event_id` and Tool `call_id` values are unique within the Run.
4. A `response.tool_result` follows a matching earlier `response.tool_call`; each call resolves once.
5. Tool events for multiple calls are ordered. Parallel Tool execution is not represented in MVP.
6. Exactly one terminal event appears last.
7. `response.completed.final_output` is authoritative. Concatenated deltas, when present, must equal
   it byte-for-byte.
8. A terminal event with unresolved Tool calls is invalid.
9. EOF before a terminal event is `AGENT_ERROR`.

Event types:

- `response.created`: acknowledges the Run.
- `response.output_text.delta`: appends observable assistant text.
- `response.tool_call`: reports Tool name and complete JSON arguments.
- `response.tool_result`: reports one success result or structured error with duration.
- `response.completed`: contains `COMPLETED`, authoritative final output, and optional usage.
- `response.error`: contains `AGENT_ERROR` or `TIMEOUT` and a structured error.

## 6. Retry and Cancellation

The Runner enforces the request deadline and cancels the HTTP context when the Run is cancelled.
The target should stop work when the connection context is cancelled. A network failure after the
stream starts is not retried because Tool calls may have side effects. A user-requested retry is a
new Run with a new Run ID, its own Run database, and a new idempotency key.

The target should treat duplicate `Idempotency-Key` values idempotently only for transport races
before execution starts. The platform does not depend on target replay support.

Default timeout matrix:

| Operation | Timeout |
|---|---:|
| Model List | 10 seconds |
| Knowledge upload HTTP request | 30 seconds |
| Knowledge status HTTP request | 10 seconds |
| Knowledge asynchronous indexing wait | 10 minutes |
| Target Chat SSE | Test Run deadline |

All timeouts are hard context deadlines. They include connection, response headers, and body
processing; a shorter parent context always wins.

## 7. Evaluation Modes

`DETERMINISTIC_CHAIN` requires a configured Tool Catalog and a non-null Expected Tool Call Chain.
Tool events are normalized and matched, then also included in Judge evidence.

`SESSION_LLM_ONLY` requires no Expected Tool Call Chain. The platform preserves a bounded transcript
of output deltas and any reported Tool events, skips deterministic Tool-chain scoring, and asks the
Judge to evaluate observable behavior. State, mutation, pre/postcondition, safety, and infrastructure
hard failures remain deterministic and cannot be overridden by the Judge.

## 8. Compatibility Tests

A compatible target implementation must pass:

- valid text-only completion;
- model selection and unavailable model rejection;
- text knowledge upload, indexing transition, READY gating, failure, and timeout;
- sequential Tool call and Tool result;
- `tools=null` session;
- Bearer rejection;
- malformed JSON event;
- sequence gap or duplicate event ID;
- result without a call;
- unresolved call at completion;
- delta/final-output mismatch;
- terminal error;
- EOF without terminal event;
- deadline cancellation.
