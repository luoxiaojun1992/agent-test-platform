# MVP Implementation Readiness

> Decision date: 2026-09-11
> Decision: GO for Slice 0 and Slice 1

## Approved Baseline

- Go modular monolith with separate API and Worker binaries.
- React + TypeScript + Vite frontend.
- Dedicated Platform MySQL 8.4 LTS for metadata and durable queue.
- validation MySQL 8.4 instance or container pool, used for business reference schemas and
  per-dataset execution schemas and a dedicated database per Test Run (no separate Sandbox
  instance).
- Internal provider-neutral Model Gateway with OpenAI and Ollama adapters.
- Non-streaming OpenAI-compatible Chat Completions subset.
- LiteLLM is optional and not an MVP runtime dependency.
- One target-Agent API request per Run; dependent internal Tool calls are allowed.
- Test Cases contain Expected Tool Call Chains and Allowed State Changes.
- Agent API responses contain ordered, structured Tool Call Records.
- Tool evidence is either `AGENT_REPORTED` or reconciled as `GATEWAY_VERIFIED`.
- Every valid Run receives deterministic evaluation and a mandatory LLM Judge Attempt.
- Critical state, mutation, security, and Tool failures are hard gates.
- Chain-of-Thought and hidden reasoning are never requested or stored.

## Normative Artifacts

- Product architecture: `docs/architecture.md`
- Technical designs: `docs/technical/`
- Public and integration API: `contracts/openapi.yaml`
- Structured contracts: `contracts/schemas/`
- Golden positive fixtures: `contracts/examples/`
- Platform schema: `migrations/0001_initial.sql`

## Entry Gate Status

| Gate | Status |
|---|---|
| Product scope and non-goals fixed | PASS |
| Platform/validation MySQL separation designed | PASS |
| Durable queue, lease, and fencing designed | PASS |
| OpenAI/Ollama compatibility protocol designed | PASS |
| Model capability probes and Judge calibration designed | PASS |
| Generation atomicity and repair limits designed | PASS |
| SQL allowlist and canonical result semantics designed | PASS |
| Expected/Actual Tool Call contracts designed | PASS |
| Tool trust and reconciliation designed | PASS |
| Mutation scope and global invariants designed | PASS |
| Judge criteria, aggregation, hard gates, and retries designed | PASS |
| OpenAPI and JSON Schema editor diagnostics | PASS |
| Full-workspace editor diagnostics | PASS |
| MySQL 8.4 migration executed in a real server | NOT RUN: Docker/MySQL unavailable in current environment |

The missing runtime migration check is a Slice 0 task, not an architecture decision. Slice 0
cannot exit until the migration is applied to a clean MySQL 8.4 instance and all expected tables,
indexes, foreign keys, and queue claim behavior are verified.

## Implementation Order

1. Slice 0: contract harness, schema/example validation, migration test, SQL parser corpus.
2. Slice 1: deterministic per-dataset execution schema, per-Run databases loaded through the bound Target's administrator connection, state diff, constraints. Slice 1A (credentials, Target registry, and Dataset bindings) follows it.
3. Slice 2: Agent Adapter, Tool Gateway, Tool evidence and matcher.
4. Slice 3: Model Gateway, OpenAI/Ollama probes, Judge and calibration.
5. Slice 4: AI generation, `create_test_case`, repair, critic and atomic publish.
6. Slice 5: public API, frontend and version comparison.

Each slice starts only after the previous slice exit criteria in
`docs/technical/05-api-and-implementation-plan.md` pass.

## Configuration Decision

- Platform process configuration comes from environment variables: `HTTP_ADDR`, `PLATFORM_MYSQL_DSN` (including the
  database password), and the encryption keys (architecture 16.4).
- Test-environment configuration (Targets, Database Profiles, Agent Versions, LLM providers) and every credential are
  entered through the UI and stored in Platform MySQL.
- A Dataset Revision may bind to several Targets. A Run requires its Test Case, Scenario, Dataset, and Agent Version,
  and a Dataset Revision bound to the Run's Target (architecture 16.5).
- Still environment variables until their UI work lands (architecture 16.4 and 16.6): `API_BEARER_TOKEN`,
  `GENERATOR_CONFIG_SHA256`, and `GENERATION_MODEL_*`. `TARGET_MYSQL_ADMIN_DSN` and `TARGET_MYSQL_CA_FILE` were removed
  when the Worker began resolving Targets from the registry (Slice 1A).
- Agent database credentials are entered by testers and stored encrypted; the platform creates no validation MySQL
  accounts (technical/06 section 3.3).
- The AGENT_MANAGED cross-Run trade-off is recorded for review (technical/06 section 3.3).

## Open Decision

- Platform administrator credential (the "test platform administrator password"): where it is stored and how it
  authenticates the Web UI and API. This needs the owner's answer before its implementation starts. It does not
  block Slice 0 or Slice 1.

## Deferred Decisions

These are intentionally post-MVP and do not block implementation:

- Platform-native long-lived Knowledge Base and vector retrieval benchmarking. Temporary text
	fixtures uploaded to a Target compatibility API are part of the MVP execution surface.
- Multi-turn user conversations.
- Browser, email, and non-MySQL Tools.
- External queue infrastructure.
- Mandatory LiteLLM deployment.
- Kubernetes and service decomposition.

## Go Decision

Multi-Target support, Dataset bindings, and the keyring are decided (architecture 16.4 and 16.5). The keyring,
rotation, Target registry, Dataset bindings, and the Worker's Target resolution are implemented (Slice 1A). The platform administrator credential is the only open decision and does not block Slice 0 or Slice 1.
Implementation may begin. Provider/model aliases and dependency versions are configuration and
contract-test decisions made inside the relevant slice; they do not change the approved domain
contracts.
