package forward

import (
	"net/http"
	"testing"

	"github.com/Gaussian-id/meridian-go"
)

// The handler never passes nil today; if a future path does, it must fail closed.
func TestBrowserErrorFailsClosedOnNil(t *testing.T) {
	g := browserError(nil)
	if g == nil || g.Code != meridian.CodeServiceUnavailable || g.Status != http.StatusServiceUnavailable || g.Retryable {
		t.Fatalf("got %+v", g)
	}
}

// The ServeSSE hook relays the engine's own frame only when browserError hands back the very
// same error; a copy would re-encode it.
func TestBrowserErrorReturnsAPassThroughErrorItself(t *testing.T) {
	e := &meridian.Error{Code: meridian.CodeRateLimited, Type: meridian.TypeRateLimit, Status: 429}
	if browserError(e) != e {
		t.Fatal("a pass-through error must be returned as is")
	}
}
