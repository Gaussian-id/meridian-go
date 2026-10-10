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
go vet -tags contract ./... # the contract suite builds, though only scripts/contract.sh runs it
if [ ! -x "$bin/staticcheck" ] && [ ! -x "$bin/staticcheck.exe" ]; then
  GOBIN="$bin" go install honnef.co/go/tools/cmd/staticcheck@v0.8.1
fi
"$bin/staticcheck" ./...
"$bin/staticcheck" -tags contract ./internal/contract/

# [3] Tests, with the race detector when this machine can build it (it needs cgo).
if go test -race -count=1 -run '^$' ./... >/dev/null 2>&1; then race=-race; else
  echo "warning: race detector unavailable here (needs cgo); CI must run it" >&2; race=; fi
go test $race -count=1 ./...

# [4] Generated types must match the pinned documents: no changed, deleted or untracked file.
bash scripts/generate.sh
drift=$(git status --porcelain -- internal/gen)
if [ -n "$drift" ]; then
  echo "internal/gen drifted from spec/; commit the regenerated files:" >&2; echo "$drift" >&2; exit 1
fi

# [5] The module uses the standard library only, tests included. Each non-standard package is
# reduced to its module path, and only this module's own path (matched whole, not as a prefix) is
# allowed; a package with no module is shown by its import path.
module=$(go list -m)
third=$(go list -tags contract -deps -test -f '{{if not .Standard}}{{with .Module}}{{.Path}}{{else}}{{.ImportPath}}{{end}}{{end}}' ./... | sort -u | grep -vxF "$module" || true)
if [ -n "$third" ]; then echo "third-party imports: $third" >&2; exit 1; fi

echo "gate: all checks passed"
