package sandbox

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/oklog/ulid/v2"
)

var (
	identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// DatasetIdentifiers names the execution database of one Dataset Revision. It is created when the
// Revision is validated and holds the validated schema and seed.
type DatasetIdentifiers struct {
	Database string
}

// DeriveDatasetIdentifiers creates the deterministic database name from a canonical Dataset Revision
// ULID. Application code must call this only with an ID it generated, never with a test case name,
// prompt text, or other model output.
func DeriveDatasetIdentifiers(datasetRevisionID string) (DatasetIdentifiers, error) {
	compact, err := compactULID(datasetRevisionID)
	if err != nil {
		return DatasetIdentifiers{}, err
	}
	identifiers := DatasetIdentifiers{Database: "dataset_" + compact}
	if err := requireValidIdentifiers(map[string]string{"database": identifiers.Database}); err != nil {
		return DatasetIdentifiers{}, err
	}
	return identifiers, nil
}

// compactULID lowercases a canonical ULID after strict parsing. Every character of a valid
// ULID already falls inside [a-z0-9] once lowercased, so no further filtering is needed.
func compactULID(id string) (string, error) {
	canonical := strings.ToUpper(strings.TrimSpace(id))
	parsed, err := ulid.ParseStrict(canonical)
	if err != nil {
		return "", fmt.Errorf("ID must be a 26-character canonical ULID")
	}
	return strings.ToLower(parsed.String()), nil
}

func requireValidIdentifiers(named map[string]string) error {
	for label, identifier := range named {
		if !identifierPattern.MatchString(identifier) {
			return fmt.Errorf("derived %s identifier %q is invalid", label, identifier)
		}
	}
	return nil
}

// QuoteIdentifier quotes an application-derived MySQL identifier.
func QuoteIdentifier(identifier string) (string, error) {
	if !identifierPattern.MatchString(identifier) {
		return "", fmt.Errorf("unsafe MySQL identifier %q", identifier)
	}
	return "`" + identifier + "`", nil
}
