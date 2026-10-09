package meridian_test

import (
	"regexp"
	"testing"

	"meridian.local/meridian-go"
)

func TestEngineSpecCommitIsRecorded(t *testing.T) {
	if got := meridian.EngineSpecCommit(); !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(got) {
		t.Fatalf("EngineSpecCommit() = %q, want 12 hex characters", got)
	}
}
