package contractguard

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// -- helpers ---------------------------------------------------------------------------------------

// runtimeWith makes a runtime folder under a fresh LOCALAPPDATA with an env file holding text
// (a harmless line when text is empty).
func runtimeWith(t *testing.T, text string) (Environ, string) {
	t.Helper()

	if text == "" {
		text = "GAUSSMERIDIAN_DB_PASSWORD=x\n"
	}

	root := t.TempDir()
	folder := filepath.Join(root, "Gaussian", "MeridianEngine")

	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(folder, envFileName), text)

	return Environ{"LOCALAPPDATA": root}, folder
}

func with(environ Environ, pairs ...string) Environ {
	out := maps.Clone(environ)
	for i := 0; i < len(pairs); i += 2 {
		out[pairs[i]] = pairs[i+1]
	}

	return out
}

func noRegistry() (map[string]string, error) { return map[string]string{}, nil }

// engineID is the short id the fake docker reports for the engine container.
const engineID = "0123456789ab"

// fakeDocker answers the guard's docker calls from a script. It never runs docker.
type fakeDocker struct {
	project   string
	providers string
	engines   string
	ports     map[string]string
	env       string
	label     string
	broken    string // the docker subcommand that fails
	calls     [][]string
}

// newKeys are the engine's direct-provider keys, with the token each dummy prints.
var newKeys = []struct{ name, token string }{
	{OpenAI, "openai_dummy"},
	{Anthropic, "anthropic_dummy"},
	{XAI, "xai_dummy"},
	{DeepSeek, "deepseek_dummy"},
	{Mistral, "mistral_dummy"},
	{Qwen, "qwen_dummy"},
}

// allTokens is what the allowlisted comparison prints for a healthy simulator engine.
const allTokens = "gemini_base openrouter_base sim_flag gemini_dummy openrouter_dummy " +
	"openai_dummy anthropic_dummy xai_dummy deepseek_dummy mistral_dummy qwen_dummy "

// proven is the docker of a simulator engine started from envFile.
func proven(envFile string) *fakeDocker {
	return &fakeDocker{
		project:   "meridian-engine",
		providers: "fedcba987654\n",
		engines:   engineID + "\n",
		ports:     map[string]string{"8180/tcp": "127.0.0.1:8180\n", "8181/tcp": "127.0.0.1:8181\n"},
		env:       allTokens,
		label:     envFile + "\n",
	}
}

func (d *fakeDocker) run(args []string) (string, error) {
	d.calls = append(d.calls, args)

	if len(args) == 0 || args[0] == d.broken {
		return "", errors.New("exit status 1")
	}

	switch args[0] {
	case "ps":
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "label=com.docker.compose.project="+d.project+" ") {
			return "", nil
		}

		switch {
		case strings.Contains(joined, "label=com.docker.compose.service=provider-sim "):
			return d.providers, nil
		case strings.Contains(joined, "label=com.docker.compose.service=engine "):
			return d.engines, nil
		}
	case "port":
		return d.ports[args[2]], nil
	case "inspect":
		switch args[len(args)-1] {
		case SimTemplate:
			return d.env, nil
		case EnvFileTemplate:
			return d.label, nil
		}
	}

	return "", errors.New("unexpected docker call")
}

// expectedEnvFile is the env file the guard binds the engine to.
func expectedEnvFile(environ Environ) string {
	if f := environ["GAUSSMERIDIAN_ENV_FILE"]; f != "" {
		return f
	}

	return filepath.Join(RuntimeDir(environ), envFileName)
}

// provenSystem is a system with no registry proxy and a docker that proves simulator mode.
func provenSystem(environ Environ) System {
	return System{Registry: noRegistry, Docker: proven(expectedEnvFile(environ)).run}
}

func refused(t *testing.T, message string, wants ...string) {
	t.Helper()

	if message == "" {
		t.Fatal("allowed, want a refusal")
	}

	for _, want := range wants {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q lacks %q", message, want)
		}
	}

	if strings.Contains(message, secret) {
		t.Fatalf("message %q holds the secret", message)
	}
}

func allowed(t *testing.T, message string) {
	t.Helper()

	if message != "" {
		t.Fatalf("refused: %s", message)
	}
}

// -- loopback -------------------------------------------------------------------------------------

func TestLoopbackURLsAreAccepted(t *testing.T) {
	for _, u := range []string{
		"http://127.0.0.1:8180", "http://127.0.0.1", "http://127.0.0.2:8181", "http://127.255.255.254/",
		"http://localhost:8181", "http://LocalHost:8181", "https://127.0.0.1:8180/ready", "http://[::1]:8180",
		"http://[::ffff:127.0.0.1]:8180", "http://[::ffff:7f00:1]:8180", "HTTP://127.0.0.1:8180",
	} {
		if !IsLoopbackURL(u) {
			t.Errorf("%q refused", u)
		}
	}
}

func TestAnythingElseIsRefused(t *testing.T) {
	for _, u := range []string{
		"http://127.0.0.1.evil.com", "http://localhost.evil.com", "http://localhost@evil.com",
		"http://127.0.0.1@evil.com", "http://127.0.0.1:80@evil.com", "http://evil.com@127.0.0.1",
		"http://user:pass@127.0.0.1:8180", `http://127.0.0.1\@evil.com`, `http://evil.com\@127.0.0.1`,
		"http://127.0.0.1 .evil.com", "http://127.0.0.1\n.evil.com", "http://127.0.0.1:8180\n",
		"http://127.0.0.1:8180\t", " http://127.0.0.1:8180", "http://127.0.0.1:8180\x7f", "https://api.example.com",
		"http://0.0.0.0:8180", "http://[::]:8180", "http://10.0.0.5", "http://192.168.1.10:8180",
		"http://[::ffff:8.8.8.8]", "http://[::ffff:808:808]", "http://2130706433", "http://0x7f000001",
		"http://0x7f.0.0.1", "http://0177.0.0.1", "http://127.1", "http://127.0.0.1.", "http://localhost.",
		"http://localhost%2eevil.com", "http://[::1%25lo]:8180", "http://\xef\xbc\x91\xef\xbc\x92\xef\xbc\x97.0.0.1",
		"http://127.0.0.1:99999", "http://127.0.0.1:abc", "http://::1", "http://[::1", "ftp://127.0.0.1",
		"file:///etc/passwd", "localhost:8180", "//127.0.0.1:8180", "127.0.0.1", "", "not a url",
	} {
		if IsLoopbackURL(u) {
			t.Errorf("%q accepted", u)
		}
	}
}

// -- containment ----------------------------------------------------------------------------------

// link makes a directory link: a symlink where the platform allows it, a junction on Windows otherwise.
func link(t *testing.T, target, at string) {
	t.Helper()

	if err := os.Symlink(target, at); err == nil {
		return
	}

	if runtime.GOOS != "windows" {
		t.Skip("cannot create a symlink here")
	}

	if err := exec.Command("cmd", "/c", "mklink", "/J", at, target).Run(); err != nil {
		t.Fatalf("mklink: %v", err)
	}
}

func TestContainmentResolvesPaths(t *testing.T) {
	tmp := t.TempDir()
	rt := filepath.Join(tmp, "MeridianEngine")

	cases := []struct {
		child string
		want  bool
	}{
		{filepath.Join(rt, "meridian-engine.env"), true},
		{filepath.Join(rt, "a", "..", "meridian-engine.env"), true},
		{rt + string(filepath.Separator) + ".." + string(filepath.Separator) + "elsewhere.env", false},
		{rt, false},
		{filepath.Join(tmp, "MeridianEngine2", "x.env"), false},
		{tmp, false},
		{filepath.Join(tmp, "x") + "\x00", false},
	}
	for _, c := range cases {
		if got := IsInside(rt, c.child); got != c.want {
			t.Errorf("IsInside(%q) = %v", c.child, got)
		}
	}
}

func TestALinkOutOfTheFolderIsFollowed(t *testing.T) {
	tmp := t.TempDir()
	rt, outside := filepath.Join(tmp, "MeridianEngine"), filepath.Join(tmp, "outside")
	mkdirs(t, rt, outside)
	link(t, outside, filepath.Join(rt, "escape"))

	if IsInside(rt, filepath.Join(rt, "escape", "meridian-engine.env")) || IsInside(rt, filepath.Join(rt, "escape")) {
		t.Fatal("a link out of the folder counted as inside")
	}
}

func TestALinkIntoTheFolderIsInside(t *testing.T) {
	tmp := t.TempDir()
	rt := filepath.Join(tmp, "MeridianEngine")
	mkdirs(t, rt)
	link(t, rt, filepath.Join(tmp, "alias"))

	if !IsInside(rt, filepath.Join(tmp, "alias", "meridian-engine.env")) ||
		!IsInside(filepath.Join(tmp, "alias"), filepath.Join(rt, "meridian-engine.env")) {
		t.Fatal("a link into the folder counted as outside")
	}
}

func TestWindowsDriveLettersAndSeparatorsAreNotCaseOrSlashSensitive(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path spelling")
	}

	tmp := t.TempDir()
	rt := filepath.Join(tmp, "MeridianEngine")
	mkdirs(t, rt)

	swapped := strings.ToLower(rt[:1]) + rt[1:]
	if swapped == rt {
		swapped = strings.ToUpper(rt[:1]) + rt[1:]
	}

	if !IsInside(swapped, rt+`\meridian-engine.env`) ||
		!IsInside(strings.ReplaceAll(rt, `\`, "/"), swapped+`\a/b.env`) ||
		!IsInside(strings.ToUpper(rt), strings.ToLower(rt)+"/x.env") {
		t.Fatal("spelling changed the answer")
	}

	if IsInside(strings.ReplaceAll(rt, `\`, "/"), swapped+`\..\x.env`) {
		t.Fatal("climbed out")
	}

	// a folder that does not exist yet (a first run) is compared by spelling alone
	if !IsInside(filepath.Join(tmp, "Missing"), filepath.Join(tmp, "MISSING", "x.env")) ||
		IsInside(filepath.Join(tmp, "Missing"), filepath.Join(tmp, "MISSING")) {
		t.Fatal("a missing folder")
	}
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()

	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

// -- the whole judgement ----------------------------------------------------------------------------

func TestACleanRuntimeIsAllowed(t *testing.T) {
	environ, _ := runtimeWith(t, "")
	allowed(t, Refusal(environ, provenSystem(environ)))
}

func TestTheRuntimeFolderFollowsTheEngineScripts(t *testing.T) {
	tmp := t.TempDir()
	cases := []struct {
		env  Environ
		want string
	}{
		{Environ{"LOCALAPPDATA": tmp}, filepath.Join(tmp, "Gaussian", "MeridianEngine")},
		{Environ{"XDG_STATE_HOME": tmp}, filepath.Join(tmp, "gaussian", "meridian-engine")},
		{Environ{"HOME": tmp}, filepath.Join(tmp, ".local", "state", "gaussian", "meridian-engine")},
		{Environ{"LOCALAPPDATA": filepath.Join(tmp, "a"), "XDG_STATE_HOME": filepath.Join(tmp, "b")},
			filepath.Join(tmp, "a", "Gaussian", "MeridianEngine")},
		{Environ{}, ""},
		{Environ{"LOCALAPPDATA": "", "HOME": ""}, ""},
	}
	for _, c := range cases {
		if got := RuntimeDir(c.env); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}

func TestAnUnknownRuntimeFolderIsARefusal(t *testing.T) {
	refused(t, Refusal(Environ{}, provenSystem(Environ{})), "runtime")
}

func TestAPaidKeyInTheEnvFileIsRefusedByName(t *testing.T) {
	for _, name := range both {
		environ, _ := runtimeWith(t, "A=1\r\nset "+name+"="+secret+"\r\n")
		refused(t, Refusal(environ, provenSystem(environ)), name)
	}
}

func TestAPaidKeyInTheProcessEnvironmentIsRefusedByName(t *testing.T) {
	environ, _ := runtimeWith(t, "")
	refused(t, Refusal(with(environ, strings.ToLower(G), secret), provenSystem(environ)), G)
	allowed(t, Refusal(with(environ, O, ""), provenSystem(environ)))
}

func TestANewProviderKeyInTheEnvFileIsRefusedByName(t *testing.T) {
	for _, k := range newKeys {
		environ, _ := runtimeWith(t, "A=1\nset "+k.name+"="+secret+"\n")
		message := Refusal(environ, provenSystem(environ))
		refused(t, message, k.name)

		if strings.Contains(message, secret) {
			t.Errorf("%s: the message holds the value", k.name)
		}

		if got := PaidKeysIn(k.name + "=" + secret); !slices.Equal(got, []string{k.name}) {
			t.Errorf("%s: got %v", k.name, got)
		}

		if got := PaidKeysIn(k.name + "=\n"); len(got) != 0 {
			t.Errorf("%s: an empty assignment counts as set: %v", k.name, got)
		}
	}
}

func TestANewProviderKeyInTheProcessEnvironmentIsRefusedByName(t *testing.T) {
	for _, k := range newKeys {
		environ, _ := runtimeWith(t, "")
		message := Refusal(with(environ, strings.ToLower(k.name), secret), provenSystem(environ))
		refused(t, message, k.name)

		if strings.Contains(message, secret) {
			t.Errorf("%s: the message holds the value", k.name)
		}

		allowed(t, Refusal(with(environ, k.name, ""), provenSystem(environ)))

		if got := PaidKeysInEnviron(Environ{k.name: "x"}); !slices.Equal(got, []string{k.name}) {
			t.Errorf("%s: got %v", k.name, got)
		}
	}
}

func TestANewProviderKeyThatIsNotThePublicDummyIsRefusedByNameOnly(t *testing.T) {
	for _, k := range newKeys {
		message, _ := simCase(t, func(e Environ, d *fakeDocker) Environ {
			d.env = strings.Replace(d.env, k.token+" ", "", 1)
			return e
		})
		refused(t, message, "missing: "+k.token)

		if strings.Contains(message, "sim-local-key") {
			t.Errorf("%s: the message holds the dummy", k.name)
		}
	}
}

func TestBothSourcesAreNamedTogether(t *testing.T) {
	environ, _ := runtimeWith(t, O+"="+secret)
	refused(t, Refusal(with(environ, G, secret), provenSystem(environ)), G, O)
}

func TestAMissingEnvFileIsARefusalThatSaysItIsMissing(t *testing.T) {
	environ, folder := runtimeWith(t, "")
	if err := os.Remove(filepath.Join(folder, envFileName)); err != nil {
		t.Fatal(err)
	}

	message := Refusal(environ, provenSystem(environ))
	refused(t, message, "missing")

	if strings.Contains(message, "UTF-8") {
		t.Fatalf("message %q", message)
	}

	// the file the engine would read is the override when there is one
	writeFile(t, filepath.Join(folder, envFileName), "A=1\n")
	override := with(environ, "GAUSSMERIDIAN_ENV_FILE", filepath.Join(folder, "nope.env"))
	refused(t, Refusal(override, provenSystem(override)), "missing")
}

func TestAnUnreadableEnvFileIsARefusal(t *testing.T) {
	environ, folder := runtimeWith(t, "")
	path := filepath.Join(folder, envFileName)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	mkdirs(t, path) // a directory cannot be read as text
	refused(t, Refusal(environ, provenSystem(environ)), "cannot be read")
}

func TestAnEnvFileThatCannotBeReadAsTextIsARefusal(t *testing.T) {
	utf16 := []byte{0xff, 0xfe}
	for _, r := range G + "=" + secret + "\n" {
		utf16 = append(utf16, byte(r), 0)
	}

	for _, raw := range [][]byte{utf16, []byte("A=\xff\xfe\n"), []byte("A=1\x00\n")} {
		environ, folder := runtimeWith(t, "")
		if err := os.WriteFile(filepath.Join(folder, envFileName), raw, 0o600); err != nil {
			t.Fatal(err)
		}

		refused(t, Refusal(environ, provenSystem(environ)))
	}
}

func TestAnEnvFileOverrideMustStayInTheRuntimeFolder(t *testing.T) {
	environ, folder := runtimeWith(t, "")
	writeFile(t, filepath.Join(folder, "other.env"), "A=1\n")

	inside := with(environ, "GAUSSMERIDIAN_ENV_FILE", filepath.Join(folder, "other.env"))
	allowed(t, Refusal(inside, provenSystem(inside)))

	outside := filepath.Join(filepath.Dir(filepath.Dir(folder)), "clean.env")
	writeFile(t, outside, "A=1\n")

	for _, bad := range []string{outside, folder + string(filepath.Separator) + ".." + string(filepath.Separator) + "clean.env", folder} {
		e := with(environ, "GAUSSMERIDIAN_ENV_FILE", bad)
		refused(t, Refusal(e, provenSystem(e)), "GAUSSMERIDIAN_ENV_FILE")
	}
}

func TestAnEnvFileThatIsALinkOutOfTheFolderIsRefused(t *testing.T) {
	environ, folder := runtimeWith(t, "")
	outside := filepath.Join(filepath.Dir(filepath.Dir(folder)), "outside")
	mkdirs(t, outside)
	writeFile(t, filepath.Join(outside, "clean.env"), "A=1\n")
	link(t, outside, filepath.Join(folder, "escape"))

	e := with(environ, "GAUSSMERIDIAN_ENV_FILE", filepath.Join(folder, "escape", "clean.env"))
	refused(t, Refusal(e, provenSystem(e)), "GAUSSMERIDIAN_ENV_FILE")
}

func TestARuntimeDirOverrideMustStayInTheRuntimeFolder(t *testing.T) {
	environ, folder := runtimeWith(t, "")
	mkdirs(t, filepath.Join(folder, "sub"))
	allowed(t, Refusal(with(environ, "MERIDIAN_RUNTIME_DIR", folder), provenSystem(environ)))
	allowed(t, Refusal(with(environ, "MERIDIAN_RUNTIME_DIR", filepath.Join(folder, "sub")), provenSystem(environ)))

	root := filepath.Dir(filepath.Dir(folder))
	elsewhere := filepath.Join(root, "elsewhere")
	mkdirs(t, elsewhere)
	writeFile(t, filepath.Join(elsewhere, envFileName), "A=1\n")

	sep := string(filepath.Separator)
	for _, bad := range []string{elsewhere, folder + sep + "..", folder + sep + "sub" + sep + ".." + sep + "..", root} {
		refused(t, Refusal(with(environ, "MERIDIAN_RUNTIME_DIR", bad), provenSystem(environ)), "MERIDIAN_RUNTIME_DIR")
	}
}

func TestARuntimeDirThatIsALinkOutOfTheFolderIsRefused(t *testing.T) {
	environ, folder := runtimeWith(t, "")
	outside := filepath.Join(filepath.Dir(filepath.Dir(folder)), "outside")
	mkdirs(t, outside)
	link(t, outside, filepath.Join(folder, "escape"))

	refused(t, Refusal(with(environ, "MERIDIAN_RUNTIME_DIR", filepath.Join(folder, "escape")), provenSystem(environ)),
		"MERIDIAN_RUNTIME_DIR")
}

func TestTheEnginesOwnEnvFileIsCheckedWhateverTheRuntimeDirSays(t *testing.T) {
	// the engine scripts read <runtime>/meridian-engine.env; a clean decoy in a sub-folder must not hide it
	environ, folder := runtimeWith(t, G+"="+secret)
	mkdirs(t, filepath.Join(folder, "sub"))
	writeFile(t, filepath.Join(folder, "sub", envFileName), "A=1\n")
	refused(t, Refusal(with(environ, "MERIDIAN_RUNTIME_DIR", filepath.Join(folder, "sub")), provenSystem(environ)), G)
}

func TestTheCanonicalEnvFileIsCheckedWhenBothOverridesPointAtACleanDecoy(t *testing.T) {
	environ, folder := runtimeWith(t, G+"="+secret)
	mkdirs(t, filepath.Join(folder, "sub"))
	writeFile(t, filepath.Join(folder, "sub", "clean.env"), "A=1\n")

	e := with(environ, "MERIDIAN_RUNTIME_DIR", filepath.Join(folder, "sub"),
		"GAUSSMERIDIAN_ENV_FILE", filepath.Join(folder, "sub", "clean.env"))
	refused(t, Refusal(e, provenSystem(e)), G)
}

func TestTheCanonicalEnvFileIsCheckedWhenOnlyTheEnvFilePointsElsewhere(t *testing.T) {
	environ, folder := runtimeWith(t, O+"="+secret)
	writeFile(t, filepath.Join(folder, "clean.env"), "A=1\n")

	e := with(environ, "GAUSSMERIDIAN_ENV_FILE", filepath.Join(folder, "clean.env"))
	refused(t, Refusal(e, provenSystem(e)), O)
}

func TestAnEnvFileNamedByTwoSpellingsIsReadOnceAndAnAbsentCanonicalOneIsFine(t *testing.T) {
	environ, folder := runtimeWith(t, "")
	sep := string(filepath.Separator)

	e := with(environ, "GAUSSMERIDIAN_ENV_FILE", folder+sep+"sub"+sep+".."+sep+envFileName)
	allowed(t, Refusal(e, System{Registry: noRegistry, Docker: proven(filepath.Join(folder, envFileName)).run}))

	if err := os.Remove(filepath.Join(folder, envFileName)); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(folder, "clean.env"), "A=1\n")
	e = with(environ, "GAUSSMERIDIAN_ENV_FILE", filepath.Join(folder, "clean.env"))
	allowed(t, Refusal(e, provenSystem(e)))
}

func TestAnEnvFileInTheRuntimeDirOverrideIsCheckedToo(t *testing.T) {
	environ, folder := runtimeWith(t, "")
	mkdirs(t, filepath.Join(folder, "sub"))
	writeFile(t, filepath.Join(folder, "sub", envFileName), O+"="+secret)
	refused(t, Refusal(with(environ, "MERIDIAN_RUNTIME_DIR", filepath.Join(folder, "sub")), provenSystem(environ)), O)
}

func TestAKeyFileThatLinksOutOfTheRuntimeFolderIsRefused(t *testing.T) {
	for _, name := range keyFiles {
		environ, folder := runtimeWith(t, "")
		outside := filepath.Join(filepath.Dir(filepath.Dir(folder)), "outside")
		mkdirs(t, outside)
		writeFile(t, filepath.Join(outside, name), "k")
		allowed(t, Refusal(environ, provenSystem(environ)))

		if err := os.Symlink(filepath.Join(outside, name), filepath.Join(folder, name)); err != nil {
			t.Skip("cannot create a file symlink here")
		}

		refused(t, Refusal(environ, provenSystem(environ)), name)
	}
}

func TestANonLoopbackURLIsRefusedByVariableNameOnly(t *testing.T) {
	for _, variable := range []string{"MERIDIAN_PUBLIC_URL", "MERIDIAN_ADMIN_URL", "MERIDIAN_URL"} {
		environ, _ := runtimeWith(t, "")
		if variable == "MERIDIAN_URL" {
			for _, loopback := range []string{"http://127.0.0.1:9999", "http://localhost:1", "http://[::1]:2"} {
				allowed(t, Refusal(with(environ, variable, loopback), provenSystem(environ)))
			}
		}

		message := Refusal(with(environ, variable, "http://user:"+secret+"@evil.example.com"), provenSystem(environ))
		refused(t, message, variable)

		if strings.Contains(message, "evil.example.com") {
			t.Fatalf("message %q", message)
		}
	}
}

func TestTheDefaultURLsAreLoopback(t *testing.T) {
	environ, _ := runtimeWith(t, "")
	allowed(t, Refusal(environ, provenSystem(environ)))
	refused(t, Refusal(with(environ, "MERIDIAN_PUBLIC_URL", ""), provenSystem(environ))) // set but empty
}

// -- proxies --------------------------------------------------------------------------------------

func TestAProxyInTheEnvironmentIsRefusedByNameOnly(t *testing.T) {
	for _, name := range []string{"http_proxy", "HTTP_PROXY", "https_proxy", "Https_Proxy", "ALL_PROXY", "all_proxy"} {
		environ, _ := runtimeWith(t, "")
		message := Refusal(with(environ, name, "http://user:"+secret+"@proxy.example:3128"), provenSystem(environ))
		refused(t, message, "proxy")

		if strings.Contains(message, "proxy.example") {
			t.Fatalf("message %q", message)
		}
	}
}

func TestAProxyVariableThatIsEmptyOrOnlyABypassListIsNotAProxy(t *testing.T) {
	environ, _ := runtimeWith(t, "")
	allowed(t, Refusal(with(environ, "HTTP_PROXY", "", "all_proxy", ""), provenSystem(environ)))
	allowed(t, Refusal(with(environ, "NO_PROXY", "*", "no_proxy", "127.0.0.1"), provenSystem(environ)))
}

func TestARegistryProxyIsRefusedByScheme(t *testing.T) {
	for _, scheme := range []string{"http", "https", "all"} {
		environ, _ := runtimeWith(t, "")
		system := provenSystem(environ)
		system.Registry = func() (map[string]string, error) {
			return map[string]string{scheme: "http://u:" + secret + "@reg.example:8080"}, nil
		}

		message := Refusal(with(environ, "no_proxy", "localhost"), system)
		refused(t, message, scheme)

		if strings.Contains(message, "reg.example") {
			t.Fatalf("message %q", message)
		}
	}
}

func TestAnyOtherRegistryProxyEntryIsRefused(t *testing.T) {
	for _, entries := range []map[string]string{
		{"socks": "socks://reg.example:1080"}, {"ftp": "http://reg.example:21"}, {"gopher": "reg.example:70"},
	} {
		environ, _ := runtimeWith(t, "")
		system := provenSystem(environ)
		system.Registry = func() (map[string]string, error) { return entries, nil }
		refused(t, Refusal(environ, system), "registry")
	}

	environ, _ := runtimeWith(t, "")
	system := provenSystem(environ)
	system.Registry = func() (map[string]string, error) { return map[string]string{"socks": ""}, nil }
	allowed(t, Refusal(environ, system))
}

func TestARegistryThatCannotBeReadCountsAsAProxy(t *testing.T) {
	environ, _ := runtimeWith(t, "")
	system := provenSystem(environ)
	system.Registry = func() (map[string]string, error) { return nil, errors.New("access denied") }
	refused(t, Refusal(environ, system), "proxy")

	system.Registry = nil
	refused(t, Refusal(environ, system), "proxy")
}

func TestTheProxyServerEntryIsReadTheWayPythonReadsIt(t *testing.T) {
	cases := map[string]map[string]string{
		"":                                {},
		"reg.example:3128":                {"http": "reg.example:3128", "https": "reg.example:3128", "ftp": "reg.example:3128"},
		"socks=reg.example:1080":          {"socks": "reg.example:1080"},
		"ftp=reg.example:21":              {"ftp": "reg.example:21"},
		"HTTP=a:1;https=b:2;":             {"http": "a:1", "https": "b:2"},
		"http=a:1;bare.example:3":         {"http": "a:1", "registry": "bare.example:3"},
		"socks=reg.example:1080;ftp=f:21": {"socks": "reg.example:1080", "ftp": "f:21"},
	}
	for server, want := range cases {
		if got := ParseProxyServer(server); !maps.Equal(got, want) {
			t.Errorf("%q: got %v, want %v", server, got, want)
		}

		environ, _ := runtimeWith(t, "")
		system := provenSystem(environ)
		system.Registry = func() (map[string]string, error) { return ParseProxyServer(server), nil }

		if message := Refusal(environ, system); (message == "") != (server == "") {
			t.Errorf("%q: message %q", server, message)
		}
	}
}
