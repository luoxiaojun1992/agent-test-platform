# Generation and Validation Design

## 1. Stage Contracts

Every stage consumes an immutable input snapshot and emits a versioned JSON document:

| Stage | Input | Output | Terminal Validation |
|---|---|---|---|
| Scenario | normalized request | scenario draft | JSON Schema + coverage rules + Critic (Scenario-only scope) |
| Dataset | request + scenario | SQL scripts + constraints + metadata | AST + validation MySQL execution + Critic (Scenario+Dataset scope) |
| Test Case | scenario + dataset catalog | Tool calls | `create_test_case` validator + Critic (Scenario+Dataset+Test Case scope) |
| Publisher | one Job's accepted output | immutable revision(s) | digest + transaction checks |

Critic is part of each stage's Terminal Validation, not a separate stage: it runs once per
`VALIDATING_<KIND>`, only after that stage's deterministic checks pass, and its ACCEPT/REJECT
combines with the deterministic result into one Accept/Reject decision for the stage (see
Section 9). There is no independent `CRITIC_REVIEW` Job status.

Prompt templates, Tool schemas, normalizers, and validators each have an independent semantic
version recorded in the Generation Manifest.

`create_test_case` is a transport contract and includes `job_id` and `client_case_id`. During
atomic publication, the Publisher removes those transport fields, replaces Draft Revision IDs
with final ULIDs, and validates the result against `test-case.schema.json` before computing the
immutable content digest.

## 2. Scenario Rules

Business rules carry provenance:

```json
{
  "id": "cancel-shipped-order",
  "text": "Shipped orders cannot be cancelled.",
  "source": "SOURCE_REQUIREMENT",
  "critical": true
}
```

Allowed sources:

- `SOURCE_REQUIREMENT`: explicitly stated by the user or referenced approved policy.
- `GENERATED_ASSUMPTION`: inferred by the model.

A Package containing generated assumptions is marked `SYNTHETIC`. A `SYNTHETIC` Suite can be
automatically activated and executed, but UI and reports must not call it authoritative or
production-grounded. A future approved policy source may upgrade a new revision to
`SOURCE_GROUNDED`; an existing revision is never relabeled.

## 3. Dataset Constraint Contract

`constraints.json` contains executable, read-only assertions rather than prose-only rules:

```json
{
  "version": "1.0",
  "checks": [
    {
      "id": "cancelled-order-has-no-active-shipment",
      "phases": ["AFTER_SEED", "AFTER_AGENT"],
      "query": "SELECT o.id FROM orders o JOIN shipment s ON s.order_id = o.id WHERE o.status = 'CANCELLED' AND s.status IN ('PENDING', 'SHIPPED') ORDER BY o.id",
      "expectation": {"type": "ROW_COUNT", "equals": 0},
      "critical": true,
      "rule_id": "cancel-shipped-order"
    }
  ]
}
```

Rules:

- Query is one read-only `SELECT` and passes the same AST safety checks as Post Conditions.
- `AFTER_SEED` proves the generated fixture is initially consistent.
- `AFTER_AGENT` detects unintended cross-entity damage.
- A critical invariant failure invalidates a Dataset during generation or hard-fails a Run.
- Free-form constraint text may be retained for display but is not executable evidence.

## 4. Canonical SQL Result

All SQL assertions use this typed representation:

```json
{
  "columns": [
    {"name": "status", "type": "STRING"}
  ],
  "rows": [
    [
      {"type": "STRING", "value": "CANCELLED"}
    ]
  ],
  "ordered": true,
  "truncated": false
}
```

Supported scalar types:

| Type | Canonical Value |
|---|---|
| `NULL` | JSON `null` |
| `STRING` | UTF-8 string |
| `INTEGER` | base-10 string without leading zeros except `0` |
| `DECIMAL` | normalized base-10 string; scale retained from expectation |
| `BOOLEAN` | JSON boolean |
| `DATE` | `YYYY-MM-DD` |
| `DATETIME` | RFC 3339 UTC with microseconds |
| `BINARY` | unpadded base64 string |
| `JSON` | canonical JSON value |

Comparison rules:

1. Column count, order, case-insensitive name, and type family must match.
2. Query returning potentially multiple rows requires an explicit deterministic `ORDER BY`.
3. Row count and row order must match exactly.
4. `NULL` only equals `NULL`.
5. Integer comparison is exact after canonicalization.
6. Decimal comparison is exact by default; an explicit absolute tolerance may be declared.
7. Datetime values are converted from Dataset timezone to UTC before comparison.
8. A truncated actual result never passes.
9. JSON object key order is ignored; array order is preserved.

Expected result schemas must use this representation. The compact two-dimensional examples in
the architecture are illustrative only.

## 5. State-Diff Enforcement

Every generated table must have a primary key. Before Agent execution, the Runner records a
canonical row digest keyed by `(table, primary-key tuple)`. After execution it records another
digest and computes inserts, updates, and deletes.

An `allowed_state_changes` rule contains:

```json
{
  "table": "orders",
  "operation": "UPDATE",
  "columns": ["status", "updated_at"],
  "predicate": {
    "column": "id",
    "operator": "=",
    "value": "O10001"
  },
  "min_affected_rows": 1,
  "max_affected_rows": 1
}
```

A Run hard-fails when:

- a changed table has no matching rule;
- the operation does not match;
- an updated column is not allowed;
- a changed row does not satisfy the predicate;
- affected rows are outside the configured range;
- the schema digest changes.

Each rule covers one operation. `INSERT` and `UPDATE` require a non-empty column allowlist;
`DELETE` requires an empty column list because row identity and count are enforced by the
predicate and affected-row range.

The 10,000-row Dataset limit makes full logical snapshots acceptable for MVP. Snapshots are
hashed and only diffs plus assertion evidence are persisted.

## 6. SQL Validation

Implementation uses a maintained MySQL AST parser; the initial engineering spike must validate
Vitess `sqlparser` against the contract corpus before dependency lock. The parser must support
all allowed MySQL 8.4 statements. Parser gaps fail closed.

Explicit allowlist:

| Context | Allowed Statements | Additional Restrictions |
|---|---|---|
| `schema.sql` | `CREATE TABLE`, `CREATE INDEX` | no triggers, views, functions, procedures, events, partitions, or cross-database names |
| `seed.sql` | `INSERT` | explicit columns, literal values, no `INSERT ... SELECT`, max 1,000 rows per statement |
| Constraint / Pre / Post | one `SELECT` | no locks, variables, temp tables, outfile, or nondeterministic functions |
| `mysql.query` | one `SELECT` | same restrictions plus row/time limit |
| `mysql.execute` | one `INSERT`, `UPDATE`, or `DELETE` | must satisfy Tool and Mutation Scope; no DDL or transaction control |

`CREATE TEMPORARY TABLE`, `ALTER`, `DROP`, `TRUNCATE`, `REPLACE`, `CALL`, `DO`, `SET`,
`LOAD DATA`, `LOCK TABLES`, explicit transaction statements, and administrative statements are
forbidden in every Agent-facing context.

Validation order:

1. Byte and statement-count limits.
2. Parse using MySQL dialect.
3. Reject comments that alter token boundaries, versioned comments, dynamic SQL, variables,
   procedures, functions, events, plugins, file access, and cross-database identifiers.
4. Enforce statement-type allowlist by artifact class.
5. Enforce table and row limits.
6. Execute with multi-statements disabled and a server-side timeout.
7. Verify Dataset constraints and canonical result shape.

Database privileges remain the final security boundary; AST validation is defense in depth.

The Tool Validator also performs rules that JSON Schema cannot express:

- evaluation criteria weights sum to `1.0` within `1e-6`;
- when any per-check weight is supplied inside a deterministic component, every scored check
  in that component must supply one and the weights sum to `1.0` within `1e-6`; otherwise checks
  in that component receive equal weight;
- minimum values do not exceed maximum values;
- IDs are unique within their arrays;
- Required Tool Step weights have a positive total;
- TASK mode contains a critical Post Condition, critical Tool Step, and Mutation Rule;
- every expected Tool is granted by the Agent capability-to-tool mapping.

## 7. Test Case Tool Error Catalog

| Code | Retryable | Meaning |
|---|---|---|
| `SCHEMA_INVALID` | yes | JSON does not match the versioned Tool schema |
| `LENGTH_LIMIT_EXCEEDED` | yes | Field or payload exceeds limit |
| `TOKEN_LIMIT_EXCEEDED` | yes | Prompt or output budget exceeds limit |
| `REFERENCE_NOT_FOUND` | no | Job or draft revision is missing |
| `REFERENCE_SCOPE_MISMATCH` | no | Referenced drafts belong to another Job |
| `SQL_PARSE_ERROR` | yes | SQL cannot be parsed |
| `SQL_MULTIPLE_STATEMENTS` | yes | More than one assertion statement supplied |
| `SQL_STATEMENT_NOT_ALLOWED` | yes | Statement type is not allowed |
| `SQL_CROSS_DATABASE_REFERENCE` | yes | SQL references another database |
| `SQL_NONDETERMINISTIC` | yes | Result depends on disallowed nondeterminism |
| `SQL_EXECUTION_ERROR` | yes | validation MySQL execution fails |
| `PRECONDITION_MISMATCH` | yes | Seed state does not match expected value |
| `RESULT_SCHEMA_INVALID` | yes | Expected result cannot be canonicalized |
| `MISSING_CRITICAL_POSTCONDITION` | yes | Task has no critical state assertion |
| `MUTATION_SCOPE_INVALID` | yes | Allowed state-change rule is unsafe or incomplete |
| `TOOL_CHAIN_SCHEMA_INVALID` | yes | Expected chain is malformed |
| `TOOL_UNKNOWN` | yes | Tool is not in the Agent capability registry |
| `TOOL_MATCHER_INVALID` | yes | Argument or SQL matcher cannot be compiled |
| `EVALUATION_WEIGHT_INVALID` | yes | Criteria weights do not sum to one |
| `ENTITY_NOT_FOUND` | yes | Prompt references data absent from Dataset |
| `DUPLICATE_CASE` | yes | Prompt and Oracle duplicate an accepted case |
| `CONTEXT_BUDGET_EXCEEDED` | no | Remaining Job budget is insufficient |
| `RESOURCE_LIMIT_EXCEEDED` | no | Job, Tool-call, SQL, or time limit is exhausted |
| `VALIDATION_DB_UNAVAILABLE` | no | Infrastructure failed; Job may be retried as a new Job |
| `GENERATION_JOB_DEADLINE_EXCEEDED` | no | Job exhausted its 10-minute automated processing budget (technical/01 section 3) |

`retryable` means eligible for the bounded LLM repair loop, not automatic Provider retry.

## 8. Repair Algorithm

Every `VALIDATING_<KIND>` rejection (deterministic or Critic) that is fully retryable enters
`REPAIRING_<KIND>` and follows the same shape; only the unit of repair differs by `job_kind`:

- `TEST_CASES`: the unit of repair is one candidate, identified by `client_case_id`. Only
  rejected candidates are regenerated; already-accepted candidates carry forward unchanged. A
  `package_findings`-only Critic rejection (no specific `client_case_id`) has no single candidate
  to blame and is treated as rejecting every currently-accepted candidate, so the whole batch is
  regenerated in that repair round.
- `SCENARIO` / `DATASET`: the unit of repair is the whole draft, since it is one document, not a
  list of independent items. The entire draft is regenerated.

Rules:

1. Group all retryable validation errors (deterministic and/or Critic findings) for the unit
   being repaired.
2. Send only unit identity, failed fields/findings, error codes, safe messages, and required
   catalog fragments to the same generation Role.
3. Do not send previous free-form model responses.
4. Validate the replacement as a complete unit (full candidate, or full Scenario/Dataset draft).
5. Stop after two repair rounds per Job (`MaxRepairRounds`), regardless of `job_kind`.
6. Provider transport retry does not consume a repair round; a new model output does.
7. Non-retryable error or exhausted round fails the entire Job with zero publication for that
   Job's own output. An already-published upstream Job (for example the Scenario a failed
   `DATASET` Job referenced) is unaffected.

No exponential backoff applies to semantic repair. Provider transport retries follow the LLM
Gateway policy.

## 9. Critic Result

The Critic runs once per `VALIDATING_<KIND>` stage, only after that stage's deterministic checks
pass, over the unit's already-established context: Scenario alone for `SCENARIO` Jobs,
Scenario+Dataset for `DATASET` Jobs, or Scenario+Dataset+all currently-accepted Test Cases for
`TEST_CASES` Jobs. There is no separate `CRITIC_REVIEW` Job status; its ACCEPT/REJECT decision
combines with the deterministic result into one `VALIDATING_<KIND>` outcome.

```json
{
  "decision": "ACCEPT",
  "coverage": {
    "requested": ["NORMAL", "EDGE_CASE", "AMBIGUOUS"],
    "covered": ["NORMAL", "EDGE_CASE", "AMBIGUOUS"]
  },
  "case_findings": [],
  "package_findings": [],
  "confidence": 0.91
}
```

`case_findings` (only meaningful for `TEST_CASES` Jobs) identify a specific `client_case_id` and
feed the per-candidate Repair Algorithm (Section 8) exactly like a deterministic Tool Validator
rejection. `package_findings` have no specific case and, when retryable, trigger a full redo of
the unit being validated. `REJECT` findings use the Tool error catalog where possible.
Model-reported confidence is stored for analysis but never bypasses deterministic validation or
controls publication by itself.

### Dataset Critic tool loop

For `DATASET` Jobs the Critic reads rows instead of receiving them. The Dataset document it reviews has
`seed_sql` removed. The message carries the schema, constraints, metadata, and the per-table row counts
from the execution schema. Rows are read with `read_dataset_rows` (`table`, `offset`, `limit`; default
50, at most 200). Each page is ordered by primary key (every column when the table has none), holds at
most 4 KiB of row data (a single larger row is still returned), and carries `next_offset` while more
rows remain. A review may make at most 16 reads. After that, reads return `READ_BUDGET_EXHAUSTED` and
the Critic must submit. The verdict is recorded only through `submit_critic_review`, whose arguments
are the Critic result object, validated against its schema; a failed check returns `REVIEW_INVALID` for
correction. Tool choice is required. Compaction follows technical/02 section 8.1, and the coverage
ledger (row ranges read per table) is the state anchor. Provenance records the prompt version
`semantic-critic-dataset/v1`, model and tool call counts, token usage, and the coverage ledger.
A review makes at most 24 model calls (16 reads and up to 8 further calls for submission and
correction) and runs under the 6-minute `VALIDATING_DATASET` Stage timeout (technical/01 section 3).

## 10. Schema Compatibility

Persisted artifacts retain their original `schema_version`. Runtime readers support the current
major version and the immediately previous major version. Compatible minor changes add optional
fields only. A major change requires:

1. a new Schema `$id` and version;
2. a pure, deterministic upgrader that creates a new Artifact Revision;
3. golden input/output fixtures for that upgrader;
4. no in-place modification of historical revisions or Runs.

Unsupported versions return `ARTIFACT_SCHEMA_UNSUPPORTED` before environment creation.
