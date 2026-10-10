package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"meridian.local/meridian-go/internal/contractguard"
)

// simulator answers the guard's docker calls as a simulator engine started from envFile would. It
// never runs docker.
func simulator(envFile string) contractguard.Docker {
	return func(args []string) (string, error) {
		joined := strings.Join(args, " ")

		switch {
		case args[0] == "ps" && strings.Contains(joined, "service=provider-sim "):
			return "fedcba987654\n", nil
		case args[0] == "ps" && strings.Contains(joined, "service=engine "):
			return "0123456789ab\n", nil
		case args[0] == "port":
			return "127.0.0.1:" + strings.TrimSuffix(args[2], "/tcp") + "\n", nil
		case args[0] == "inspect" && args[3] == contractguard.SimTemplate:
			return "gemini_base openrouter_base sim_flag gemini_dummy openrouter_dummy " +
				"openai_dummy anthropic_dummy xai_dummy deepseek_dummy mistral_dummy qwen_dummy ", nil
		case args[0] == "inspect" && args[3] == contractguard.EnvFileTemplate:
			return envFile + "\n", nil
		}

		return "", errors.New("unexpected docker call")
	}
}

type recorded struct {
	argv, env []string
	dir       string
	calls     int
}

func (r *recorded) run(argv, env []string, dir string) int {
	r.argv, r.env, r.dir = argv, env, dir
	r.calls++

	return 7
}

// setup is a clean runtime under a fresh LOCALAPPDATA, a module root and a simulator docker.
func setup(t *testing.T, envText string) (contractguard.Environ, contractguard.System, string) {
	t.Helper()

	root := t.TempDir()
	folder := filepath.Join(root, "Gaussian", "MeridianEngine")
	envFile := filepath.Join(folder, "meridian-engine.env")
	module := t.TempDir()

	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}

	for path, text := range map[string]string{envFile: envText, filepath.Join(module, "go.mod"): "module x\n"} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	none := func() (map[string]string, error) { return map[string]string{}, nil }

	return contractguard.Environ{"LOCALAPPDATA": root, "PATH": "/bin"}, contractguard.System{Registry: none, Docker: simulator(envFile)}, module
}

func TestARefusalLaunchesNothingAndContactsNothing(t *testing.T) {
	environ, sys, module := setup(t, contractguard.Gemini+"=v-not-printed\n")
	runner := &recorded{}
	asked := false

	var stderr bytes.Buffer

	code := run(environ, sys, func(string) bool { asked = true; return true }, runner.run, module, &stderr)
	if code != 1 || asked || runner.calls != 0 {
		t.Fatalf("code %d, ready asked %v, runner calls %d", code, asked, runner.calls)
	}

	if !strings.Contains(stderr.String(), "Contract run refused: "+contractguard.Gemini) ||
		strings.Contains(stderr.String(), "v-not-printed") {
		t.Fatalf("stderr %q", stderr.String())
	}
}

func TestAnEngineNotProvenToBeTheSimulatorIsRefused(t *testing.T) {
	environ, sys, module := setup(t, "A=1\n")
	sys.Docker = func([]string) (string, error) { return "", errors.New("docker: not found") }
	runner := &recorded{}

	var stderr bytes.Buffer

	if code := run(environ, sys, func(string) bool { return true }, runner.run, module, &stderr); code != 1 ||
		runner.calls != 0 || !strings.Contains(stderr.String(), "docker cannot be asked") {
		t.Fatalf("code %d, calls %d, stderr %q", code, runner.calls, stderr.String())
	}
}

func TestAnEngineThatIsNotReadyIsRefused(t *testing.T) {
	environ, sys, module := setup(t, "A=1\n")
	runner := &recorded{}

	var (
		stderr bytes.Buffer
		asked  string
	)

	code := run(environ, sys, func(u string) bool { asked = u; return false }, runner.run, module, &stderr)
	if code != 1 || runner.calls != 0 || asked != "http://127.0.0.1:8180" || !strings.Contains(stderr.String(), "not ready") {
		t.Fatalf("code %d, calls %d, asked %q, stderr %q", code, runner.calls, asked, stderr.String())
	}
}

func TestOutsideTheModuleRootNothingRuns(t *testing.T) {
	environ, sys, _ := setup(t, "A=1\n")
	runner := &recorded{}

	var stderr bytes.Buffer

	if code := run(environ, sys, func(string) bool { return true }, runner.run, t.TempDir(), &stderr); code != 1 ||
		runner.calls != 0 {
		t.Fatalf("code %d, calls %d", code, runner.calls)
	}
}

func TestAnAllowedRunLaunchesTheContractSuiteOfflineAndWithoutAProxy(t *testing.T) {
	environ, sys, module := setup(t, "A=1\n")
	environ["GOFLAGS"] = "-mod=mod"
	environ["goproxy"] = "https://proxy.example"
	runner := &recorded{}

	var stderr bytes.Buffer

	if code := run(environ, sys, func(string) bool { return true }, runner.run, module, &stderr); code != 7 {
		t.Fatalf("code %d, stderr %q", code, stderr.String())
	}

	if !slices.Equal(runner.argv, []string{"go", "test", "-tags", "contract", "-count=1", "-v", "./internal/contract/"}) ||
		runner.dir != module {
		t.Fatalf("argv %q in %q", runner.argv, runner.dir)
	}

	for _, want := range []string{
		"MERIDIAN_CONTRACT=1", "NO_PROXY=*", "no_proxy=*", "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local",
		"GOWORK=off", "GOFLAGS=-mod=readonly", "PATH=/bin", "LOCALAPPDATA=" + environ["LOCALAPPDATA"],
		"MERIDIAN_RUNTIME_DIR=" + filepath.Join(environ["LOCALAPPDATA"], "Gaussian", "MeridianEngine"),
	} {
		if !slices.Contains(runner.env, want) {
			t.Errorf("env lacks %q", want)
		}
	}

	for _, unwanted := range []string{"GOFLAGS=-mod=mod", "goproxy=https://proxy.example"} {
		if slices.Contains(runner.env, unwanted) {
			t.Errorf("env keeps %q", unwanted)
		}
	}
}
