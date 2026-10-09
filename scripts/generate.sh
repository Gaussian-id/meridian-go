#!/usr/bin/env bash
# Regenerates internal/gen from the pinned engine documents in spec/.
set -euo pipefail
cd "$(dirname "$0")/.."
bin="$PWD/.bin"

# [1] oapi-codegen is an approved dev-only tool, pinned here.
if [ ! -x "$bin/oapi-codegen" ] && [ ! -x "$bin/oapi-codegen.exe" ]; then
  GOBIN="$bin" go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0
fi

# [2] Downconvert each document to the 3.0 subset, then generate types only.
for doc in public admin; do
  go run ./internal/cmd/downconvert "spec/meridian-$doc.json" "internal/gen/$doc/openapi.json"
  "$bin/oapi-codegen" -config "internal/gen/$doc/oapi.yaml" "internal/gen/$doc/openapi.json"
done

gofmt -w internal/gen
