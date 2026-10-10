// Package contractguard is the contract guard: a contract run talks to the real local engine and
// must never spend money, so it may start only when the engine is proven to be the provider
// simulator, started from this runtime's env file, with no paid key anywhere it could read one.
//
// It is a port of the Python guard (meridian-python tests/support/contract_guard.py) and of the
// JS simulator proof. Everything here reports names only: no value of an environment variable,
// env file, docker setting or URL ever reaches a message.
package contractguard

import (
	"bytes"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The paid provider keys: when any is set the engine would call a paid provider.
const (
	Gemini     = "GAUSSMERIDIAN_GEMINI_API_KEY"
	OpenRouter = "GAUSSMERIDIAN_OPENROUTER_API_KEY"
	OpenAI     = "GAUSSMERIDIAN_OPENAI_API_KEY"
	Anthropic  = "GAUSSMERIDIAN_ANTHROPIC_API_KEY"
	XAI        = "GAUSSMERIDIAN_XAI_API_KEY"
	DeepSeek   = "GAUSSMERIDIAN_DEEPSEEK_API_KEY"
	Mistral    = "GAUSSMERIDIAN_MISTRAL_API_KEY"
	Qwen       = "GAUSSMERIDIAN_QWEN_API_KEY"
)

const envFileName = "meridian-engine.env"

var (
	paidKeys = []string{Gemini, OpenRouter, OpenAI, Anthropic, XAI, DeepSeek, Mistral, Qwen}
	keyFiles = []string{"local-api-key.txt", "local-integrator-key.txt"}

	// DefaultURLs are the engine's loopback listeners when the environment names none.
	DefaultURLs = map[string]string{"MERIDIAN_PUBLIC_URL": "http://127.0.0.1:8180", "MERIDIAN_ADMIN_URL": "http://127.0.0.1:8181"}

	proxySchemeNames = []string{"http", "https", "all"}
)

// Environ is a process environment by name.
type Environ map[string]string

// ProcessEnviron is this process's environment. On Windows, where names are case-insensitive,
// names are upper-cased, as Python's os.environ does.
func ProcessEnviron() Environ {
	env := Environ{}

	for _, kv := range os.Environ() {
		// Windows keeps per-drive entries such as "=C:=C:\x", so the separator is searched for
		// after the first byte.
		if kv == "" {
			continue
		}

		rest, value, ok := strings.Cut(kv[1:], "=")
		if !ok {
			continue
		}

		name := kv[:1] + rest
		if runtime.GOOS == "windows" {
			name = strings.ToUpper(name)
		}

		env[name] = value
	}

	return env
}

// What a line may hold. Anything else is skipped, folded or split at differently by one of the
// readers (compose takes NBSP and NEL as blank and matches a dotted capital I to I; the
// culture-sensitive StartsWith of local-common.ps1 skips controls, marks and some letters; a lone
// CR ends a line for pwsh but not for compose or bash), so it is refused.
var printable = regexp.MustCompile(`^[\t\x20-\x7e]*$`)

// The only line with a paid name that leaves it unset: `[export |set ]NAME =` (or `:`), then
// nothing or `""`. Two single quotes are a value: both local-up readers strip double quotes only.
var emptyAssignment = regexp.MustCompile(`^(?:(?:export|set)[ \t]+)?(?:` +
	strings.ToLower(strings.Join(paidKeys, `|`)) + `)[ \t]*[=:](?:"")?$`)

// PaidKeysIn returns the names of the paid provider keys an env file's text may set, never a
// value. It is not a parser but an allowlist: a text with any character outside printable ASCII
// and tab (after one leading byte order mark; a lone CR included), or that is not UTF-8, cannot be
// checked and reports both names. Otherwise a line that names a paid key, in any letter case, sets
// it unless the line is exactly an empty assignment, so a whitespace-only value and two single quotes are set.
// A comment line is skipped unless it holds a quote, which may close a quoted value.
func PaidKeysIn(text string) []string {
	text = strings.TrimPrefix(text, "\xef\xbb\xbf")
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")

	if !utf8.ValidString(text) || slices.ContainsFunc(lines, func(l string) bool { return !printable.MatchString(l) }) {
		return slices.Clone(paidKeys)
	}

	var names []string

	for _, line := range lines {
		lowered := strings.ToLower(line)
		comment := strings.HasPrefix(strings.TrimLeft(lowered, " \t"), "#") && !strings.ContainsAny(line, "\"'`")

		if comment || emptyAssignment.MatchString(lowered) {
			continue
		}

		for _, name := range paidKeys {
			if strings.Contains(lowered, strings.ToLower(name)) && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}

	return names
}

// PaidKeysInEnviron returns the names of the paid provider keys that are non-empty in an
// environment, in any letter case, always in the same order.
func PaidKeysInEnviron(env Environ) []string {
	var names []string

	for _, key := range paidKeys {
		for name, value := range env {
			if strings.ToUpper(name) == key && value != "" {
				names = append(names, key)

				break
			}
		}
	}

	return names
}

// loopbackHost: `localhost` is accepted because the engine's own docs use it, but it is resolved by
// the system resolver (hosts file, DNS, a VPN client), which the guard does not control. Literal
// 127.0.0.1 or ::1 is preferred.
func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}

	// ParseAddr also refuses every decimal, hex, octal and short IPv4 spelling, and zones.
	addr, err := netip.ParseAddr(host)

	return err == nil && addr.Zone() == "" && addr.Unmap().IsLoopback() // IPv4-mapped IPv6 by its IPv4 part
}

// IsLoopbackURL reports whether value is an http(s) URL whose host, as written, is localhost, an
// address in 127.0.0.0/8 or ::1. No userinfo, no whitespace, control or non-ASCII characters, no
// `%` or backslash, no alternative IP spellings.
func IsLoopbackURL(value string) bool {
	for i := 0; i < len(value); i++ {
		if c := value[i]; c <= ' ' || c >= 0x7f || c == '%' || c == '\\' {
			return false
		}
	}

	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.User != nil ||
		strings.Contains(u.Host, "@") {
		return false
	}

	host := strings.ToLower(u.Hostname())
	if host == "" || (strings.Contains(host, ":") && !strings.HasPrefix(u.Host, "[")) {
		return false
	}

	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
			return false
		}
	}

	return loopbackHost(host)
}

// maxLinks bounds how many links a path may pass through before it counts as unresolvable.
const maxLinks = 64

// real is path made absolute, with every symlink and junction along it followed (as far as the
// path exists; the rest is kept as spelled) and, on Windows, in one letter case.
func real(path string) (string, bool) {
	if strings.ContainsRune(path, 0) {
		return "", false
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}

	for hops := 0; ; {
		resolved, followed, ok := followOne(abs)
		if !ok {
			return "", false
		}

		if !followed {
			if runtime.GOOS == "windows" {
				resolved = strings.ToLower(resolved)
			}

			return resolved, true
		}

		if hops++; hops > maxLinks {
			return "", false
		}

		abs = resolved
	}
}

// followOne walks an absolute, clean path from its root and follows the first link it meets,
// returning the new path and true; or the path unchanged and false when no part is a link.
// os.Readlink reads symlinks and Windows junctions alike, which filepath.EvalSymlinks does not.
func followOne(abs string) (string, bool, bool) {
	volume := filepath.VolumeName(abs)
	parts := strings.FieldsFunc(abs[len(volume):], func(r rune) bool { return os.IsPathSeparator(uint8(r)) })
	current := volume + string(filepath.Separator)

	for i, part := range parts {
		next := filepath.Join(current, part)

		target, err := os.Readlink(next)
		if err != nil {
			current = next

			continue
		}

		if !filepath.IsAbs(target) {
			target = filepath.Join(current, target)
		}

		return filepath.Clean(filepath.Join(append([]string{target}, parts[i+1:]...)...)), true, true
	}

	return current, false, true
}

// IsInside reports whether child, after symlinks, junctions and `..`, is strictly below parent.
// Anything unresolvable is not.
func IsInside(parent, child string) bool {
	p, okP := real(parent)
	c, okC := real(child)

	if !okP || !okC || c == p {
		return false
	}

	prefix := p
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}

	return strings.HasPrefix(c, prefix)
}

// samePath reports whether two paths resolve to the same place.
func samePath(a, b string) bool {
	ra, okA := real(a)
	rb, okB := real(b)

	return okA && okB && ra == rb
}

// RuntimeDir is the folder the engine's local scripts keep their secrets in, found the way
// local-common does; "" when it cannot be located.
func RuntimeDir(env Environ) string {
	switch {
	case env["LOCALAPPDATA"] != "":
		return filepath.Join(env["LOCALAPPDATA"], "Gaussian", "MeridianEngine")
	case env["XDG_STATE_HOME"] != "":
		return filepath.Join(env["XDG_STATE_HOME"], "gaussian", "meridian-engine")
	case env["HOME"] != "":
		return filepath.Join(env["HOME"], ".local", "state", "gaussian", "meridian-engine")
	}

	return ""
}

// ParseProxyServer reads the registry's ProxyServer entry the way Python's getproxies_registry
// does: a bare server serves http, https and ftp; otherwise `scheme=server` items separated by
// `;`, with an item without a scheme kept under `registry`. Scheme names are lower-cased.
func ParseProxyServer(server string) map[string]string {
	entries := map[string]string{}
	if server == "" {
		return entries
	}

	if !strings.ContainsAny(server, "=;") {
		for _, scheme := range []string{"http", "https", "ftp"} {
			entries[scheme] = server
		}

		return entries
	}

	for _, item := range strings.Split(server, ";") {
		if item == "" {
			continue
		}

		if scheme, address, ok := strings.Cut(item, "="); ok {
			entries[strings.ToLower(scheme)] = address
		} else {
			entries["registry"] = item
		}
	}

	return entries
}

// ProxySchemes returns the schemes (http, https, all) for which a proxy is configured in the
// environment (any letter case) or the registry entries, plus `registry` for any other proxy
// server entry there (socks, ftp, ...). Names only; a proxy would carry the suite's traffic and
// the local keys off-machine.
func ProxySchemes(env Environ, entries map[string]string) []string {
	var schemes []string

	for _, scheme := range proxySchemeNames {
		set := entries[scheme] != ""

		for name, value := range env {
			if value != "" && strings.ToLower(name) == scheme+"_proxy" {
				set = true
			}
		}

		if set {
			schemes = append(schemes, scheme)
		}
	}

	for name, value := range entries {
		if value != "" && !slices.Contains(proxySchemeNames, name) {
			return append(schemes, "registry")
		}
	}

	return schemes
}

// paidKeysInFile returns the names in an env file, or false when it cannot be read as UTF-8 text
// (so it cannot be checked).
func paidKeysInFile(path string) ([]string, bool) {
	data, err := os.ReadFile(path)
	if err != nil || bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) { // UTF-16 and binary hide names
		return nil, false
	}

	return PaidKeysIn(string(data)), true // PaidKeysIn drops one byte order mark and refuses a second
}

func exists(path string) bool {
	_, err := os.Lstat(path)

	return err == nil
}

// System is what the guard asks about this machine. Both are injectable; the guard's own tests
// never read this machine's registry or call docker. A nil Registry or Docker refuses.
type System struct {
	// Registry returns the Windows registry's proxy entries as ParseProxyServer reads them.
	Registry func() (map[string]string, error)
	// Docker runs docker with a fixed argv and no shell, and returns its standard output.
	Docker Docker
}

// Live is this machine: its registry and its docker.
func Live() System {
	return System{Registry: ReadRegistryProxies, Docker: RunDocker}
}

// Refusal says why a contract run must not start, or "" when it may. Names only; never a value,
// path or URL.
func Refusal(env Environ, sys System) string {
	// [1] The runtime folder the engine's scripts use is the only place the guard trusts.
	rt := RuntimeDir(env)
	if rt == "" {
		return "the local runtime folder cannot be located (set LOCALAPPDATA, XDG_STATE_HOME or HOME)."
	}

	// [2] Every path the run reads from must resolve inside it.
	folder := rt
	if f := env["MERIDIAN_RUNTIME_DIR"]; f != "" {
		folder = f
	}

	if !samePath(folder, rt) && !IsInside(rt, folder) {
		return "MERIDIAN_RUNTIME_DIR resolves outside the local runtime folder."
	}

	canonical := filepath.Join(rt, envFileName)

	engineEnv := canonical
	if f := env["GAUSSMERIDIAN_ENV_FILE"]; f != "" {
		engineEnv = f
	}

	if !IsInside(rt, engineEnv) {
		return "GAUSSMERIDIAN_ENV_FILE resolves outside the local runtime folder."
	}

	for _, name := range keyFiles {
		if keyFile := filepath.Join(folder, name); exists(keyFile) && !IsInside(rt, keyFile) {
			return name + " resolves outside the local runtime folder."
		}
	}

	// [3] No paid provider key may be set: not in the process, and not in any env file the engine
	//     or the suite could read: the canonical one, the runtime dir's own, and the override (a
	//     clean decoy must not hide the others). Only the file the engine would read has to exist.
	names := PaidKeysInEnviron(env)
	required, _ := real(engineEnv)
	checked := map[string]bool{}

	for _, envFile := range []string{engineEnv, canonical, filepath.Join(folder, envFileName)} {
		key, ok := real(envFile)
		if !ok {
			return "the engine env file cannot be read as UTF-8 text, so it cannot be checked for provider keys."
		}

		if checked[key] {
			continue
		}

		checked[key] = true

		if !exists(envFile) {
			if key == required {
				return "the engine env file is missing, so it cannot be checked for provider keys."
			}

			continue
		}

		found, ok := paidKeysInFile(envFile)
		if !ok {
			return "the engine env file cannot be read as UTF-8 text, so it cannot be checked for provider keys."
		}

		for _, name := range found {
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}

	if len(names) > 0 {
		return strings.Join(names, ", ") + " is set, so the engine would call a paid provider."
	}

	// [4] Only a loopback engine.
	for _, name := range []string{"MERIDIAN_PUBLIC_URL", "MERIDIAN_ADMIN_URL", "MERIDIAN_URL"} {
		value, ok := env[name]
		if !ok {
			value, ok = DefaultURLs[name]
		}

		if ok && !IsLoopbackURL(value) {
			return name + " is not a loopback URL (localhost, 127.0.0.0/8 or ::1)."
		}
	}

	// [4.5] The engine must be proven to be the simulator, started from this runtime's env file.
	//       After the URLs, so the ports it compares are loopback ones.
	if unproven := SimulatorProof(env, rt, sys.Docker); unproven != "" {
		return unproven
	}

	// [5] No proxy between the suite and the engine; a registry that cannot be read counts as one.
	entries := map[string]string{"unreadable": "1"}

	if sys.Registry != nil {
		if read, err := sys.Registry(); err == nil {
			entries = read
		}
	}

	if proxies := ProxySchemes(env, entries); len(proxies) > 0 {
		return "a proxy is configured (" + strings.Join(proxies, ", ") +
			"), so traffic and the local keys could leave this machine."
	}

	return ""
}
