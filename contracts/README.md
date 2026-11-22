# Contracts

These files are normative for the MVP.

## Public and Integration API

- `openapi.yaml`: Platform HTTP API.
- `target-openapi.yaml`: Target Platform compatibility API for models, SSE chat, and knowledge indexing.
- `schemas/secret-write-request.schema.json`: body of `PUT /api/v1/secrets/{name}`. Values are write-only and never echoed in responses or errors.
- `schemas/validation-mysql-request.schema.json`: body of `POST /api/v1/validation-mysql`. It names the admin password secret; it never carries a password.

## Generation Contracts

| Schema | Positive Example |
|---|---|
| `generation-request.schema.json` | `examples/generation-request.json` |
| `scenario.schema.json` | `examples/scenario.json` |
| `dataset-metadata.schema.json` | `examples/dataset-metadata.json` |
| `dataset-constraints.schema.json` | `examples/dataset-constraints.json` |
| `create-test-case.schema.json` | `examples/create-test-case.json` |
| `test-case.schema.json` | `examples/test-case.json` |
| `critic-result.schema.json` | `examples/critic-result.json` |

## Execution and Evaluation Contracts

| Schema | Positive Example |
|---|---|
| `canonical-sql-result.schema.json` | `examples/canonical-sql-result.json` |
| `target-chat-request.schema.json` | `examples/target-chat-request.json` |
| `target-chat-event.schema.json` | `examples/target-chat-event.json` |
| `target-model-list-response.schema.json` | `examples/target-model-list-response.json` |
| `target-knowledge-upload-request.schema.json` | `examples/target-knowledge-upload-request.json` |
| `target-knowledge-upload-response.schema.json` | `examples/target-knowledge-upload-response.json` |
| `target-knowledge-status-response.schema.json` | `examples/target-knowledge-status-response.json` |
| `judge-request.schema.json` | `examples/judge-request.json` |
| `judge-result.schema.json` | `examples/judge-result.json` |

## Validation Boundary

JSON Schema validates structure, types, local cardinality, and field limits. The service
Validator additionally enforces rules that standard JSON Schema cannot express reliably:

- exact Token budgets;
- uniqueness by semantic identity;
- weight sums within `1e-6`;
- minimum/maximum relationships;
- SQL AST safety and execution;
- Dataset entity consistency;
- capability-to-Tool mapping;
- expected Tool Chain compilation;
- allowed mutation scope;
- exact requested Test Case count.
- Agent Tool Call sequence is contiguous from 1 and call IDs are unique.
- Agent response status, Final Output, and Error fields are mutually consistent.
- Judge criteria IDs exactly match the requested set and are unique.
- Dataset difficulty minimum does not exceed maximum.

No persisted artifact is valid merely because it passes JSON Schema.

## Versioning

Contracts use semantic versions in payload `schema_version` fields. Compatible minor versions
add optional fields. A major version requires a deterministic upgrader that creates a new
Artifact Revision; historical revisions and Runs are never changed in place.
