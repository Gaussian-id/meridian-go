// Package idemkey holds the one rule for a valid Idempotency-Key, shared by the root package (a
// caller's ChatParams.IdempotencyKey) and package forward (a browser's header), so the two
// cannot drift apart.
package idemkey

// The engine accepts 1–255 bytes, each visible ASCII: 0x21 ('!') to 0x7E ('~'), so no space, tab
// or control character and nothing outside ASCII (meridian-domain/src/ids.rs).
const (
	maxLen = 255
	first  = '!'
	last   = '~'
)

// Valid reports whether k is an idempotency key the engine accepts.
func Valid(k string) bool {
	if len(k) < 1 || len(k) > maxLen {
		return false
	}

	for i := range len(k) {
		if k[i] < first || k[i] > last {
			return false
		}
	}

	return true
}
