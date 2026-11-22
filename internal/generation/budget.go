package generation

import "time"

// GenerationJobBudget is the processing time one Generation Job may use, counted from its first
// advance past acceptance. Time spent waiting in AWAITING_REVIEW counts too
// (docs/technical/01-storage-and-workflows.md, section 3).
const GenerationJobBudget = 10 * time.Minute

// DraftPurge reports how many expired staging rows one purge pass deleted, per table.
type DraftPurge struct {
	ArtifactDrafts int64
	TestCaseDrafts int64
}
