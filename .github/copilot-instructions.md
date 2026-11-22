# Repository Instructions

The canonical product architecture is `docs/architecture.md`. Implementation-level
behavior is defined under `docs/technical/`, `contracts/`, and `migrations/`.

## Current Scope

- Preserve the Generation-first MVP described in the architecture.
- Test assets are AI-generated from one short user description.
- MVP execution is single-turn from the user API while allowing internal multi-step Tool calls.
- Platform storage and the durable queue use a dedicated MySQL instance; validation MySQL is a
  separate, platform-controlled (never production) instance that also hosts per-dataset
  execution schemas (no separate Sandbox MySQL instance).
- OpenAI and Ollama are supported through the internal Model Gateway contract.
- Test case creation is gated by deterministic validation.
- Test Cases contain Expected Tool Call Chains; Agent responses contain structured Tool Call Records.
- Every valid test run requires an LLM Judge evaluation.
- Tool evidence is stored, but Chain-of-Thought and hidden reasoning are never stored.
- Knowledge Base and multi-turn user chat are post-MVP features.

## Engineering Guidelines

- Keep architecture changes consistent across domain models, APIs, state machines,
  storage, execution, evaluation, and delivery phases.
- Follow the vertical slices in `docs/technical/05-api-and-implementation-plan.md`.
- Treat versioned JSON Schemas, OpenAPI, and migrations as normative contracts.
- Domain packages must not import OpenAI-, Ollama-, HTTP-, or SQL-driver-specific types.
- Do not introduce PostgreSQL or an external queue without an approved architecture change.
- Do not silently truncate prompts, Tool arguments, SQL, or expected values.
- Deterministic state, mutation, and Tool hard failures cannot be overridden by LLM scores.
- Keep generated artifacts and runtime environments isolated and reproducible.
- Prefer minimal, focused changes and update the README when the repository workflow
  changes.

## Setup Status

- [x] Workspace created under `source/repos/agent-test-platform`.
- [x] Documentation-first repository structure created.
- [x] Architecture document added under `docs/`.
- [x] Implementation-level technical designs added under `docs/technical/`.
- [x] OpenAPI and JSON Schema contracts added under `contracts/`.
- [x] Initial Platform MySQL migration added under `migrations/`.
- [x] README and repository metadata added.
- [x] No extensions required.
- [x] Architecture is ready for Slice 0 and Slice 1 implementation.
- [x] No VS Code task required at this stage.
- [x] No runnable application to launch at this stage.
