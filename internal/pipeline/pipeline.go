// Package pipeline defines the run pipeline DAG — the execution plan of a multi-turn Test Run —
// and the functions to derive, validate, and walk it. A pipeline is a schema-validated JSON
// document stored on the Run (technical/10 §3.6); the engine here only ever executes a validated DAG.
package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"time"

	"agent-test-platform/contracts"
)

const pipelineSchema = "schemas/pipeline.schema.json"

// Node types registered for v1. New types only add a case here and a node executor.
const (
	NodeTypeInit   = "init"
	NodeTypeTurn   = "turn"
	NodeTypeJudge  = "judge"
	NodeTypeFinish = "finish"
)

// StatusSkip marks a node that was bypassed because an earlier node failed the iteration. The
// pipeline still reaches finish (the teardown is guaranteed), but the skipped nodes never ran.
const StatusSkip = "SKIP"

// Bounds mirrored from pipeline.schema.json so derivation and validation stay in sync.
const (
	MaxTurns       = 16
	MaxRepetitions = 10
	MaxKnowledge   = 8
	MaxTables      = 20
)

// ErrInvalid marks a pipeline that fails a programmatic check.
var ErrInvalid = errors.New("pipeline is invalid")

// Pipeline is the run DAG plus its mutable runtime state: a collection of typed nodes and
// dependency edges, a static config, and the live context/result the engine rewrites as nodes run.
type Pipeline struct {
	SchemaVersion string       `json:"schema_version"`
	Config        Config       `json:"config"`
	Nodes         []Node       `json:"nodes"`
	Edges         []Edge       `json:"edges"`
	Context       Context      `json:"context"`
	Result        []NodeResult `json:"result"`
}

// Config is the static, immutable execution configuration of the Run.
type Config struct {
	Repetitions int `json:"repetitions"`
}

// Context is the Run's mutable runtime state, persisted inside pipeline_json. Its teardown handles
// are bounded so the pipeline record does not balloon (technical/10 §3.2).
type Context struct {
	Iteration            int       `json:"iteration"`
	RoundPassed          bool      `json:"round_passed"`
	KnowledgeDocumentIDs []string  `json:"knowledge_document_ids"`
	DatabaseTables       []string  `json:"database_tables"`
	IterationScores      []float64 `json:"iteration_scores"`
}

// NodeResult is one short, report-oriented record of a node execution. It never carries the test
// prompt, the LLM output, or the judge's reasoning (technical/10 §3.3).
type NodeResult struct {
	Iteration  int      `json:"iteration"`
	NodeID     string   `json:"node_id"`
	Type       string   `json:"type"`
	Status     string   `json:"status"`
	Pass       *bool    `json:"pass,omitempty"`
	FinalScore *float64 `json:"final_score,omitempty"`
	StartedAt  string   `json:"started_at,omitempty"`
	FinishedAt string   `json:"finished_at,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// Node is one executable unit. Type selects a registered executor; Params carries the
// executor-specific parameters (a free-form object validated by the executor).
type Node struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Params json.RawMessage `json:"params"`
}

// Edge is a dependency from one node to another. An edge without When is static: the target runs
// once the source completes. An edge with When is a runtime condition evaluated against the
// source node's output.
type Edge struct {
	From string     `json:"from"`
	To   string     `json:"to"`
	When *Condition `json:"when,omitempty"`
}

// Condition is a minimal predicate: the source node's output field must equal a value.
type Condition struct {
	Field  string `json:"field"`
	Equals any    `json:"equals"`
}

// TurnParams is the executor parameter object carried by the turn and judge node types.
type TurnParams struct {
	Turn int `json:"turn"`
}

// Turn returns the turn number carried by a turn/judge node's params.
func (node Node) Turn() (int, error) {
	var params TurnParams
	if err := json.Unmarshal(node.Params, &params); err != nil {
		return 0, fmt.Errorf("node %s params: %w", node.ID, err)
	}
	return params.Turn, nil
}

// IsInit reports whether the node is the init lifecycle node.
func (node Node) IsInit() bool { return node.Type == NodeTypeInit }

// IsFinish reports whether the node is the finish lifecycle node.
func (node Node) IsFinish() bool { return node.Type == NodeTypeFinish }

// IsTurn reports whether the node is a turn node.
func (node Node) IsTurn() bool { return node.Type == NodeTypeTurn }

// IsJudge reports whether the node is a judge node.
func (node Node) IsJudge() bool { return node.Type == NodeTypeJudge }

// TurnCount returns the number of turn/judge pairs, inferred from the node set.
func (p Pipeline) TurnCount() int {
	count := 0
	for _, node := range p.Nodes {
		if node.Type == NodeTypeTurn {
			count++
		}
	}
	return count
}

// TurnNodeID returns the turn node id for a 1-based turn number.
func TurnNodeID(turn int) string { return "turn-" + strconv.Itoa(turn) }

// JudgeNodeID returns the judge node id for a 1-based turn number.
func JudgeNodeID(turn int) string { return "judge-" + strconv.Itoa(turn) }

// InitNodeID is the fixed init node id.
const InitNodeID = "init"

// FinishNodeID is the fixed finish node id.
const FinishNodeID = "finish"

// Derive builds the init/turn/judge/finish pipeline for a Test Case with the given turn count and
// repetition count:
//
//	init → turn-1 → judge-1 → (pass) turn-2 → judge-2 → … → turn-N → judge-N → finish
//
// Each intermediate judge has a conditional edge to the next turn (when pass == true) and to finish
// (when pass == false); the last judge always reaches finish. A single turn yields
// init → turn-1 → judge-1 → finish, which matches the 09 single-turn behavior for repetitions = 1.
func Derive(turnCount, repetitions int) (Pipeline, error) {
	if turnCount < 1 || turnCount > MaxTurns {
		return Pipeline{}, fmt.Errorf("%w: turn count must be 1-%d", ErrInvalid, MaxTurns)
	}
	if repetitions < 1 || repetitions > MaxRepetitions {
		return Pipeline{}, fmt.Errorf("%w: repetitions must be 1-%d", ErrInvalid, MaxRepetitions)
	}
	p := Pipeline{
		SchemaVersion: "1.0",
		Config:        Config{Repetitions: repetitions},
		Context: Context{
			Iteration:            1,
			RoundPassed:          true,
			KnowledgeDocumentIDs: []string{},
			DatabaseTables:       []string{},
			IterationScores:      []float64{},
		},
		Result: []NodeResult{},
	}
	p.Nodes = append(p.Nodes, Node{ID: InitNodeID, Type: NodeTypeInit, Params: json.RawMessage(`{}`)})
	for n := 1; n <= turnCount; n++ {
		params := mustParams(n)
		p.Nodes = append(p.Nodes, Node{ID: TurnNodeID(n), Type: NodeTypeTurn, Params: params})
		p.Nodes = append(p.Nodes, Node{ID: JudgeNodeID(n), Type: NodeTypeJudge, Params: params})
	}
	p.Nodes = append(p.Nodes, Node{ID: FinishNodeID, Type: NodeTypeFinish, Params: json.RawMessage(`{}`)})

	p.Edges = append(p.Edges, Edge{From: InitNodeID, To: TurnNodeID(1)})
	for n := 1; n <= turnCount; n++ {
		p.Edges = append(p.Edges, Edge{From: TurnNodeID(n), To: JudgeNodeID(n)})
		if n < turnCount {
			p.Edges = append(p.Edges, Edge{From: JudgeNodeID(n), To: TurnNodeID(n + 1), When: &Condition{Field: "pass", Equals: true}})
			p.Edges = append(p.Edges, Edge{From: JudgeNodeID(n), To: FinishNodeID, When: &Condition{Field: "pass", Equals: false}})
		} else {
			// The last judge always reaches finish, whatever its verdict.
			p.Edges = append(p.Edges, Edge{From: JudgeNodeID(n), To: FinishNodeID})
		}
	}
	return p, nil
}

func mustParams(turn int) json.RawMessage {
	data, _ := json.Marshal(TurnParams{Turn: turn})
	return data
}

// Marshal encodes the pipeline and validates it against pipeline.schema.json
// (validate-by-construction).
func Marshal(validator *contracts.Validator, p Pipeline) ([]byte, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encode pipeline: %w", err)
	}
	if err := validator.ValidateJSON(pipelineSchema, data); err != nil {
		return nil, fmt.Errorf("pipeline contract: %w", err)
	}
	return data, nil
}

// Parse decodes a stored pipeline, schema-validates it, then runs the programmatic checks, so a
// corrupt or schema-drifted row fails fast instead of mis-executing.
func Parse(validator *contracts.Validator, data []byte) (Pipeline, error) {
	if err := validator.ValidateJSON(pipelineSchema, data); err != nil {
		return Pipeline{}, fmt.Errorf("pipeline contract: %w", err)
	}
	var p Pipeline
	if err := json.Unmarshal(data, &p); err != nil {
		return Pipeline{}, fmt.Errorf("decode pipeline: %w", err)
	}
	if err := Validate(p); err != nil {
		return Pipeline{}, err
	}
	return p, nil
}

// Validate runs the programmatic checks a JSON schema cannot express: unique node ids, edges that
// reference existing nodes, an acyclic graph, registered node types, per-type params, and a node set
// that matches the turn count (one init, one finish, and a turn/judge pair per turn).
func Validate(p Pipeline) error {
	if p.Config.Repetitions < 1 || p.Config.Repetitions > MaxRepetitions {
		return fmt.Errorf("%w: repetitions must be 1-%d", ErrInvalid, MaxRepetitions)
	}
	ids := make(map[string]struct{}, len(p.Nodes))
	turnSeen := make(map[int]struct{}, len(p.Nodes)/2)
	judgeSeen := make(map[int]struct{}, len(p.Nodes)/2)
	initCount, finishCount := 0, 0
	for _, node := range p.Nodes {
		if _, exists := ids[node.ID]; exists {
			return fmt.Errorf("%w: duplicate node id %q", ErrInvalid, node.ID)
		}
		ids[node.ID] = struct{}{}
		switch node.Type {
		case NodeTypeInit:
			initCount++
		case NodeTypeFinish:
			finishCount++
		case NodeTypeTurn, NodeTypeJudge:
			var params TurnParams
			if err := json.Unmarshal(node.Params, &params); err != nil || params.Turn < 1 || params.Turn > MaxTurns {
				return fmt.Errorf("%w: node %q params must be {\"turn\": 1..%d}", ErrInvalid, node.ID, MaxTurns)
			}
			if node.Type == NodeTypeTurn {
				turnSeen[params.Turn] = struct{}{}
			} else {
				judgeSeen[params.Turn] = struct{}{}
			}
		default:
			return fmt.Errorf("%w: unregistered node type %q", ErrInvalid, node.Type)
		}
	}
	if initCount != 1 || finishCount != 1 {
		return fmt.Errorf("%w: pipeline must have exactly one init and one finish node", ErrInvalid)
	}
	if len(turnSeen) != len(judgeSeen) || len(turnSeen) == 0 {
		return fmt.Errorf("%w: pipeline must pair every turn node with a judge node", ErrInvalid)
	}
	for turn := range turnSeen {
		if _, ok := judgeSeen[turn]; !ok {
			return fmt.Errorf("%w: turn %d has no judge node", ErrInvalid, turn)
		}
	}
	for _, edge := range p.Edges {
		if _, ok := ids[edge.From]; !ok {
			return fmt.Errorf("%w: edge from %q references an unknown node", ErrInvalid, edge.From)
		}
		if _, ok := ids[edge.To]; !ok {
			return fmt.Errorf("%w: edge to %q references an unknown node", ErrInvalid, edge.To)
		}
	}
	if !acyclic(p) {
		return fmt.Errorf("%w: pipeline contains a cycle", ErrInvalid)
	}
	return nil
}

// acyclic reports whether the pipeline has no cycle (Kahn's algorithm).
func acyclic(p Pipeline) bool {
	indegree := make(map[string]int, len(p.Nodes))
	adj := make(map[string][]string, len(p.Nodes))
	for _, node := range p.Nodes {
		indegree[node.ID] = 0
	}
	for _, edge := range p.Edges {
		adj[edge.From] = append(adj[edge.From], edge.To)
		indegree[edge.To]++
	}
	var queue []string
	for _, node := range p.Nodes {
		if indegree[node.ID] == 0 {
			queue = append(queue, node.ID)
		}
	}
	visited := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		visited++
		for _, next := range adj[id] {
			indegree[next]--
			if indegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	return visited == len(p.Nodes)
}

// Find returns the node with the given id.
func (p Pipeline) Find(id string) (Node, bool) {
	for _, node := range p.Nodes {
		if node.ID == id {
			return node, true
		}
	}
	return Node{}, false
}

// NodesAfter returns the turn/judge nodes that come after the given node in the linear pipeline,
// excluding finish (finish still runs as the teardown). A failed node marks these as skipped.
func (p Pipeline) NodesAfter(id string) []Node {
	var after []Node
	found := false
	for _, node := range p.Nodes {
		if node.ID == id {
			found = true
			continue
		}
		if found && node.ID != FinishNodeID {
			after = append(after, node)
		}
	}
	return after
}

// NodeOutput is the structured result of a completed node, read by conditions and the final verdict.
type NodeOutput struct {
	Status           string  `json:"status,omitempty"`            // turn/finish: COMPLETED / TIMEOUT / AGENT_ERROR / CANCELLED
	SessionID        string  `json:"session_id,omitempty"`        // turn
	FinalOutput      string  `json:"final_output,omitempty"`      // turn, display-only
	ContextWatermark int     `json:"context_watermark,omitempty"` // turn: highest context sequence after this turn
	Pass             *bool   `json:"pass,omitempty"`              // judge
	LLMScore         float64 `json:"llm_score,omitempty"`         // judge
	FinalScore       float64 `json:"final_score,omitempty"`       // judge
}

// NextNodes returns the ids of nodes that become eligible after fromID completes with the given
// output. For the v1 linear pipeline every node has at most one incoming edge, so evaluating
// fromID's outgoing edges is sufficient; a static edge always enables its target, and a conditional
// edge enables it only when the condition holds.
func (p Pipeline) NextNodes(fromID string, output NodeOutput) []string {
	var next []string
	for _, edge := range p.Edges {
		if edge.From != fromID {
			continue
		}
		if edge.When == nil || edge.When.holds(output) {
			next = append(next, edge.To)
		}
	}
	return next
}

func (c Condition) holds(output NodeOutput) bool {
	data, _ := json.Marshal(output)
	var fields map[string]any
	if json.Unmarshal(data, &fields) != nil {
		return false
	}
	value, ok := fields[c.Field]
	return ok && reflect.DeepEqual(value, c.Equals)
}

// NewNodeResult builds a NodeResult record with timestamps; timestamps are omitted when empty.
func NewNodeResult(iteration int, nodeID, nodeType, status string, startedAt, finishedAt time.Time) NodeResult {
	record := NodeResult{Iteration: iteration, NodeID: nodeID, Type: nodeType, Status: status}
	if !startedAt.IsZero() {
		record.StartedAt = startedAt.UTC().Format(time.RFC3339)
	}
	if !finishedAt.IsZero() {
		record.FinishedAt = finishedAt.UTC().Format(time.RFC3339)
	}
	return record
}

// SkipResult returns a minimal SKIP record for a bypassed node (no timestamps or scores).
func SkipResult(iteration int, node Node) NodeResult {
	return NodeResult{Iteration: iteration, NodeID: node.ID, Type: node.Type, Status: StatusSkip}
}

// Round2 rounds a score to two decimals so the result record stays short.
func Round2(value float64) float64 {
	return math.Round(value*100) / 100
}
