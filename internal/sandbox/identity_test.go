package sandbox

import (
	"strings"
	"testing"
)

const (
	testDatasetRevisionID = "01K4Z6Y7X8W9V0T1S2R3Q4P5N6"
	testRunID             = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
)

func TestDeriveDatasetIdentifiers(t *testing.T) {
	t.Parallel()

	identifiers, err := DeriveDatasetIdentifiers(strings.ToLower(testDatasetRevisionID))
	if err != nil {
		t.Fatal(err)
	}
	if identifiers.Database != "dataset_01k4z6y7x8w9v0t1s2r3q4p5n6" {
		t.Errorf("database = %q", identifiers.Database)
	}
}

func TestDeriveDatasetIdentifiersRejectsUnsafeID(t *testing.T) {
	t.Parallel()

	for _, id := range []string{
		"",
		"01K4Z6Y7X8W9V0T1S2R3Q4P5N6`;DROP DATABASE platform;--",
		"01K4Z6Y7X8W9V0T1S2R3Q4P5I6",
		"01K4Z6Y7X8W9V0T1S2R3Q4P5N",
		"81K4Z6Y7X8W9V0T1S2R3Q4P5N6",
	} {
		if _, err := DeriveDatasetIdentifiers(id); err == nil {
			t.Errorf("DeriveDatasetIdentifiers(%q) succeeded", id)
		}
	}
}

func TestDeriveRunIdentifiers(t *testing.T) {
	t.Parallel()

	identifiers, err := DeriveRunIdentifiers(strings.ToLower(testRunID))
	if err != nil {
		t.Fatal(err)
	}
	if identifiers.Database != "run_01arz3ndektsv4rrffq69g5fav" {
		t.Errorf("database = %q", identifiers.Database)
	}
}

func TestDeriveRunIdentifiersRejectsUnsafeInput(t *testing.T) {
	t.Parallel()

	for _, id := range []string{"", "01ARZ3NDEKTSV4RRFFQ69G5FA", "81ARZ3NDEKTSV4RRFFQ69G5FAV"} {
		if _, err := DeriveRunIdentifiers(id); err == nil {
			t.Errorf("DeriveRunIdentifiers(%q) succeeded", id)
		}
	}
}

func TestQuoteIdentifier(t *testing.T) {
	t.Parallel()

	quoted, err := QuoteIdentifier("run_01k4z6y7")
	if err != nil {
		t.Fatal(err)
	}
	if quoted != "`run_01k4z6y7`" {
		t.Fatalf("quoted identifier = %q", quoted)
	}
	for _, identifier := range []string{"", "UPPER", "run-01", "run_x`;DROP TABLE orders;--"} {
		if _, err := QuoteIdentifier(identifier); err == nil {
			t.Errorf("QuoteIdentifier(%q) succeeded", identifier)
		}
	}
}
