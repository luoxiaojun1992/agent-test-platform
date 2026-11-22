# MVP Technical Design Index

Status: Ready for implementation

The architecture baseline is [../architecture.md](../architecture.md). The files in
this directory define implementation-level behavior. Contracts under `contracts/`
and migrations under `migrations/` are normative.

## Decisions

- Backend: Go modular monolith with API and worker processes.
- Platform storage and durable queue: dedicated MySQL 8.4 LTS.
- Test environments: validation MySQL 8.4 instance or container pool; each Dataset Revision has a validated execution schema and each Test Run uses its own
	database (no separate Sandbox instance). The platform creates no validation MySQL accounts; it uses only the
	registered Target, with several Targets allowed per Dataset Revision (architecture 16.5; technical/06 section 1.1). Test-environment credentials are entered through the UI and stored encrypted (technical/06
	section 8.1). Platform settings come from environment variables (technical/05 section 3).
- LLM providers: OpenAI and Ollama through an internal provider-neutral gateway.
- Common wire subset: non-streaming OpenAI-compatible `/v1/chat/completions`.
- Optional proxy: LiteLLM may be configured later but is not required by the MVP.
- Target interaction: one Bearer-authenticated `POST /v1/chat` SSE request; internal multi-step
	Tool calls are allowed and no separate History API is required.
- Tool evidence: structured Agent-returned records, with optional gateway verification.
- Evaluation: deterministic hard gates plus a mandatory LLM Judge.

## Specifications

1. [Storage and workflows](01-storage-and-workflows.md)
2. [LLM gateway](02-llm-gateway.md)
3. [Generation and validation](03-generation-and-validation.md)
4. [Agent, tool-chain, and evaluation](04-agent-tool-evaluation.md)
5. [API and implementation plan](05-api-and-implementation-plan.md)
6. [Sandbox and security](06-sandbox-and-security.md)
7. [Target platform SSE contract](07-target-sse-contract.md)

## Normative Contracts

- `contracts/README.md`: complete Schema-to-example index and validation boundary.
- `contracts/openapi.yaml`: public platform API.
- `contracts/schemas/create-test-case.schema.json`: generator Tool input.
- `contracts/schemas/test-case.schema.json`: immutable published Test Case content.
- `contracts/schemas/target-chat-request.schema.json`: outbound Target chat request.
- `contracts/schemas/target-chat-event.schema.json`: inbound SSE event envelope.
- `contracts/schemas/judge-request.schema.json`: bounded evidence sent to the Judge.
- `contracts/schemas/judge-result.schema.json`: LLM Judge structured result.
- `migrations/0001_initial.sql`: initial Platform MySQL schema.

## Change Rule

A contract change that affects persisted JSON, API payloads, scoring, or workflow states
requires a version bump and a migration or compatibility note. Architecture prose must
not silently override a versioned schema.
