package generation

import "errors"

// Dataset limits that every Dataset source obeys: an AI-generated Dataset, a Database Profile
// import, and the execution validator (docs/architecture.md section 11.3).
const (
	DatasetMaxTables           = 20
	DatasetMaxRowsPerStatement = 1000
	DatasetMaxTotalRows        = 10000
)

// Errors a Dataset source reports so the Generation Pipeline can map each one to a stable Job
// failure code without learning how the source connects to validation MySQL.
var (
	ErrDatasetSourceUnsupported = errors.New("reference schema uses an unsupported object")
	ErrDatasetSourceLimit       = errors.New("reference schema exceeds platform Dataset limits")
	ErrDatasetSourceUnavailable = errors.New("reference schema is unavailable")
	ErrDatasetSourceConfig      = errors.New("Database Profile is misconfigured")
)

// DatabaseProfileConnection is what a Dataset import needs from a Database Profile. It carries the
// Secret Reference, never the password it names.
type DatabaseProfileConnection struct {
	ID                     string
	ConfigSHA256           string
	Host                   string
	Port                   int
	Database               string
	Username               string
	PasswordSecretRef      string
	TLSMode                string
	CACertificatePEM       string
	SnapshotMode           string
	SnapshotTimeoutSeconds int
}

// DatasetSnapshot is the Dataset SQL read from a reference schema: schema.sql in platform form and
// seed.sql holding the reference rows as literal INSERT statements (empty for SCHEMA_ONLY).
type DatasetSnapshot struct {
	SchemaSQL string
	SeedSQL   string
}
