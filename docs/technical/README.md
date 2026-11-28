# MVP Technical Design Index

Status: Ready for implementation

The architecture baseline is [../architecture.md](../architecture.md). The files in
this directory define implementation-level behavior. Contracts under `contracts/`
and migrations under `migrations/` are normative.

## Decisions

- Backend: Go modular monolith with API and worker processes.
- Platform storage and durable queue: dedicated MySQL 8.4 LTS.
- Test environments: validation MySQL 8.4 instance or container pool; each Dataset Revision has a validated execution schema and each Test Run injects and removes test data in the Target's fixed database (no separate Sandbox instance; technical/08). The platform creates no validation MySQL accounts; it uses only the
	registered Target, with several Targets allowed per Dataset Revision (architecture 16.5; technical/06 section 1.1). Test-environment credentials are entered through the UI and stored encrypted (technical/06
	section 8.1). Platform settings come from environment variables (technical/05 section 3).
- LLM providers: OpenAI and Ollama through an internal provider-neutral gateway.
- Common wire subset: non-streaming OpenAI-compatible `/v1/chat/completions`.
- Optional proxy: LiteLLM may be configured later but is not required by the MVP.
- Target interaction: Bearer-authenticated `POST /v1/chat` SSE requests; internal multi-step Tool
	calls are allowed and no separate History API is required. A Test Case may carry multiple turns
	that continue one session via `session_id` (technical/10).
- Tool evidence: the judge reads the Target's authoritative session context through a paginated
	context API; SSE is the real-time display path only (technical/09). A multi-turn judge reads the
	context from the session start up to its turn, running between turns (technical/10).
- Evaluation: a single LLM Judge over the session context and Tool calls; no evaluation modes,
	deterministic gates, or tool-chain matching (technical/09).
- Run pipeline: a composable node DAG (init, turn, judge, and finish nodes with static and
	runtime-conditional edges) derived from the Test Case's turns; init provisions the test
	environment, finish tears it down, a failing turn judge stops the pipeline, and the whole
	sequence may repeat within one Run (technical/10).

## Specifications

1. [Storage and workflows](01-storage-and-workflows.md)
2. [LLM gateway](02-llm-gateway.md)
3. [Generation and validation](03-generation-and-validation.md)
4. [Agent, tool-chain, and evaluation](04-agent-tool-evaluation.md)
5. [API and implementation plan](05-api-and-implementation-plan.md)
6. [Sandbox and security](06-sandbox-and-security.md)
7. [Target platform SSE contract](07-target-sse-contract.md)
8. [Run data lifecycle, target auth, and concurrency](08-run-data-lifecycle-and-target-auth.md)
9. [Simplified judge and session context API](09-simplified-judge-and-context-api.md)
10. [Multi-turn test cases and a composable run pipeline](10-multi-turn-test-cases-and-pipeline.md)

## Normative Contracts

- `contracts/README.md`: complete Schema-to-example index and validation boundary.
- `contracts/openapi.yaml`: public platform API.
- `contracts/schemas/create-test-case.schema.json`: generator Tool input.
- `contracts/schemas/test-case.schema.json`: immutable published Test Case content.
- `contracts/schemas/target-chat-request.schema.json`: outbound Target chat request.
- `contracts/schemas/target-chat-event.schema.json`: inbound SSE event envelope.
- `contracts/schemas/judge-request.schema.json`: bounded evidence sent to the Judge.
- `contracts/schemas/judge-result.schema.json`: LLM Judge structured result.
- `contracts/schemas/pipeline.schema.json`: the run pipeline DAG (nodes / edges / conditions / config / context / result; technical/10).
- `migrations/0001_initial.sql`: initial Platform MySQL schema.

## Change Rule

A contract change that affects persisted JSON, API payloads, scoring, or workflow states
requires a version bump and a migration or compatibility note. Architecture prose must
not silently override a versioned schema.
