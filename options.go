package meridian

import (
	"net/http"
	"strings"
	"time"
)

type config struct {
	httpClient    *http.Client
	maxRetries    int
	userAgent     string
	headerTimeout time.Duration
}

// Option configures a client.
type Option func(*config)

// WithHTTPClient sets the *http.Client used for every request (proxies, TLS, tracing, tests).
// A nil client is ignored.
func WithHTTPClient(c *http.Client) Option {
	return func(cfg *config) {
		if c != nil {
			cfg.httpClient = c
		}
	}
}

// WithMaxRetries sets how many times a retryable failure is retried before the body is read.
// Values are clamped to 0–10. The default is 2.
func WithMaxRetries(n int) Option {
	return func(cfg *config) { cfg.maxRetries = min(max(n, 0), 10) }
}

// WithUserAgentSuffix appends s to the User-Agent header.
func WithUserAgentSuffix(s string) Option {
	return func(cfg *config) {
		if s = strings.TrimSpace(s); s != "" {
			cfg.userAgent += " " + s
		}
	}
}

// WithHeaderTimeout bounds each attempt's wait for response headers (default 60s). A value
// that is not positive is ignored. The whole call is bounded only by the context.
func WithHeaderTimeout(d time.Duration) Option {
	return func(cfg *config) {
		if d > 0 {
			cfg.headerTimeout = d
		}
	}
}

func newConfig(opts []Option) config {
	cfg := config{httpClient: http.DefaultClient, maxRetries: 2, userAgent: "meridian-go/" + Version, headerTimeout: 60 * time.Second}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}

	return cfg
}
