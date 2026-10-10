// Command contract runs the contract suite against the real local engine, in simulator mode only.
// It must never spend money, so the guard (internal/contractguard) decides first: it refuses on a
// paid provider key (process or any env file), a path outside the runtime folder, a non-loopback
// URL, an engine not proven to be the simulator started from this runtime's env file, or a proxy
// (environment or Windows registry). It never prints a value.
//
//	go run ./internal/cmd/contract       (or: bash scripts/contract.sh)
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"meridian.local/meridian-go/internal/contractguard"
)

// command is the suite: only the contract package, built with the contract tag, never cached.
var command = []string{"go", "test", "-tags", "contract", "-count=1", "-v", "./internal/contract/"}

// childSettings are set for the suite on top of this process's environment. The guard found no
// proxy; NO_PROXY=* keeps it so. The go command stays offline: no module proxy or checksum
// database, no toolchain download, no workspace, and go.mod is never rewritten.
var childSettings = [][2]string{
	{"MERIDIAN_CONTRACT", "1"},
	{"NO_PROXY", "*"}, {"no_proxy", "*"},
	{"GOPROXY", "off"}, {"GOSUMDB", "off"}, {"GOTOOLCHAIN", "local"}, {"GOWORK", "off"}, {"GOFLAGS", "-mod=readonly"},
}

// Runner runs argv in dir with env and returns its exit code.
type Runner func(argv, env []string, dir string) int

func runGo(argv, env []string, dir string) int {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env, cmd.Dir = env, dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, os.Stdout, os.Stderr

	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}

		return 1
	}

	return 0
}

func main() {
	dir, err := os.Getwd()
	if err != nil {
		dir = "."
	}

	os.Exit(run(contractguard.ProcessEnviron(), contractguard.Live(), contractguard.EngineReady, runGo, dir, os.Stderr))
}

func run(environ contractguard.Environ, sys contractguard.System, ready func(string) bool, runner Runner,
	dir string, stderr io.Writer,
) int {
	// [1] The guard decides first: nothing is contacted or launched on a refusal.
	if reason := contractguard.Refusal(environ, sys); reason != "" {
		fmt.Fprintf(stderr, "Contract run refused: %s\n", reason)

		return 1
	}

	// [2] The suite runs from the module root.
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		fmt.Fprintln(stderr, "Contract run refused: run it from the module root (go run ./internal/cmd/contract).")

		return 1
	}

	// [3] The engine must be ready.
	public, ok := environ["MERIDIAN_PUBLIC_URL"]
	if !ok {
		public = contractguard.DefaultURLs["MERIDIAN_PUBLIC_URL"]
	}

	if !ready(public) {
		fmt.Fprintln(stderr, "Contract run refused: the local engine is not ready (or did not answer in 5 s).")

		return 1
	}

	// [4] The suite, in contract mode, against the engine.
	folder := environ["MERIDIAN_RUNTIME_DIR"]
	if folder == "" {
		folder = contractguard.RuntimeDir(environ)
	}

	return runner(command, childEnv(environ, folder), dir)
}

// childEnv is environ with the suite's settings; a name it sets replaces any spelling of it.
func childEnv(environ contractguard.Environ, folder string) []string {
	settings := slices.Concat(childSettings, [][2]string{{"MERIDIAN_RUNTIME_DIR", folder}})

	var env []string

	for name, value := range environ {
		replaced := false

		for _, s := range settings {
			if strings.EqualFold(name, s[0]) {
				replaced = true
			}
		}

		if !replaced {
			env = append(env, name+"="+value)
		}
	}

	for _, s := range settings {
		env = append(env, s[0]+"="+s[1])
	}

	return env
}
