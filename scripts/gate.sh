#!/usr/bin/env bash
# The quality gate for meridian-go. Run from anywhere: bash scripts/gate.sh
set -euo pipefail
cd "$(dirname "$0")/.."
bin="$PWD/.bin"

# [1] Formatting.
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then echo "gofmt needed: $unformatted" >&2; exit 1; fi

# [2] Vet and static analysis (staticcheck is an approved dev-only tool).
go vet ./...
if [ ! -x "$bin/staticcheck" ] && [ ! -x "$bin/staticcheck.exe" ]; then
  GOBIN="$bin" go install honnef.co/go/tools/cmd/staticcheck@v0.8.1
fi
"$bin/staticcheck" ./...

# [3] Tests, with the race detector when this machine can build it (it needs cgo).
if go test -race -count=1 -run '^$' ./... >/dev/null 2>&1; then race=-race; else
  echo "warning: race detector unavailable here (needs cgo); CI must run it" >&2; race=; fi
go test $race -count=1 ./...

# [4] Generated types must match the pinned documents.
bash scripts/generate.sh
if ! git diff --quiet -- internal/gen; then
  echo "internal/gen drifted from spec/; commit the regenerated files" >&2; git diff --stat -- internal/gen >&2; exit 1
fi

# [5] The module uses the standard library only, tests included.
module=$(go list -m)
third=$(go list -deps -test -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./... | grep -v "^$module" || true)
if [ -n "$third" ]; then echo "third-party imports: $third" >&2; exit 1; fi

echo "gate: all checks passed"
