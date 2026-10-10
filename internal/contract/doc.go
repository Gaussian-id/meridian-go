// Package contract holds the contract suite: the SDK against the real local engine in simulator
// mode. Its tests build only with the contract tag and run only through the guard:
//
//	go run ./internal/cmd/contract
//
// The normal suite (go test ./...) never builds them. Cases the simulator cannot produce, such as
// an error frame in the middle of a stream, stay in the fake-engine tests of the SDK packages.
package contract
