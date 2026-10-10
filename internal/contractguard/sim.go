package contractguard

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Docker runs docker with a fixed argv and no shell, and returns its standard output. Its output
// is read by the guard and never printed.
type Docker func(args []string) (string, error)

const dockerTimeout = 20 * time.Second

// RunDocker runs the docker CLI. Standard error is dropped unread: it could echo a value.
func RunDocker(args []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()

	var out bytes.Buffer

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = &out, io.Discard, nil

	if err := cmd.Run(); err != nil {
		return "", err
	}

	return out.String(), nil
}

const defaultProject = "meridian-engine"

var (
	projectName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	containerID = regexp.MustCompile(`^[0-9a-f]{12,64}$`)
)

// simEnv are the only environment entries the proof may compare, each by exact equality inside the
// docker template, so the output is a token and never a value. Every paid key is compared
// with the public dummy of the engine's deploy/compose.simulator.yaml (which sets all eight) and nothing else.
var simEnv = [][2]string{
	{"gemini_base", "GAUSSMERIDIAN_GEMINI_BASE_URL=http://provider-sim:8184/v1beta"},
	{"openrouter_base", "GAUSSMERIDIAN_OPENROUTER_BASE_URL=http://provider-sim:8184/api/v1"},
	{"sim_flag", "GAUSSMERIDIAN_PROVIDER_SIMULATOR=true"},
	{"gemini_dummy", "GAUSSMERIDIAN_GEMINI_API_KEY=sim-local-key"},
	{"openrouter_dummy", "GAUSSMERIDIAN_OPENROUTER_API_KEY=sim-local-key"},
	{"openai_dummy", "GAUSSMERIDIAN_OPENAI_API_KEY=sim-local-key"},
	{"anthropic_dummy", "GAUSSMERIDIAN_ANTHROPIC_API_KEY=sim-local-key"},
	{"xai_dummy", "GAUSSMERIDIAN_XAI_API_KEY=sim-local-key"},
	{"deepseek_dummy", "GAUSSMERIDIAN_DEEPSEEK_API_KEY=sim-local-key"},
	{"mistral_dummy", "GAUSSMERIDIAN_MISTRAL_API_KEY=sim-local-key"},
	{"qwen_dummy", "GAUSSMERIDIAN_QWEN_API_KEY=sim-local-key"},
}

// SimTemplate prints one token per allowlisted setting whose exact text the engine container has.
var SimTemplate = func() string {
	var b strings.Builder

	b.WriteString("{{range .Config.Env}}")

	for _, e := range simEnv {
		b.WriteString(`{{if eq . "` + e[1] + `"}}` + e[0] + ` {{end}}`)
	}

	b.WriteString("{{end}}")

	return b.String()
}()

// EnvFileTemplate prints the compose label naming the env file the project was started with.
const EnvFileTemplate = `{{index .Config.Labels "com.docker.compose.project.environment_file"}}`

const unasked = "the simulator cannot be proven: docker cannot be asked."

func ask(docker Docker, args ...string) (string, bool) {
	if docker == nil {
		return "", false
	}

	out, err := docker(args)

	return out, err == nil
}

func nonBlankLines(text string) []string {
	var lines []string

	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}

	return lines
}

// urlPort is the port a URL names, or its scheme's default.
func urlPort(value string) (string, bool) {
	u, err := url.Parse(value)
	if err != nil {
		return "", false
	}

	if p := u.Port(); p != "" {
		return p, true
	}

	if u.Scheme == "https" {
		return "443", true
	}

	return "80", true
}

// SimulatorProof says why the engine on this machine is not proven to be in simulator mode and
// started from this runtime's env file, or "" when it is:
//
//	S1 provider-sim runs, exactly once, in the compose project;
//	S2 exactly one engine container runs, and it alone owns the public and admin ports on 127.0.0.1;
//	S3 its provider settings are the simulator's (exact comparisons inside the docker template,
//	   tokens out);
//	S4 it was started from the env file of this runtime: the compose label
//	   com.docker.compose.project.environment_file names exactly that file (after resolving
//	   paths); a missing label refuses.
//
// Any error counts as a refusal. Names only.
func SimulatorProof(env Environ, rt string, docker Docker) string {
	project := env["GAUSSMERIDIAN_LOCAL_PROJECT"]
	if project == "" {
		project = defaultProject
	}

	if !projectName.MatchString(project) {
		return "GAUSSMERIDIAN_LOCAL_PROJECT is not a plain project name."
	}

	running := func(service string) ([]string, bool) {
		out, ok := ask(docker, "ps", "-q", "--filter", "label=com.docker.compose.project="+project,
			"--filter", "label=com.docker.compose.service="+service, "--filter", "status=running")

		return nonBlankLines(out), ok
	}

	// [S1]
	providers, ok := running("provider-sim")
	if !ok {
		return unasked
	}

	if len(providers) != 1 || !containerID.MatchString(providers[0]) {
		return "provider-sim is not running exactly once, so this is not the simulator."
	}

	// [S2]
	engines, ok := running("engine")
	if !ok {
		return unasked
	}

	if len(engines) != 1 || !containerID.MatchString(engines[0]) {
		return "there is not exactly one engine container running."
	}

	id := engines[0]
	wanted := [][2]string{{"8180/tcp", "MERIDIAN_PUBLIC_URL"}, {"8181/tcp", "MERIDIAN_ADMIN_URL"}}

	for _, w := range wanted {
		value, set := env[w[1]]
		if !set {
			value = DefaultURLs[w[1]]
		}

		port, ok := urlPort(value)
		if !ok {
			return "MERIDIAN_PUBLIC_URL or MERIDIAN_ADMIN_URL cannot be read, so the engine ports cannot be compared."
		}

		bound, ok := ask(docker, "port", id, w[0])
		if !ok {
			return unasked
		}

		if b := nonBlankLines(bound); len(b) != 1 || b[0] != "127.0.0.1:"+port {
			return "the engine does not own the public and admin ports on 127.0.0.1 (ports differ from the URLs)."
		}
	}

	// [S3]
	printed, ok := ask(docker, "inspect", id, "--format", SimTemplate)
	if !ok {
		return unasked
	}

	tokens := strings.Fields(printed)

	var missing []string

	for _, e := range simEnv {
		if !slices.Contains(tokens, e[0]) {
			missing = append(missing, e[0])
		}
	}

	if len(missing) > 0 {
		return "the engine is not set up for the simulator (missing: " + strings.Join(missing, ", ") + ")."
	}

	if len(tokens) != len(simEnv) {
		return "the engine is not set up for the simulator."
	}

	// [S4]
	label, ok := ask(docker, "inspect", id, "--format", EnvFileTemplate)
	if !ok {
		return unasked
	}

	mine := filepath.Join(rt, envFileName)
	if f := env["GAUSSMERIDIAN_ENV_FILE"]; f != "" {
		mine = f
	}

	// The label is set by the installed compose (checked on the live simulator engine), so a
	// missing label refuses; there is no weaker fallback.
	if stated := strings.TrimSpace(label); stated == "" || !samePath(stated, mine) {
		return "the engine was not started from the env file of this runtime."
	}

	return ""
}

// EngineReady reports a plain 200 from the engine's /ready within 5 s. No proxy and no redirect,
// so the answer is the engine's own.
func EngineReady(publicURL string) bool {
	client := &http.Client{
		Transport:     &http.Transport{Proxy: nil},
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	resp, err := client.Get(strings.TrimRight(publicURL, "/") + "/ready")
	if err != nil {
		return false
	}

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}
