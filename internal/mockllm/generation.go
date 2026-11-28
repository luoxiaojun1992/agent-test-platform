package mockllm

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"agent-test-platform/contracts"
)

// Scripted generation outputs. They describe one small "orders" Dataset and Test Cases over it, so a whole
// generation package (scenario -> dataset -> test cases) can be produced and validated without a real model.

const mockOrderCount = 6

const mockDatasetSchemaSQL = "CREATE TABLE orders (id VARCHAR(16) NOT NULL, status VARCHAR(16) NOT NULL, amount INT NOT NULL, PRIMARY KEY (id));"

func mockDatasetSeedSQL() string {
	rows := make([]string, 0, mockOrderCount)
	for index := 1; index <= mockOrderCount; index++ {
		rows = append(rows, fmt.Sprintf("('O%d', 'PAID', %d)", index, index*100))
	}
	return "INSERT INTO orders (id, status, amount) VALUES " + strings.Join(rows, ", ") + ";"
}

type decodedMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func decodeMessages(messages []json.RawMessage) []decodedMessage {
	decoded := make([]decodedMessage, 0, len(messages))
	for _, raw := range messages {
		var message decodedMessage
		if json.Unmarshal(raw, &message) == nil {
			decoded = append(decoded, message)
		}
	}
	return decoded
}

func contentText(message decodedMessage) string {
	text, _ := message.Content.(string)
	return text
}

func countToolResults(messages []decodedMessage) int {
	count := 0
	for _, message := range messages {
		if message.Role == "tool" {
			count++
		}
	}
	return count
}

func hasTool(request completionRequest, name string) bool {
	for _, tool := range request.Tools {
		if tool.Function.Name == name {
			return true
		}
	}
	return false
}

var (
	jobIDPattern      = regexp.MustCompile(`job_id="([^"]+)"`)
	scenarioIDPattern = regexp.MustCompile(`scenario_revision_id="([^"]+)"`)
	datasetIDPattern  = regexp.MustCompile(`dataset_revision_id="([^"]+)"`)
)

// scriptedToolCall answers the generation Tool loops: the Dataset Critic (read the table, then submit its review) and the
// Test Case generator (one create_test_case call per requested candidate).
func scriptedToolCall(request completionRequest) (string, any, bool, error) {
	messages := decodeMessages(request.Messages)
	switch {
	case hasTool(request, "submit_critic_review"):
		if countToolResults(messages) == 0 {
			return "read_dataset_rows", map[string]any{"table": "orders", "offset": 0}, true, nil
		}
		review, err := contracts.Files.ReadFile("examples/critic-result.json")
		if err != nil {
			return "", nil, false, err
		}
		var arguments any
		if err := json.Unmarshal(review, &arguments); err != nil {
			return "", nil, false, err
		}
		return "submit_critic_review", arguments, true, nil
	case hasTool(request, "create_test_case"):
		return scriptedTestCase(messages)
	}
	return "", nil, false, nil
}

func scriptedTestCase(messages []decodedMessage) (string, any, bool, error) {
	var user string
	for _, message := range messages {
		if message.Role == "user" && user == "" {
			user = contentText(message)
		}
	}
	job, scenario, dataset := firstMatch(jobIDPattern, user), firstMatch(scenarioIDPattern, user), firstMatch(datasetIDPattern, user)
	if job == "" || scenario == "" || dataset == "" {
		return "", nil, false, fmt.Errorf("mock Test Case generator could not find the fixed ids in the prompt")
	}
	index := countToolResults(messages) % mockOrderCount
	order := "O" + strconv.Itoa(index+1)
	candidate := map[string]any{
		"schema_version": "2.0", "job_id": job, "client_case_id": "order-" + strings.ToLower(order),
		"scenario_revision_id": scenario, "dataset_revision_id": dataset,
		"turns": []any{
			map[string]any{
				"turn":   1,
				"prompt": "What is the current status of order " + order + "?",
				"expected_output": map[string]any{"type": "SEMANTIC",
					"value": "The response states that order " + order + " is PAID."},
				"evaluation_criteria": []any{
					map[string]any{"name": "correct_status", "description": "Reports the correct status.", "weight": 0.6},
					map[string]any{"name": "clear_answer", "description": "Names the order and its state.", "weight": 0.4},
				},
			},
			map[string]any{
				"turn":   2,
				"prompt": "Now cancel order " + order + " and confirm.",
				"expected_output": map[string]any{"type": "SEMANTIC",
					"value": "The response confirms that order " + order + " was cancelled."},
				"evaluation_criteria": []any{
					map[string]any{"name": "cancelled", "description": "Confirms cancellation.", "weight": 0.7},
					map[string]any{"name": "correct_order", "description": "Refers to the right order.", "weight": 0.3},
				},
			},
		},
		"knowledge_documents": []any{},
		"difficulty":          "EASY", "generation_assumptions": []any{},
		"difficulty_profile": map[string]any{"data_complexity": 1, "reasoning_complexity": 1, "tool_complexity": 1, "ambiguity": 1, "multi_step": 4, "state_change_complexity": 2, "adversarial_complexity": 1},
	}
	return "create_test_case", candidate, true, nil
}

func firstMatch(pattern *regexp.Regexp, text string) string {
	if match := pattern.FindStringSubmatch(text); match != nil {
		return match[1]
	}
	return ""
}

func scriptedDataset() (json.RawMessage, error) {
	return json.Marshal(map[string]any{
		"schema_sql": mockDatasetSchemaSQL, "seed_sql": mockDatasetSeedSQL(),
		"constraints": map[string]any{"version": "1.0", "checks": []any{
			map[string]any{
				"id": "orders-seeded", "phases": []any{"AFTER_SEED"}, "query": "SELECT id FROM orders",
				"expectation": map[string]any{"type": "ROW_COUNT", "equals": mockOrderCount}, "critical": true, "rule_id": "orders-exist",
			},
		}},
		"difficulty_profile": map[string]any{"data_complexity": 1, "reasoning_complexity": 1, "tool_complexity": 1, "ambiguity": 1, "multi_step": 1, "state_change_complexity": 1, "adversarial_complexity": 1},
		"coverage_tags":      []any{"NORMAL"},
	})
}

// isDatasetSchema recognises the Dataset generation response schema by its model-facing members.
func isDatasetSchema(document map[string]any) bool {
	properties, _ := document["properties"].(map[string]any)
	_, hasSchema := properties["schema_sql"]
	_, hasSeed := properties["seed_sql"]
	return hasSchema && hasSeed
}
