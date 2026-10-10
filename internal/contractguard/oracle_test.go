package contractguard

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// The guard against the engine's own local-up readers (copies in testdata, from meridian-python):
// whenever a reader would hand the engine a paid key, the guard must refuse. Opt-in, because it
// runs pwsh and bash: MERIDIAN_GUARD_ORACLES=1 go test ./internal/contractguard/ -run Oracle.
// The compose reader is not run here: it needs docker, and these tests never call docker.

// toolEnv is what pwsh and bash need to start; no other variable of this process reaches them,
// so a bare `NAME` line cannot pick up a value from here.
func toolEnv() []string {
	keep := []string{"PATH", "SYSTEMROOT", "PROGRAMFILES", "PROGRAMDATA", "HOME", "USERPROFILE", "TEMP", "TMP", "LANG", "LC_ALL"}

	var env []string

	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && slices.Contains(keep, strings.ToUpper(name)) {
			env = append(env, kv)
		}
	}

	return env
}

// someOfEachCategory is about 24 code points from each category the ICU StartsWith skips most of:
// marks, format and control characters.
func someOfEachCategory() []rune {
	var picked []rune

	for _, table := range []*unicode.RangeTable{unicode.Cc, unicode.Mn, unicode.Mc, unicode.Me, unicode.Cf} {
		var chars []rune

		for r := rune(0x80); r <= unicode.MaxRune; r++ {
			if unicode.Is(table, r) {
				chars = append(chars, r)
			}
		}

		step := (len(chars) + 23) / 24
		for i := 0; i < len(chars); i += step {
			picked = append(picked, chars[i])
		}
	}

	return picked
}

// differentialTexts are the reviewers' probes, the unit table, and suspicious characters around,
// inside and at the end of a name.
func differentialTexts() map[string]string {
	texts := reviewFixtures()
	for i, c := range textsTable() {
		texts[fmt.Sprintf("texts[%d]", i)] = c
	}

	for _, c := range hiding {
		ch := string(c)
		texts[fmt.Sprintf("U+%04X after the name", c)] = G + ch + "=FAKE\n"
		texts[fmt.Sprintf("U+%04X before the name", c)] = ch + O + "=FAKE\n"
		texts[fmt.Sprintf("U+%04X as the value", c)] = G + "=" + ch + "\n"
	}

	inside := slices.Clone(hiding)
	for c := rune(0); c < 0x80; c++ {
		if c != '\n' {
			inside = append(inside, c)
		}
	}

	for _, c := range inside {
		texts[fmt.Sprintf("U+%04X inside the name", c)] = G[:5] + string(c) + G[5:] + "=FAKE\n"
		texts[fmt.Sprintf("U+%04X ends the name", c)] = O + string(c) + "=FAKE\n"
	}

	for _, c := range someOfEachCategory() {
		texts[fmt.Sprintf("U+%04X inside the name", c)] = G[:5] + string(c) + G[5:] + "=FAKE\n"
	}

	return texts
}

func textsTable() []string {
	out := make([]string, len(texts))
	for i, c := range texts {
		out[i] = c.text
	}

	return out
}

// readWith runs a copied reader over every text and returns the paid names it set, per label.
func readWith(t *testing.T, command []string, all map[string]string) map[string][]string {
	t.Helper()

	labels := slices.Sorted(maps.Keys(all))
	dir := t.TempDir()

	for i, label := range labels {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("%06d.env", i)), all[label])
	}

	cmd := exec.Command(command[0], append(command[1:], filepath.ToSlash(dir))...)
	cmd.Env = toolEnv()

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s failed", filepath.Base(command[len(command)-1])) // not its output, which might hold a value
	}

	found := map[string][]string{}

	for _, row := range strings.Split(strings.TrimSpace(strings.ReplaceAll(string(out), "\r\n", "\n")), "\n") {
		cols := strings.Split(row, "\t")
		if len(cols) != 3 {
			t.Fatalf("a row with %d columns", len(cols))
		}

		index, err := strconv.Atoi(cols[0])
		if err != nil || index >= len(labels) {
			t.Fatalf("a row for an unknown file")
		}

		var names []string
		if cols[1] == "1" {
			names = append(names, G)
		}

		if cols[2] == "1" {
			names = append(names, O)
		}

		found[labels[index]] = names
	}

	if len(found) != len(labels) {
		t.Fatalf("%d rows for %d files", len(found), len(labels))
	}

	return found
}

// What each reader must find, so one that reads nothing cannot pass. Both local-up readers keep
// two single quotes as a value, match case and take the first line; pwsh splits lines at a lone CR, bash deletes it.
var anchors = map[string]map[string][]string{
	"pwsh": {"r3-sq-G": {G}, "20-lower": nil, "19-first-empty": nil, "01-cr-after-eq": nil},
	"bash": {"r3-sq-G": {G}, "20-lower": nil, "19-first-empty": nil, "01-cr-after-eq": {G}},
}

func TestOracleWheneverAReaderSetsAPaidKeyTheGuardRefuses(t *testing.T) {
	if os.Getenv("MERIDIAN_GUARD_ORACLES") != "1" {
		t.Skip("set MERIDIAN_GUARD_ORACLES=1 to run the local-up readers (pwsh, bash)")
	}

	all := differentialTexts()

	// The texts the guard lets a contract run start with, as the runtime's env file.
	environ, folder := runtimeWith(t, "")
	guardAllows := map[string]bool{}

	for label, text := range all {
		writeFile(t, filepath.Join(folder, envFileName), text)

		if Refusal(environ, provenSystem(environ)) == "" {
			guardAllows[label] = true
		}
	}

	readers := map[string][]string{}
	if pwsh, err := exec.LookPath("pwsh"); err == nil {
		readers["pwsh"] = []string{pwsh, "-NoProfile", "-NonInteractive", "-File", filepath.Join("testdata", "get_env_value.ps1")}
	}

	if bash, err := exec.LookPath("bash"); err == nil {
		wsl := runtime.GOOS == "windows" && IsInside(os.Getenv("SYSTEMROOT"), bash)
		if !wsl {
			readers["bash"] = []string{bash, filepath.ToSlash(filepath.Join("testdata", "env_value.sh"))}
		}
	}

	if len(readers) == 0 {
		t.Skip("neither pwsh nor bash is on PATH")
	}

	for name, command := range readers {
		t.Run(name, func(t *testing.T) {
			found := readWith(t, command, all)

			var missed []string

			for label := range guardAllows {
				for _, key := range found[label] {
					missed = append(missed, label+": "+key)
				}
			}

			slices.Sort(missed)

			if len(missed) > 0 {
				t.Fatalf("the guard allowed texts this reader sets a paid key from: %v", missed)
			}

			for label, want := range anchors[name] {
				if !slices.Equal(found[label], want) {
					t.Errorf("anchor %s: got %v, want %v", label, found[label], want)
				}
			}
		})
	}
}
