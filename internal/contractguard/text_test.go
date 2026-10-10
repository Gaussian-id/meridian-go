package contractguard

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	G = Gemini
	O = OpenRouter
	// SQ is two single quotes: a two-character value for both local-up readers.
	SQ     = "''"
	dbRows = "GAUSSMERIDIAN_DB_USERNAME=fakeuser\nGAUSSMERIDIAN_DB_PASSWORD=fakepw\n"
)

// Characters outside ASCII, spelled as their UTF-8 bytes so the source stays ASCII.
const (
	bom      = "\xef\xbb\xbf" // U+FEFF ZERO WIDTH NO-BREAK SPACE (byte order mark)
	kelvin   = "\xe2\x84\xaa" // U+212A KELVIN SIGN
	longS    = "\xc5\xbf"     // U+017F LATIN SMALL LETTER LONG S
	dotted   = "\xc4\xb0"     // U+0130 LATIN CAPITAL LETTER I WITH DOT ABOVE
	dotless  = "\xc4\xb1"     // U+0131 LATIN SMALL LETTER DOTLESS I
	tatweel  = "\xd9\x80"     // U+0640 ARABIC TATWEEL
	etnahta  = "\xd6\x91"     // U+0591 HEBREW ACCENT ETNAHTA
	eAcute   = "\xc3\xa9"     // U+00E9 LATIN SMALL LETTER E WITH ACUTE
	nel      = "\xc2\x85"     // U+0085 NEXT LINE
	nbsp     = "\xc2\xa0"     // U+00A0 NO-BREAK SPACE
	shy      = "\xc2\xad"     // U+00AD SOFT HYPHEN
	lineSep  = "\xe2\x80\xa8" // U+2028 LINE SEPARATOR
	zwsp     = "\xe2\x80\x8b" // U+200B ZERO WIDTH SPACE
	emSpace  = "\xe2\x80\x83" // U+2003 EM SPACE
	tailFrag = "\xef\xb9\xb3" // U+FE73 ARABIC TAIL FRAGMENT
	lajanyal = "\xdf\xba"     // U+07FA NKO LAJANYALAN
	nirugu   = "\xe1\xa0\x8a" // U+180A MONGOLIAN NIRUGU
	nihshva  = "\xe1\xb3\x93" // U+1CD3 VEDIC SIGN NIHSHVASA
)

// secret is a stand-in value built at run time; no message may ever contain it.
var secret = "v" + strings.Repeat("q", 24)

var both = []string{G, O}

type textCase struct {
	text  string
	names []string
}

// texts are env file texts and the names the guard must report (a port of the Python guard's
// table). A paid name counts as set unless its line is exactly an empty assignment; a character
// that some reader skips, folds away or breaks a line at means neither name can be checked.
var texts = []textCase{
	{"GAUSSMERIDIAN_GEMINI_API_KEY=abc", []string{G}},
	{"export GAUSSMERIDIAN_OPENROUTER_API_KEY=\"x\"\r\n", []string{O}},
	{"gaussmeridian_gemini_api_key = y", []string{G}},
	{"set GAUSSMERIDIAN_GEMINI_API_KEY=abc\r\n", []string{G}},
	{"SET GAUSSMERIDIAN_OPENROUTER_API_KEY=abc", []string{O}},
	{"set \"GAUSSMERIDIAN_GEMINI_API_KEY=abc\"", []string{G}},
	{"   export    GAUSSMERIDIAN_GEMINI_API_KEY   =   \"abc\"   ", []string{G}},
	{"GAUSSMERIDIAN_GEMINI_API_KEY='abc'", []string{G}},
	{"GAUSSMERIDIAN_GEMINI_API_KEY: abc", []string{G}},
	{bom + "GAUSSMERIDIAN_GEMINI_API_KEY=abc", []string{G}},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=abc # trailing comment", []string{G}},
	// a quoted value that runs over several lines is a value
	{"GAUSSMERIDIAN_GEMINI_API_KEY=\"line one\nline two\"\nB=2", []string{G}},
	{"GAUSSMERIDIAN_OPENROUTER_API_KEY='a\r\nb\r\n'", []string{O}},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=\"", []string{G}},
	{`GAUSSMERIDIAN_GEMINI_API_KEY="a\"b"`, []string{G}},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=\" \"", []string{G}},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=#abc", []string{G}},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=   # looks empty", []string{G}},
	// any line that sets it counts, even if a later one empties it
	{"GAUSSMERIDIAN_GEMINI_API_KEY=abc\nGAUSSMERIDIAN_GEMINI_API_KEY=\n", []string{G}},
	{O + "=a\n" + G + "=b\n" + O + "=c", []string{O, G}},
	// a quoted value is followed by a new statement on the same line (compose)
	{"GAUSSMERIDIAN_GEMINI_API_KEY=\"\"GAUSSMERIDIAN_GEMINI_API_KEY=VAL", []string{G}},
	{"A=\"\" GAUSSMERIDIAN_OPENROUTER_API_KEY=VAL", []string{O}},
	{"A='' GAUSSMERIDIAN_OPENROUTER_API_KEY=VAL", []string{O}},
	{"A=\"x\nline\" GAUSSMERIDIAN_OPENROUTER_API_KEY=VAL", []string{O}},
	{"A=\"x\" GAUSSMERIDIAN_OPENROUTER_API_KEY=VAL", []string{O}},
	{`A="a\" GAUSSMERIDIAN_OPENROUTER_API_KEY=VAL`, []string{O}},
	{"A=\"x\nGAUSSMERIDIAN_OPENROUTER_API_KEY=VAL\n\"", []string{O}},
	// a line that starts with `#` but holds a quote may close a quoted value; compose then reads what follows
	{"A=\"x\n#\"GAUSSMERIDIAN_GEMINI_API_KEY=VAL", []string{G}},
	{"A=\"x\n# a\" GAUSSMERIDIAN_GEMINI_API_KEY=VAL", []string{G}},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=${UNSET:-VAL}", []string{G}},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=$X", []string{G}},
	// a whitespace-only value is set for both local-up readers
	{"GAUSSMERIDIAN_GEMINI_API_KEY= ", []string{G}},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=\t", []string{G}},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=   \r\n", []string{G}},
	// both local-up readers strip only double quotes, so two single quotes are a two-character value
	{"GAUSSMERIDIAN_GEMINI_API_KEY=''", []string{G}},
	{"export GAUSSMERIDIAN_GEMINI_API_KEY=''", []string{G}},
	{"GAUSSMERIDIAN_GEMINI_API_KEY\t:''", []string{G}},
	{G + "=\n" + O + "=''\n", []string{O}},
	// anything but exactly an empty assignment counts, wherever the name is on the line
	{"GAUSSMERIDIAN_GEMINI_API_KEY=''  # empty", []string{G}},
	{"set \"GAUSSMERIDIAN_GEMINI_API_KEY=\"", []string{G}},
	{"export GAUSSMERIDIAN_GEMINI_API_KEY", []string{G}}, // compose takes the value from its own environment
	{"GAUSSMERIDIAN_GEMINI_API_KEY", []string{G}},
	{"  GAUSSMERIDIAN_GEMINI_API_KEY=", []string{G}},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=\"\"\"\"", []string{G}},
	{"A=\"x\" # GAUSSMERIDIAN_OPENROUTER_API_KEY=VAL", []string{O}},
	{"OTHER=1 # GAUSSMERIDIAN_GEMINI_API_KEY=abc", []string{G}},
	{"REM set GAUSSMERIDIAN_GEMINI_API_KEY=abc", []string{G}},
	{"XGAUSSMERIDIAN_GEMINI_API_KEY=abc", []string{G}},
	{"GAUSSMERIDIAN_GEMINI_API_KEY_OTHER=abc", []string{G}},
	// anything outside printable ASCII and tab, anywhere in the file: neither name can be checked.
	// Some reader folds it to a name's letter (Kelvin sign, long s, dotted capital I), skips it
	// (marks, tatweel), or breaks a line at it.
	{"GAUSSMERIDIAN_GEMINI_API_" + kelvin + "EY=VAL", both},
	{"GAUS" + longS + "MERIDIAN_GEMINI_API_KEY=VAL", both},
	{"GAUSSMERIDIAN_GEMINI_API_" + kelvin + "EY=", both},
	{"GAUSSMER" + dotted + "DIAN_GEMINI_API_KEY=VAL", both},
	{"GAUSS" + tatweel + "MERIDIAN_GEMINI_API_KEY=VAL", both},
	{"GAUSSMERIDIAN_GEMINI_API_KEY" + etnahta + "=VAL", both},
	{"A=caf" + eAcute + "\n", both},
	{"# " + eAcute + "\n", both},
	{"A=1\rGAUSSMERIDIAN_GEMINI_API_KEY=abc\rB=2", both},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=   \rVAL", both},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=\rVAL", both},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=\r\"VAL\"", both},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=\r#VAL", both},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=\rA=1", both},
	{"GAUSSMERIDIAN_GEMINI_API_KEY\r=VAL", both},
	{"export\rGAUSSMERIDIAN_GEMINI_API_KEY=VAL", both},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=\r\r\n", both},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=\"\r\"", both},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=\x1c", both},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=\x1c\n", both},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=" + nel, both},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=" + nbsp, both},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=" + lineSep, both},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=" + zwsp, both},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=" + bom, both},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=\vVAL", both},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=\v\n", both},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=\f", both},
	{"GAUSSMERIDIAN_GEMINI_API_KEY= \t\v\f\r \r\n", both},
	{"GAUSSMERIDIAN_GEMINI_API\x01_KEY=VAL", both},
	{bom + bom + "GAUSSMERIDIAN_GEMINI_API_KEY=", both},
	{"A=1\n# a note" + nbsp + "\nB=2", both},
	// bytes that are not UTF-8 at all
	{"A=\xff\n", both},
	// not keys
	{"# GAUSSMERIDIAN_GEMINI_API_KEY=abc", nil},
	{"   # export GAUSSMERIDIAN_GEMINI_API_KEY=abc", nil},
	{"\t# GAUSSMERIDIAN_GEMINI_API_KEY=abc", nil},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=", nil},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=\"\"", nil},
	{"gaussmeridian_gemini_api_key=", nil},
	{"set GAUSSMERIDIAN_GEMINI_API_KEY=", nil},
	{"export GAUSSMERIDIAN_GEMINI_API_KEY=", nil},
	{"EXPORT\tGAUSSMERIDIAN_GEMINI_API_KEY=", nil},
	{"GAUSSMERIDIAN_GEMINI_API_KEY =", nil},
	{"GAUSSMERIDIAN_GEMINI_API_KEY\t=\"\"", nil},
	{"GAUSSMERIDIAN_GEMINI_API_KEY:", nil},
	{bom + "GAUSSMERIDIAN_GEMINI_API_KEY=", nil},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=\r\n", nil},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=\r\nA=1", nil},
	{"GAUSSMERIDIAN_GEMINI_API_KEY=\r\n\r\n", nil},
	{G + "=\n" + O + "=\"\"\n", nil},
	{"A=\"\"\nGAUSSMERIDIAN_GEMINI_API_KEY=\"\"\n# GAUSSMERIDIAN_GEMINI_API_KEY=abc", nil},
	{"A=\"x\n# GAUSSMERIDIAN_GEMINI_API_KEY=VAL\n\"", nil},
	{"A='x\n# GAUSSMERIDIAN_GEMINI_API_KEY=VAL\n'", nil},
	{"A=1\tB=~ !\"#$%&()*+,-./:;<>?@[\\]^_`{|}\n", nil},
	{"GAUSSMERIDIAN_DB_PASSWORD=abc", nil},
	{"", nil},
}

func TestPaidKeysAreFoundByNameOnly(t *testing.T) {
	for i, c := range texts {
		if got := PaidKeysIn(c.text); !slices.Equal(got, c.names) {
			t.Errorf("texts[%d] %q: got %v, want %v", i, c.text, got, c.names)
		}
	}
}

// hiding are characters some reader skips, folds or breaks a line at, plus what the
// culture-sensitive StartsWith of local-common.ps1 skips (ICU) or folds to a name's letter.
var hiding = []rune{
	0x01, 0x08, 0x0b, 0x0c, 0x0e, 0x1c, 0x1f, 0x7f, 0x80, 0x85, 0x9f, 0xa0, 0xad, 0x034f, 0x061c, 0x115f, 0x1680,
	0x17b4, 0x180b, 0x180e, 0x180f,
	0x2000, 0x200a, 0x200b, 0x200f, 0x202a, 0x202e, 0x202f, 0x2028, 0x2029, 0x205f, 0x2060, 0x2064, 0x206f, 0x3000,
	0x3164, 0xfe00, 0xfe0f,
	0xfeff, 0xffa0, 0xfff0, 0xfff9, 0x1bca0, 0x1d173, 0xe0001, 0xe0020, 0xe0100, 0xe0fff,
	'\r',
	0x00, 0x0300, 0x0591, 0x0640, 0x07fa, 0x180a, 0x1cd3, 0xfe73, 0x0903, 0x20dd, 0x0130, 0x0131, 0x017f, 0x212a,
	0x00e9, 0x4e2d, 0x1f600,
}

func TestACharacterOutsidePrintableASCIIAnywhereMeansNeitherNameCanBeChecked(t *testing.T) {
	for _, c := range hiding {
		ch := string(c)
		if got := PaidKeysIn("A=1\n# a note" + ch + "x\nB=2\n"); !slices.Equal(got, both) {
			t.Errorf("U+%04X in a comment: got %v", c, got)
		}

		if got := PaidKeysIn(G + ch + "=\n"); !slices.Equal(got, both) {
			t.Errorf("U+%04X after the name: got %v", c, got)
		}
	}
}

func TestPrintableASCIIAndTabDoNotStopTheCheck(t *testing.T) {
	printable := []rune{'\t'}
	for c := rune(0x20); c < 0x7f; c++ {
		printable = append(printable, c)
	}

	for _, c := range printable {
		ch := string(c)
		if got := PaidKeysIn("A=1" + ch + "\n# a note " + ch + "\n" + G + "=\n"); len(got) != 0 {
			t.Errorf("U+%04X: got %v, want none", c, got)
		}

		if got := PaidKeysIn(G + "=" + ch + "\n"); !slices.Equal(got, []string{G}) {
			t.Errorf("U+%04X as the value: got %v", c, got)
		}
	}
}

// reviewFixtures are the security reviewers' probes (fake values): every one is set for compose or
// for a local-up reader, except those in notSetForAnyReader and the dotless i, which no reader
// folds to I (the guard still refuses it, as it is not ASCII).
func reviewFixtures() map[string]string {
	f := map[string]string{
		"a01-name-nbsp-eq":                  G + nbsp + "=FAKEA01\n",
		"a02-name-nel-eq":                   G + nel + "=FAKEA02\n",
		"a03-export-nbsp":                   "export" + nbsp + G + "=FAKEA03\n",
		"a04-nbsp-emptyq-then-key":          "A=" + nbsp + "\"\"" + G + "=FAKEA04\n",
		"a05-nel-emptysq-then-key":          "A=" + nel + "''" + G + "=FAKEA05\n",
		"a06-name-nbsp-colon":               G + nbsp + ":FAKEA06\n",
		"a07-nbsp-q-then-key":               "A=" + nbsp + "\"x\"" + G + "=FAKEA07\n",
		"a08-export-nel":                    "export" + nel + G + "=FAKEA08\n",
		"a09-space-nbsp-eq":                 G + " " + nbsp + " =FAKEA09\n",
		"a10-self-nbsp-emptyq":              G + "=" + nbsp + "\"\"" + G + "=FAKEA10\n",
		"a11-lead-nbsp":                     nbsp + G + "=FAKEA11\n",
		"a12-export-sp-nbsp":                "export " + nbsp + G + "=FAKEA12\n",
		"a13-tab-emptyq-key":                "A=\t\"\"" + G + "=FAKEA13\n",
		"a14-vt-emptyq-key":                 "A=\v\"\"" + G + "=FAKEA14\n",
		"a15-bs-quote":                      `A="\\"` + G + `=FAKEA15"` + "\n",
		"a16-crlf-emptyq-cr-key":            "A=\"\"\r" + G + "=FAKEA16\n",
		"a17-sq-esc":                        `A='x\'` + G + "=FAKEA17\n",
		"a18-dq-multi-then":                 "A=\"l1\nl2\"" + G + "=FAKEA18\n",
		"a19-nbsp-before-colon-sp":          G + nbsp + " :FAKEA19\n",
		"a20-inherit":                       G + "\n",
		"b01-kelvin":                        "GAUSSMERIDIAN_GEMINI_API_" + kelvin + "EY=FAKEB01\n",
		"b02-longs":                         "GAUS" + longS + "MERIDIAN_GEMINI_API_KEY=FAKEB02\n",
		"b03-dotless-i":                     "GAUSSMER" + dotless + "DIAN_GEMINI_API_KEY=FAKEB03\n",
		"b04-openrouter-nbsp":               O + nbsp + "= FAKEB04\n",
		"b05-nel-q-then":                    "A=" + nel + "\"y\"" + O + "=FAKEB05\n",
		"b06-tab-nbsp-eq":                   G + "\t" + nbsp + "\t=FAKEB06\n",
		"c01-or-space_g-nbsp":               "GAUSSMERIDIAN_DB_USERNAME=fake\n" + O + "= \n" + G + nbsp + "=FAKEC01\n",
		"c02-or-vt_g-nbsp":                  O + "=\v\n" + G + nbsp + "=FAKEC02\n",
		"c03-or-softhyphen-in-quote_g-nbsp": "A=\"\n" + O + shy + "=x\n\"\n" + G + nbsp + "=FAKEC03\n",
		"c04-or-tab_g-nbsp-emptyq":          O + "=\t\nA=" + nbsp + "\"\"" + G + "=FAKEC04\n",
		"01-cr-after-eq":                    G + "=\rFAKEVAL01\n",
		"02-spaces-cr":                      G + "=   \rFAKEVAL02\n",
		"03-cr-quoted":                      G + "=\r\"FAKEVAL03\"\n",
		"04-same-line":                      "A=\"\" " + G + "=FAKEVAL04\n",
		"05-same-line-self":                 G + "=\"\"" + G + "=FAKEVAL05\n",
		"06-cr-before-eq":                   G + "\r=FAKEVAL06\n",
		"07-cr-hash":                        G + "=\r#FAKEVAL07\n",
		"08-after-multiline":                "A=\"x\nline\" " + O + "=FAKEVAL08\n",
		"09-export-cr":                      "export\r" + G + "=FAKEVAL09\n",
		"10-nbsp-only":                      G + "=" + nbsp + "\n",
		"11-emspace-only":                   G + "=" + emSpace + "\n",
		"12-zwsp-only":                      G + "=" + zwsp + "\n",
		"13-vt":                             G + "=\vFAKEVAL13\n",
		"14-vt-only":                        G + "=\v\n",
		"15-fs-only":                        G + "=\x1c\n",
		"16-nel-only":                       G + "=" + nel + "\n",
		"17-ff-only":                        G + "=\f\n",
		"18-last-empty":                     G + "=FAKEVAL18\n" + G + "=\n",
		"19-first-empty":                    G + "=\n" + G + "=FAKEVAL19\n",
		"20-lower":                          strings.ToLower(G) + "=FAKEVAL20\n",
		"21-u2028":                          G + "=" + lineSep + "FAKEVAL21\n",
		"22-fs-val":                         G + "=\x1cFAKEVAL22\n",
		"23-quoted-cr":                      G + "=\"\r\"\n",
		"24-bom-in-value":                   G + "=" + bom + "FAKEVAL24\n",
		"25-bom":                            bom + G + "=FAKEVAL25\n",
		"26-interp":                         G + "=${UNSET_THING:-FAKEVAL26}\n",
		"27-ref":                            "X=FAKEVAL27\n" + G + "=$X\n",
		"r3-sq-G":                           G + "=" + SQ + "\n",
		"r3-sq-O":                           O + "=" + SQ + "\n",
		"r3-sq-both":                        G + "=" + SQ + "\n" + O + "=" + SQ + "\n",
		"r3-sq-kelvin":                      "GAUSSMERIDIAN_GEMINI_API_" + kelvin + "EY=" + SQ + "\n",
		"r3-sq-export":                      "export " + G + "=" + SQ + "\n",
		"r3-sq-set":                         "set " + G + "=" + SQ + "\n",
		"r3-sq-colon":                       G + ":" + SQ + "\n",
		"r3-sq-tab":                         G + "\t=" + SQ + "\n",
		"r3-sq-lower":                       strings.ToLower(G) + "=" + SQ + "\n",
		"r3-sq-then-empty":                  G + "=" + SQ + "\n" + G + "=\n",
		"r3-empty-then-sq":                  G + "=\n" + G + "=" + SQ + "\n",
		"r3-sq-comment":                     G + "=" + SQ + " # c\n",
		"r3-chain-sq-dotted":                dbRows + O + "=" + SQ + "\nGAUSSMER" + dotted + "DIAN_GEMINI_API_KEY=FAKER3A\n",
		"r3-chain-icu-dotted": dbRows + "GAUSS" + tatweel + "MERIDIAN_OPENROUTER_API_KEY=\nGAUSSMER" + dotted +
			"DIAN_GEMINI_API_KEY=FAKER3B\n",
		"r3-chain-dotted-lower": dbRows + "gaussmer" + dotted + "dian_gemini_api_key=FAKER3C\n",
		"r3-chain-all-dotted":   dbRows + O + "=" + SQ + "\n" + strings.ReplaceAll(G, "I", dotted) + "=FAKER3D\n",
		"r3-export-dotted":      O + "=" + SQ + "\nexport GAUSSMER" + dotted + "DIAN_GEMINI_API_KEY=FAKER3E\n",
		"r3-dotted-colon":       O + "=" + SQ + "\nGAUSSMER" + dotted + "DIAN_GEMINI_API_KEY: FAKER3F\n",
		"r3-tatweel-val":        "GAUSS" + tatweel + "MERIDIAN_GEMINI_API_KEY=FAKER3G\n",
		"r3-fe73-val":           "GAUSS" + tailFrag + "MERIDIAN_GEMINI_API_KEY=FAKER3H\n",
		"r3-07fa-val":           "GAUSS" + lajanyal + "MERIDIAN_GEMINI_API_KEY=FAKER3I\n",
		"r3-180a-val":           "GAUSS" + nirugu + "MERIDIAN_GEMINI_API_KEY=FAKER3J\n",
		"r3-1cd3-val":           "GAUSS" + nihshva + "MERIDIAN_GEMINI_API_KEY=FAKER3K\n",
		"r3-0591-val":           "GAUSS" + etnahta + "MERIDIAN_GEMINI_API_KEY=FAKER3L\n",
		"r3-mark-in-multiline":  "A=\"\nGAUSS" + etnahta + "MERIDIAN_OPENROUTER_API_KEY=\n\"\n",
		"r3-interp-other":       "X=${" + G + ":-FAKER3M}\n",
		"r3-self-ref":           G + "=${" + G + "}\n",
		"r3-sq-multiline-hash":  "A='x\n# " + G + "=FAKER3N\n'\n",
		"r3-dq-multiline-hash":  "A=\"x\n# " + G + "=FAKER3O\n\"\n",
		"r3-unclosed-then-hash": "A=\"x\n# " + G + "=FAKER3P\n",
		"r3-backtick-hash":      "A=`x\n# " + G + "=FAKER3Q\n`\n",
	}

	for _, name := range both {
		for at, letter := range name {
			if letter == 'I' {
				f[fmt.Sprintf("r3-dotted-%c-%d", name[14], at)] = name[:at] + dotted + name[at+1:] + "=FAKEDOT\n"
			}
		}
	}

	return f
}

// notSetForAnyReader: a `#` line inside a quoted value. Compose reads it as part of the value (or
// rejects the unclosed one), and neither local-up reader reads it.
var notSetForAnyReader = map[string]bool{
	"r3-sq-multiline-hash": true, "r3-dq-multiline-hash": true, "r3-unclosed-then-hash": true, "r3-backtick-hash": true,
}

func TestEveryReviewerProbeIsRefused(t *testing.T) {
	for label, text := range reviewFixtures() {
		t.Run(label, func(t *testing.T) {
			environ, folder := runtimeWith(t, "")
			writeFile(t, filepath.Join(folder, envFileName), text)

			message := Refusal(environ, provenSystem(environ))
			if notSetForAnyReader[label] {
				if message != "" {
					t.Fatalf("refused: %s", message)
				}

				return
			}

			named := G
			if strings.Contains(text, O) {
				named = O
			}

			if !strings.Contains(message, named) || strings.Contains(message, "FAKE") {
				t.Fatalf("message %q", message)
			}
		})
	}
}

func TestAFileWithTwoByteOrderMarksCannotBeChecked(t *testing.T) {
	environ, folder := runtimeWith(t, "")
	writeFile(t, filepath.Join(folder, envFileName), bom+bom+G+"=\n")

	if message := Refusal(environ, provenSystem(environ)); !strings.Contains(message, G) {
		t.Fatalf("message %q", message)
	}

	writeFile(t, filepath.Join(folder, envFileName), bom+G+"=\n")

	if message := Refusal(environ, provenSystem(environ)); message != "" {
		t.Fatalf("refused: %s", message)
	}
}

func TestAValueNeverComesBack(t *testing.T) {
	found := PaidKeysIn(G + "=" + secret + "\n" + O + "=\"" + secret + "\"")
	if !slices.Equal(found, both) || strings.Contains(fmt.Sprint(found), secret) {
		t.Fatalf("got %v", found)
	}
}

func TestTheProcessEnvironmentIsJudgedByNameAndWithoutCase(t *testing.T) {
	cases := []struct {
		env  Environ
		want []string
	}{
		{Environ{"gaussmeridian_gemini_api_key": "x", O: ""}, []string{G}},
		{Environ{"GAUSSMERIDIAN_DB_PASSWORD": "x", "PATH": "/bin"}, nil},
		{Environ{}, nil},
	}
	for _, c := range cases {
		if got := PaidKeysInEnviron(c.env); !slices.Equal(got, c.want) {
			t.Errorf("%v: got %v, want %v", slices.Sorted(maps.Keys(c.env)), got, c.want)
		}
	}

	// Always in the same order, whatever order the environment is walked in.
	if got := PaidKeysInEnviron(Environ{O: " ", G: "x"}); !slices.Equal(got, both) {
		t.Errorf("got %v", got)
	}
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}
