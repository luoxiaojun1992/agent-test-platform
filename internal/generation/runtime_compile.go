package generation

import (
	"fmt"

	"agent-test-platform/internal/sandbox"
	"agent-test-platform/internal/toolmatch"
)

// RuntimeToolChain converts a stored Expected Tool Call Chain into the matcher form without
// re-validating it against a Scope; the Test Case was already validated before publication.
func RuntimeToolChain(source ToolChain) toolmatch.Chain {
	chain := toolmatch.Chain{AllowExtraReadCalls: source.AllowExtraReadCalls, MaxActualCalls: source.MaxActualCalls}
	for _, step := range source.Steps {
		compiled := toolmatch.Step{ID: step.ID, ToolName: step.ToolName, Required: step.Required, Critical: step.Critical, Weight: step.Weight, ArgumentsSchema: step.ArgumentsSchema}
		compiled.SQL = runtimeSQLMatch(step.SQLMatch)
		if step.ResultMatch != nil {
			compiled.Result = &toolmatch.ResultMatch{Status: toolmatch.CallStatus(step.ResultMatch.Status)}
			if step.ResultMatch.AffectedRows != nil {
				compiled.Result.MinAffectedRows = step.ResultMatch.AffectedRows.Min
				compiled.Result.MaxAffectedRows = step.ResultMatch.AffectedRows.Max
			}
		}
		chain.Steps = append(chain.Steps, compiled)
	}
	for _, forbidden := range source.Forbidden {
		chain.Forbidden = append(chain.Forbidden, toolmatch.ForbiddenCall{ToolName: forbidden.ToolName, SQL: runtimeSQLMatch(forbidden.SQLMatch)})
	}
	return chain
}

func runtimeSQLMatch(source *SQLMatch) *toolmatch.SQLMatch {
	if source == nil {
		return nil
	}
	compiled := &toolmatch.SQLMatch{Operation: source.Operation, RequiredTables: source.RequiredTables, ForbiddenTables: source.ForbiddenTables, RequiredColumns: source.RequiredColumns}
	for _, predicate := range source.RequiredPredicates {
		compiled.RequiredPredicates = append(compiled.RequiredPredicates, toolmatch.Predicate{Column: predicate.Column, Operator: predicate.Operator, Value: predicate.Value})
	}
	for _, assignment := range source.RequiredAssignments {
		compiled.RequiredAssignments = append(compiled.RequiredAssignments, toolmatch.Assignment{Column: assignment.Column, Value: assignment.Value})
	}
	return compiled
}

// RuntimeMutationRules converts allowed state changes into sandbox rules.
func RuntimeMutationRules(rules []MutationRule) []sandbox.MutationRule {
	compiled := make([]sandbox.MutationRule, 0, len(rules))
	for index, rule := range rules {
		compiled = append(compiled, sandbox.MutationRule{
			ID: fmt.Sprintf("rule-%d", index), Table: rule.Table, Operation: sandbox.Operation(rule.Operation), Columns: rule.Columns,
			Predicate:       sandbox.Predicate{Column: rule.Predicate.Column, Operator: rule.Predicate.Operator, Value: rule.Predicate.Value},
			MinAffectedRows: rule.MinAffectedRows, MaxAffectedRows: rule.MaxAffectedRows,
		})
	}
	return compiled
}
