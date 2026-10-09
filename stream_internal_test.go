package meridian

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type closeSpy struct {
	io.Reader
	closed bool
}

func (c *closeSpy) Close() error { c.closed = true; return nil }

func spyStream(body string) (*ChatStream, *closeSpy) {
	spy := &closeSpy{Reader: strings.NewReader(body)}

	return newChatStream(context.Background(), &http.Response{Header: http.Header{}, Body: spy}), spy
}

func TestAnOversizedEventFailsClosedAndIsNeverRelayed(t *testing.T) {
	old := maxEventBytes
	maxEventBytes = 64

	defer func() { maxEventBytes = old }()

	big := "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("x", 200) + "\"}}]}\n\n"

	// One long line, and many short data lines that add up past the cap.
	many := strings.Repeat("data: 0123456789\n", 10) + "\n"

	for _, wire := range []string{big, many} {
		s, spy := spyStream(wire)

		_, err := s.Text()
		if e, _ := err.(*Error); e == nil || e.Code != CodeNetworkError || !strings.Contains(e.Message, "larger") {
			t.Fatalf("Chunks: got %v", err)
		}

		if !spy.closed {
			t.Fatal("the upstream must be closed")
		}

		s2, _ := spyStream(wire)
		w := httptest.NewRecorder()

		if e, _ := s2.ServeSSE(w, nil).(*Error); e == nil || e.Code != CodeNetworkError {
			t.Fatalf("ServeSSE: got %v", e)
		}

		if w.Body.Len() != 0 {
			t.Fatalf("nothing may be relayed, got %q", w.Body.String())
		}
	}
}
