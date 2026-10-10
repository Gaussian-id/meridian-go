#!/usr/bin/env bash
# The contract run: the SDK against the real local engine, in simulator mode only. The guard
# (internal/contractguard) decides first and refuses, by name only, unless the engine is proven to
# be the provider simulator started from this runtime's env file, with no paid key, loopback URLs
# and no proxy. Run from anywhere: bash scripts/contract.sh
set -euo pipefail
cd "$(dirname "$0")/.."

# Offline: no module proxy, checksum database, toolchain download or workspace; go.mod is never
# rewritten. No proxy for the run itself.
export GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOWORK=off GOFLAGS=-mod=readonly NO_PROXY='*' no_proxy='*'
exec go run ./internal/cmd/contract
