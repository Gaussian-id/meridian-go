package contractguard

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// simCase runs the whole guard over a clean runtime with a docker the test has edited.
func simCase(t *testing.T, edit func(environ Environ, d *fakeDocker) Environ) (string, *fakeDocker) {
	t.Helper()

	environ, _ := runtimeWith(t, "")
	d := proven(expectedEnvFile(environ))

	if edit != nil {
		environ = edit(environ, d)
	}

	return Refusal(environ, System{Registry: noRegistry, Docker: d.run}), d
}

func TestASimulatorEngineStartedFromThisRuntimeIsAllowed(t *testing.T) {
	message, d := simCase(t, nil)
	allowed(t, message)

	// Every inspect is formatted; nothing asks for logs, the raw environment or a bare inspect.
	for _, call := range d.calls {
		joined := strings.Join(call, " ")
		if call[0] == "inspect" && (!slices.Contains(call, "--format") || len(call) != 4) {
			t.Fatalf("unformatted inspect: %q", joined)
		}

		if call[0] == "logs" || call[0] == "exec" || strings.Contains(joined, "{{.Config.Env}}") ||
			strings.Contains(joined, "json .") {
			t.Fatalf("call %q could print a value", joined)
		}
	}

	if len(d.calls) != 6 {
		t.Fatalf("calls: %q", d.calls)
	}
}

func TestTheTemplatesCompareInsideDockerAndPrintOnlyTokens(t *testing.T) {
	for _, entry := range []string{
		`{{if eq . "GAUSSMERIDIAN_GEMINI_BASE_URL=http://provider-sim:8184/v1beta"}}gemini_base {{end}}`,
		`{{if eq . "GAUSSMERIDIAN_OPENROUTER_BASE_URL=http://provider-sim:8184/api/v1"}}openrouter_base {{end}}`,
		`{{if eq . "GAUSSMERIDIAN_PROVIDER_SIMULATOR=true"}}sim_flag {{end}}`,
		`{{if eq . "GAUSSMERIDIAN_GEMINI_API_KEY=sim-local-key"}}gemini_dummy {{end}}`,
		`{{if eq . "GAUSSMERIDIAN_OPENROUTER_API_KEY=sim-local-key"}}openrouter_dummy {{end}}`,
	} {
		if !strings.Contains(SimTemplate, entry) {
			t.Errorf("template lacks %s", entry)
		}
	}

	if !strings.HasPrefix(SimTemplate, "{{range .Config.Env}}") || !strings.HasSuffix(SimTemplate, "{{end}}") ||
		strings.Count(SimTemplate, "{{if") != 5 || strings.Contains(SimTemplate, "{{.}}") {
		t.Fatalf("template %s", SimTemplate)
	}

	if EnvFileTemplate != `{{index .Config.Labels "com.docker.compose.project.environment_file"}}` {
		t.Fatalf("label template %s", EnvFileTemplate)
	}
}

func TestEachMissingProofRefuses(t *testing.T) {
	cases := map[string]struct {
		edit func(Environ, *fakeDocker) Environ
		want string
	}{
		"docker cannot be asked (ps)": {func(e Environ, d *fakeDocker) Environ { d.broken = "ps"; return e },
			"docker cannot be asked"},
		"docker cannot be asked (port)": {func(e Environ, d *fakeDocker) Environ { d.broken = "port"; return e },
			"docker cannot be asked"},
		"docker cannot be asked (inspect)": {func(e Environ, d *fakeDocker) Environ { d.broken = "inspect"; return e },
			"docker cannot be asked"},
		"no simulator": {func(e Environ, d *fakeDocker) Environ { d.providers = ""; return e },
			"provider-sim is not running exactly once"},
		"two simulators": {func(e Environ, d *fakeDocker) Environ { d.providers = "aaaaaaaaaaaa\nbbbbbbbbbbbb\n"; return e },
			"provider-sim is not running exactly once"},
		"a simulator id that is not an id": {func(e Environ, d *fakeDocker) Environ { d.providers = "Error: nope\n"; return e },
			"provider-sim is not running exactly once"},
		"no engine": {func(e Environ, d *fakeDocker) Environ { d.engines = "\n"; return e },
			"not exactly one engine container"},
		"two engines": {func(e Environ, d *fakeDocker) Environ { d.engines = engineID + "\ncccccccccccc\n"; return e },
			"not exactly one engine container"},
		"the public port on every address": {func(e Environ, d *fakeDocker) Environ {
			d.ports["8180/tcp"] = "0.0.0.0:8180\n"
			return e
		}, "does not own the public and admin ports"},
		"the admin port twice": {func(e Environ, d *fakeDocker) Environ {
			d.ports["8181/tcp"] = "127.0.0.1:8181\n[::1]:8181\n"
			return e
		}, "does not own the public and admin ports"},
		"the admin port not published": {func(e Environ, d *fakeDocker) Environ { delete(d.ports, "8181/tcp"); return e },
			"does not own the public and admin ports"},
		"the URL names another port": {func(e Environ, _ *fakeDocker) Environ {
			return with(e, "MERIDIAN_PUBLIC_URL", "http://127.0.0.1:9999")
		}, "does not own the public and admin ports"},
		"a real Gemini base URL": {func(e Environ, d *fakeDocker) Environ {
			d.env = "openrouter_base sim_flag gemini_dummy openrouter_dummy "
			return e
		}, "missing: gemini_base"},
		"real keys and no simulator flag": {func(e Environ, d *fakeDocker) Environ {
			d.env = "gemini_base openrouter_base "
			return e
		}, "missing: sim_flag, gemini_dummy, openrouter_dummy"},
		"a token twice": {func(e Environ, d *fakeDocker) Environ { d.env += "sim_flag "; return e },
			"not set up for the simulator"},
		"the label names another env file": {func(e Environ, d *fakeDocker) Environ {
			d.label = filepath.Join(filepath.Dir(RuntimeDir(e)), "elsewhere.env") + "\n"
			return e
		}, "not started from the env file of this runtime"},
		"the label names two env files": {func(e Environ, d *fakeDocker) Environ {
			d.label = expectedEnvFile(e) + "," + expectedEnvFile(e) + "\n"
			return e
		}, "not started from the env file of this runtime"},
		"no label and no marker": {func(e Environ, d *fakeDocker) Environ { d.label = "\n"; return e },
			"not started from the env file of this runtime"},
		"a project name that is not plain": {func(e Environ, _ *fakeDocker) Environ {
			return with(e, "GAUSSMERIDIAN_LOCAL_PROJECT", "x --filter")
		}, "GAUSSMERIDIAN_LOCAL_PROJECT"},
		"another project": {func(e Environ, _ *fakeDocker) Environ {
			return with(e, "GAUSSMERIDIAN_LOCAL_PROJECT", "meridian-sim")
		}, "provider-sim is not running exactly once"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			message, _ := simCase(t, c.edit)
			refused(t, message, c.want)
		})
	}
}

func TestNoDockerIsARefusal(t *testing.T) {
	environ, _ := runtimeWith(t, "")
	refused(t, Refusal(environ, System{Registry: noRegistry}), "docker cannot be asked")
}

// A real-mode engine, as docker would describe it: no simulator, real base URLs (so no token),
// and the label pointing at the owner's real env file. It is refused at the first check.
func TestARealModeEngineIsRefused(t *testing.T) {
	message, _ := simCase(t, func(e Environ, d *fakeDocker) Environ {
		d.providers = ""
		d.env = ""
		d.label = filepath.Join("C:", "Users", "someone", "AppData", "Local", "Gaussian", "MeridianEngine", envFileName)
		return e
	})
	refused(t, message, "provider-sim")

	// Even with a stray simulator container running beside it, the engine's settings refuse.
	message, _ = simCase(t, func(e Environ, d *fakeDocker) Environ {
		d.env = ""
		return e
	})
	refused(t, message, "missing: gemini_base, openrouter_base, sim_flag, gemini_dummy, openrouter_dummy")
}

func TestTheProjectNameComesFromTheEnvironment(t *testing.T) {
	message, d := simCase(t, func(e Environ, d *fakeDocker) Environ {
		d.project = "meridian-sim"
		return with(e, "GAUSSMERIDIAN_LOCAL_PROJECT", "meridian-sim")
	})
	allowed(t, message)

	if !slices.Contains(d.calls[0], "label=com.docker.compose.project=meridian-sim") {
		t.Fatalf("calls %q", d.calls)
	}
}

func TestLocalhostURLsOnTheEnginePortsAreAllowed(t *testing.T) {
	message, _ := simCase(t, func(e Environ, _ *fakeDocker) Environ {
		return with(e, "MERIDIAN_PUBLIC_URL", "http://localhost:8180", "MERIDIAN_ADMIN_URL", "http://127.0.0.1:8181/")
	})
	allowed(t, message)
}

// The label is the only binding: a marker file in the runtime folder does not stand in for it.
func TestWithoutTheLabelNothingElseBindsTheEngine(t *testing.T) {
	environ, folder := runtimeWith(t, "")
	d := proven(expectedEnvFile(environ))
	d.label = "\n"

	writeFile(t, filepath.Join(folder, ".sim-started-"+engineID), engineID+"\n")
	refused(t, Refusal(environ, System{Registry: noRegistry, Docker: d.run}), "not started from the env file of this runtime")
}

func TestTheLabelIsComparedAfterResolvingPaths(t *testing.T) {
	message, _ := simCase(t, func(e Environ, d *fakeDocker) Environ {
		rt := RuntimeDir(e)
		d.label = filepath.Join(rt, "sub", "..", envFileName) + "\r\n"
		return e
	})
	allowed(t, message)
}
