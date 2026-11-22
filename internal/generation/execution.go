package generation

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrInvalidExecutionRecord reports a stored Dataset execution record that does not describe one Dataset
// Revision on its Targets.
var ErrInvalidExecutionRecord = errors.New("Dataset execution record is invalid")

// DatasetExecutionRecord is the execution record a DATASET stage stores once every requested Target has
// loaded and checked the Dataset Revision (docs/architecture.md section 16.5). It names the revision, and for
// each Target, in the order the request gave, the execution schema created there. Publication reads it to
// write the Dataset Revision and its Target bindings.
type DatasetExecutionRecord struct {
	DatasetRevisionID string                   `json:"dataset_revision_id"`
	Targets           []DatasetExecutionTarget `json:"targets"`
}

// DatasetExecutionTarget is one Target's execution schema for a Dataset Revision.
type DatasetExecutionTarget struct {
	ValidationDBID string `json:"validation_db_id"`
	Database       string `json:"database"`
}

// DecodeDatasetExecutionRecord decodes a stored execution record and checks its structure: the revision ID is
// a ULID, at least one Target and at most MaxRequestTargets is listed, every Target ID is a UUID, and no
// Target appears twice. The database names are checked by the pipeline, which knows how they are derived.
func DecodeDatasetExecutionRecord(content json.RawMessage) (DatasetExecutionRecord, error) {
	var record DatasetExecutionRecord
	if err := json.Unmarshal(content, &record); err != nil {
		return DatasetExecutionRecord{}, fmt.Errorf("%w: %v", ErrInvalidExecutionRecord, err)
	}
	if !requestULIDPattern.MatchString(record.DatasetRevisionID) {
		return DatasetExecutionRecord{}, fmt.Errorf("%w: the revision ID is not a ULID", ErrInvalidExecutionRecord)
	}
	if len(record.Targets) < 1 || len(record.Targets) > MaxRequestTargets {
		return DatasetExecutionRecord{}, fmt.Errorf("%w: the record lists %d Targets", ErrInvalidExecutionRecord, len(record.Targets))
	}
	seen := make(map[string]struct{}, len(record.Targets))
	for _, target := range record.Targets {
		if !targetIDPattern.MatchString(target.ValidationDBID) || target.Database == "" {
			return DatasetExecutionRecord{}, fmt.Errorf("%w: a Target entry is incomplete", ErrInvalidExecutionRecord)
		}
		if _, repeated := seen[target.ValidationDBID]; repeated {
			return DatasetExecutionRecord{}, fmt.Errorf("%w: a Target is listed twice", ErrInvalidExecutionRecord)
		}
		seen[target.ValidationDBID] = struct{}{}
	}
	return record, nil
}
