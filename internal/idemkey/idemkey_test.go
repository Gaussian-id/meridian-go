package idemkey_test

import (
	"strings"
	"testing"

	"github.com/Gaussian-id/meridian-go/internal/idemkey"
)

func TestValid(t *testing.T) {
	for k, want := range map[string]bool{
		"a-b_c.1": true, "k": true, "!~": true, strings.Repeat("a", 255): true,
		"": false, "order 42": false, " lead": false, "trail ": false, "   ": false, "a\tb": false,
		"a\x7fb": false, "a\x00b": false, "a\nb": false, "é": false, strings.Repeat("a", 256): false,
	} {
		if got := idemkey.Valid(k); got != want {
			t.Errorf("Valid(%q) = %v, want %v", k, got, want)
		}
	}
}
