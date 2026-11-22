package contracts

import "testing"

func TestReadDatasetRowsToolArgumentsAreBounded(t *testing.T) {
	t.Parallel()

	validator, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	const schemaPath = "schemas/read-dataset-rows.schema.json"
	for _, document := range []string{
		`{"table":"orders"}`,
		`{"table":"orders","offset":50,"limit":200}`,
	} {
		if err := validator.ValidateJSON(schemaPath, []byte(document)); err != nil {
			t.Errorf("valid arguments %s rejected: %v", document, err)
		}
	}
	for _, document := range []string{
		`{}`,
		`{"table":""}`,
		`{"table":"orders","offset":-1}`,
		`{"table":"orders","offset":1.5}`,
		`{"table":"orders","limit":0}`,
		`{"table":"orders","limit":201}`,
		`{"table":"orders","where":"1=1"}`,
	} {
		if err := validator.ValidateJSON(schemaPath, []byte(document)); err == nil {
			t.Errorf("invalid arguments %s accepted", document)
		}
	}
}
