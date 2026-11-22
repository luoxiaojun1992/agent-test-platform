package contracts

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const contractBaseURL = "https://agent-test-platform.local/"

// Files contains the versioned API, schemas, and positive contract examples.
//
//go:embed openapi.yaml target-openapi.yaml schemas/*.json examples/*.json
var Files embed.FS

var ExampleSchemas = map[string]string{
	"examples/canonical-sql-result.json":             "schemas/canonical-sql-result.schema.json",
	"examples/create-test-case.json":                 "schemas/create-test-case.schema.json",
	"examples/critic-result.json":                    "schemas/critic-result.schema.json",
	"examples/dataset-constraints.json":              "schemas/dataset-constraints.schema.json",
	"examples/dataset-metadata.json":                 "schemas/dataset-metadata.schema.json",
	"examples/generation-manifest.json":              "schemas/generation-manifest.schema.json",
	"examples/generation-request.json":               "schemas/generation-request.schema.json",
	"examples/judge-request.json":                    "schemas/judge-request.schema.json",
	"examples/judge-result.json":                     "schemas/judge-result.schema.json",
	"examples/normalized-generation-request.json":    "schemas/normalized-generation-request.schema.json",
	"examples/scenario.json":                         "schemas/scenario.schema.json",
	"examples/target-chat-event.json":                "schemas/target-chat-event.schema.json",
	"examples/target-chat-request.json":              "schemas/target-chat-request.schema.json",
	"examples/target-knowledge-status-response.json": "schemas/target-knowledge-status-response.schema.json",
	"examples/target-knowledge-upload-request.json":  "schemas/target-knowledge-upload-request.schema.json",
	"examples/target-knowledge-upload-response.json": "schemas/target-knowledge-upload-response.schema.json",
	"examples/target-model-list-response.json":       "schemas/target-model-list-response.schema.json",
	"examples/target-tool-list-response.json":        "schemas/target-tool-list-response.schema.json",
	"examples/test-case.json":                        "schemas/test-case.schema.json",
}

// Validator holds compiled, immutable JSON Schemas and is safe for concurrent validation.
type Validator struct {
	schemas map[string]*jsonschema.Schema
}

// NewValidator compiles every embedded JSON Schema.
func NewValidator() (*Validator, error) {
	schemas, err := CompileSchemas()
	if err != nil {
		return nil, err
	}
	return &Validator{schemas: schemas}, nil
}

// ValidateJSON validates one JSON document against an embedded schema path.
func (validator *Validator) ValidateJSON(schemaPath string, data []byte) error {
	schema, ok := validator.schemas[schemaPath]
	if !ok {
		return fmt.Errorf("unknown schema %q", schemaPath)
	}
	var document any
	if err := json.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("parse JSON: %w", err)
	}
	if err := schema.Validate(document); err != nil {
		return fmt.Errorf("validate against %s: %w", schemaPath, err)
	}
	return nil
}

// CompileSchemas compiles every embedded schema and resolves references by $id.
func CompileSchemas() (map[string]*jsonschema.Schema, error) {
	paths, err := fs.Glob(Files, "schemas/*.json")
	if err != nil {
		return nil, fmt.Errorf("glob schemas: %w", err)
	}
	sort.Strings(paths)

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	ids := make(map[string]string, len(paths))
	seenIDs := make(map[string]string, len(paths))

	for _, schemaPath := range paths {
		document, err := readJSON(schemaPath)
		if err != nil {
			return nil, err
		}
		object, ok := document.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("schema %s: root must be an object", schemaPath)
		}
		id, ok := object["$id"].(string)
		if !ok || id == "" {
			return nil, fmt.Errorf("schema %s: missing $id", schemaPath)
		}
		if previous, exists := seenIDs[id]; exists {
			return nil, fmt.Errorf("schema %s: duplicate $id %q also used by %s", schemaPath, id, previous)
		}
		if err := compiler.AddResource(id, document); err != nil {
			return nil, fmt.Errorf("register schema %s: %w", schemaPath, err)
		}
		ids[schemaPath] = id
		seenIDs[id] = schemaPath
	}

	compiled := make(map[string]*jsonschema.Schema, len(paths))
	for _, schemaPath := range paths {
		schema, err := compiler.Compile(ids[schemaPath])
		if err != nil {
			return nil, fmt.Errorf("compile schema %s: %w", schemaPath, err)
		}
		compiled[schemaPath] = schema
	}
	return compiled, nil
}

// ValidateExamples validates every positive example against its mapped schema.
func ValidateExamples() error {
	validator, err := NewValidator()
	if err != nil {
		return err
	}

	examples := make([]string, 0, len(ExampleSchemas))
	for example := range ExampleSchemas {
		examples = append(examples, example)
	}
	sort.Strings(examples)

	for _, examplePath := range examples {
		document, err := readJSON(examplePath)
		if err != nil {
			return err
		}
		schemaPath := ExampleSchemas[examplePath]
		schema, ok := validator.schemas[schemaPath]
		if !ok {
			return fmt.Errorf("example %s: schema %s was not compiled", examplePath, schemaPath)
		}
		if err := schema.Validate(document); err != nil {
			return fmt.Errorf("validate example %s against %s: %w", examplePath, schemaPath, err)
		}
	}
	return nil
}

// LoadOpenAPI loads and validates the embedded OpenAPI document and external refs.
func LoadOpenAPI(ctx context.Context) (*openapi3.T, error) {
	return loadOpenAPI(ctx, "openapi.yaml")
}

// LoadTargetOpenAPI loads and validates the Target compatibility API contract.
func LoadTargetOpenAPI(ctx context.Context) (*openapi3.T, error) {
	return loadOpenAPI(ctx, "target-openapi.yaml")
}

func loadOpenAPI(ctx context.Context, name string) (*openapi3.T, error) {
	root, err := url.Parse(contractBaseURL + name)
	if err != nil {
		return nil, fmt.Errorf("parse OpenAPI URI: %w", err)
	}

	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true
	loader.ReadFromURIFunc = readContractURI
	document, err := loader.LoadFromURI(root)
	if err != nil {
		return nil, fmt.Errorf("load OpenAPI: %w", err)
	}
	if !document.IsOpenAPI31OrLater() {
		return nil, fmt.Errorf("OpenAPI version %q is older than 3.1", document.OpenAPI)
	}
	if err := document.Validate(ctx); err != nil {
		return nil, fmt.Errorf("validate OpenAPI: %w", err)
	}
	return document, nil
}

func readJSON(filePath string) (any, error) {
	data, err := Files.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filePath, err)
	}
	var document any
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("parse %s: %w", filePath, err)
	}
	return document, nil
}

func readContractURI(_ *openapi3.Loader, location *url.URL) ([]byte, error) {
	if location.Scheme != "https" || location.Host != "agent-test-platform.local" {
		return nil, fmt.Errorf("external contract URI is not allowed: %s", location)
	}
	filePath := strings.TrimPrefix(path.Clean(location.Path), "/")
	if filePath == "." || strings.HasPrefix(filePath, "../") {
		return nil, fmt.Errorf("invalid contract URI path: %s", location)
	}
	data, err := Files.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read contract URI %s: %w", location, err)
	}
	return data, nil
}
