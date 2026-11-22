package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/generation"

	"github.com/DATA-DOG/go-sqlmock"
)

// testTargetID is the registered validation MySQL the pipeline tests bind Datasets to. It is a lowercase UUID, the
// form the registry issues.
const testTargetID = "0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"

// singleTarget resolves every validation MySQL ID to one Target, for tests whose Dataset is bound to a single
// Target. The fake or live connection it wraps is the only Target the stage can reach.
type singleTarget struct {
	target ValidationDB
}

func (resolver singleTarget) ValidationDB(_ context.Context, _ string) (ValidationDB, error) {
	return resolver.target, nil
}

// secondTestTargetID is a second registered validation MySQL, for tests whose Dataset is bound to two Targets.
const secondTestTargetID = "5e8c1b2a-7f3d-4c6e-8a9b-0d1e2f3a4b5c"

// targetsByID resolves validation MySQL IDs to fakes. An ID that is not listed is an unregistered Target.
type targetsByID map[string]ValidationDB

func (resolver targetsByID) ValidationDB(_ context.Context, targetID string) (ValidationDB, error) {
	target, found := resolver[targetID]
	if !found {
		return nil, fmt.Errorf("validation MySQL %s is not registered", targetID)
	}
	return target, nil
}

// normalizedArtifactForTargets is the normalized request of normalizedArtifact, bound to the given validation MySQL
// IDs in order, with its digest recomputed.
func normalizedArtifactForTargets(t *testing.T, targetIDs ...string) generation.ArtifactSnapshot {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(normalizedArtifact(t).ContentJSON, &document); err != nil {
		t.Fatal(err)
	}
	document["validation_db_ids"] = targetIDs
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	canonical, digest, err := generation.CanonicalJSON(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return generation.ArtifactSnapshot{Kind: generation.ArtifactKindNormalizedRequest, Key: normalizedRequestKey, ContentJSON: canonical, SHA256: digest}
}

// newDatasetLoaderMock returns a Target connection that accepts the validating Dataset fixture: its schema, its
// seed, and a constraint query that finds no violation.
func newDatasetLoaderMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	loader, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(regexp.QuoteMeta(validatingDatasetSchemaSQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(validatingDatasetSeedSQL)).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(regexp.QuoteMeta(validatingDatasetConstraintQuery)).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	return loader, mock
}

func newProcessorWithTargets(t *testing.T, store Store, resolver TargetResolver) *Processor {
	t.Helper()
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	processor, err := New(store, validator, WithValidationDB(resolver))
	if err != nil {
		t.Fatal(err)
	}
	return processor
}

func validateDatasetStage() generation.StageRequest {
	return generation.StageRequest{JobID: pipelineJobID, Stage: generation.StatusValidatingDataset, Lease: validLease()}
}

// A Dataset bound to two Targets is loaded on each in request order, and its execution record names both Targets
// with the one execution schema.
func TestValidateDatasetLoadsOnEveryBoundTargetInRequestOrder(t *testing.T) {
	t.Parallel()

	firstLoader, firstMock := newDatasetLoaderMock(t)
	secondLoader, secondMock := newDatasetLoaderMock(t)
	store := datasetValidationFakeStore(t)
	store.artifactsByKind[generation.ArtifactKindNormalizedRequest] = normalizedArtifactForTargets(t, testTargetID, secondTestTargetID)
	processor := newProcessorWithTargets(t, store, targetsByID{
		testTargetID:       &fakeValidationDB{loader: firstLoader},
		secondTestTargetID: &fakeValidationDB{loader: secondLoader},
	})

	result, err := processor.Process(context.Background(), validateDatasetStage())
	if err != nil {
		t.Fatal(err)
	}
	if result.Failure != nil || result.Transition.Event != generation.EventValidationAccepted {
		t.Fatalf("result = %#v", result)
	}
	if err := firstMock.ExpectationsWereMet(); err != nil {
		t.Fatalf("first Target: unmet SQL expectations: %v", err)
	}
	if err := secondMock.ExpectationsWereMet(); err != nil {
		t.Fatalf("second Target: unmet SQL expectations: %v", err)
	}
	if store.saved == nil || store.saved.Kind != generation.ArtifactKindDatasetExecution {
		t.Fatalf("execution artifact = %#v", store.saved)
	}
	record, err := generation.DecodeDatasetExecutionRecord(store.saved.ContentJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Targets) != 2 || record.Targets[0].ValidationDBID != testTargetID || record.Targets[1].ValidationDBID != secondTestTargetID {
		t.Fatalf("execution Targets = %#v, want %s then %s", record.Targets, testTargetID, secondTestTargetID)
	}
	database := "dataset_" + strings.ToLower(record.DatasetRevisionID)
	for _, target := range record.Targets {
		if target.Database != database {
			t.Fatalf("Target %s database = %q, want %q", target.ValidationDBID, target.Database, database)
		}
	}
}

// When a later Target cannot provision the execution schema, the attempt ends there: no execution record is saved,
// and the schema loaded on the earlier Target is kept for manual deletion.
func TestValidateDatasetStopsAtFailingTarget(t *testing.T) {
	t.Parallel()

	firstLoader, firstMock := newDatasetLoaderMock(t)
	store := datasetValidationFakeStore(t)
	store.artifactsByKind[generation.ArtifactKindNormalizedRequest] = normalizedArtifactForTargets(t, testTargetID, secondTestTargetID)
	processor := newProcessorWithTargets(t, store, targetsByID{
		testTargetID:       &fakeValidationDB{loader: firstLoader},
		secondTestTargetID: &fakeValidationDB{execErr: errors.New("target mysql unreachable")},
	})

	result, err := processor.Process(context.Background(), validateDatasetStage())
	if err != nil || result.Failure == nil || result.Failure.Code != "VALIDATION_DB_UNAVAILABLE" {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if store.saved != nil {
		t.Fatalf("execution artifact saved for a partly loaded Dataset: %#v", store.saved)
	}
	if err := firstMock.ExpectationsWereMet(); err != nil {
		t.Fatalf("first Target was not fully loaded: %v", err)
	}
}

// A Target that the request names but the registry does not hold is a configuration failure, and it spends no
// repair round.
func TestValidateDatasetRejectsUnregisteredTarget(t *testing.T) {
	t.Parallel()

	store := datasetValidationFakeStore(t)
	processor := newProcessorWithTargets(t, store, targetsByID{})

	result, err := processor.Process(context.Background(), validateDatasetStage())
	if err != nil || result.Failure == nil || result.Failure.Code != "VALIDATION_DB_CONFIGURATION_INVALID" {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if result.Transition.Event == generation.EventValidationRejected {
		t.Fatal("an unregistered Target spent a repair round")
	}
}

// A stored execution record is reused only when it lists the request's Targets in order, with the execution schema
// derived from its revision ID.
func TestStoredDatasetExecutionMustMatchRequest(t *testing.T) {
	t.Parallel()

	const revisionID = "01JTESTDATASETREV000000000"
	const database = "dataset_01jtestdatasetrev000000000"
	record := func(targets ...generation.DatasetExecutionTarget) json.RawMessage {
		t.Helper()
		content, err := json.Marshal(generation.DatasetExecutionRecord{DatasetRevisionID: revisionID, Targets: targets})
		if err != nil {
			t.Fatal(err)
		}
		return content
	}
	onTarget := func(targetID, databaseName string) generation.DatasetExecutionTarget {
		return generation.DatasetExecutionTarget{ValidationDBID: targetID, Database: databaseName}
	}
	request := []string{testTargetID, secondTestTargetID}

	cases := []struct {
		name    string
		content json.RawMessage
		wantErr bool
	}{
		{
			name:    "matching order and database",
			content: record(onTarget(testTargetID, database), onTarget(secondTestTargetID, database)),
		},
		{
			name:    "reversed order",
			content: record(onTarget(secondTestTargetID, database), onTarget(testTargetID, database)),
			wantErr: true,
		},
		{
			name:    "database not derived from the revision",
			content: record(onTarget(testTargetID, "dataset_other"), onTarget(secondTestTargetID, "dataset_other")),
			wantErr: true,
		},
		{
			name:    "fewer Targets than the request names",
			content: record(onTarget(testTargetID, database)),
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := storedDatasetExecution(tc.content, request)
			if (err != nil) != tc.wantErr {
				t.Fatalf("storedDatasetExecution error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}
