package meridian_test

import (
	"os"
	"strings"
	"testing"
)

// The module promises a Go 1.23 floor and no third-party runtime dependencies.
func TestModuleFloorAndNoRequirements(t *testing.T) {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}

	sawFloor := false

	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		switch fields[0] {
		case "go":
			sawFloor = len(fields) == 2 && fields[1] == "1.23"
		case "require", "tool", "toolchain":
			t.Errorf("go.mod must not declare %q: %s", fields[0], line)
		}
	}

	if !sawFloor {
		t.Errorf("go.mod must say `go 1.23`")
	}
}
