package meridian

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// hangingTransport never answers until the request is cancelled.
type hangingTransport struct{}

func (hangingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	<-r.Context().Done()

	return nil, r.Context().Err()
}

func TestTheHeaderTimeoutDefaultsToSixtySecondsAndThreeHundredForABufferedCall(t *testing.T) {
	stream := &transport{cfg: newConfig(nil)}
	buffered := call{headerTimeout: bufferedHeaderTimeout}

	if got := stream.headerTimeoutFor(call{}); got != 60*time.Second {
		t.Errorf("a stream or admin call waits %v, want 60s", got)
	}

	if got := stream.headerTimeoutFor(buffered); got != 300*time.Second {
		t.Errorf("a buffered call waits %v, want 300s", got)
	}

	explicit := &transport{cfg: newConfig([]Option{WithHeaderTimeout(7 * time.Second)})}
	for _, c := range []call{{}, buffered} {
		if got := explicit.headerTimeoutFor(c); got != 7*time.Second {
			t.Errorf("an explicit timeout gives %v, want 7s for every call", got)
		}
	}
}

func TestABufferedCallUsesItsOwnLongerDefaultInPractice(t *testing.T) {
	// Lower both defaults so the test needs no real wait: the plain default is 10 ms, the buffered one
	// 200 ms. A buffered call that gave up after the plain default would fail well before 200 ms.
	oldDefault, oldBuffered := defaultHeaderTimeout, bufferedHeaderTimeout
	defaultHeaderTimeout, bufferedHeaderTimeout = 10*time.Millisecond, 200*time.Millisecond

	t.Cleanup(func() { defaultHeaderTimeout, bufferedHeaderTimeout = oldDefault, oldBuffered })

	hc := &http.Client{Transport: hangingTransport{}}
	c := NewClient("http://engine.test", "gmr_"+strings.Repeat("0", 64), WithHTTPClient(hc), WithMaxRetries(0))

	start := time.Now()
	_, err := c.Chat.Create(context.Background(), ChatParams{})

	var me *Error
	if !errors.As(err, &me) || me.Code != CodeNetworkError {
		t.Fatalf("got %v", err)
	}

	if took := time.Since(start); took < 180*time.Millisecond {
		t.Fatalf("a buffered call gave up after %v, before its own 200 ms limit", took)
	}
}
