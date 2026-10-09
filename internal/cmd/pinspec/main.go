// Command pinspec copies the engine's committed OpenAPI documents into spec/ and records the
// engine commit in spec/SOURCE.json. It reads git HEAD, never the working tree.
//
// Usage (from the repository root): go run ./internal/cmd/pinspec [engine-repo]
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	// [1] The engine repository is the argument or the sibling folder.
	engine := filepath.Join("..", "..", "1. Engine")
	if len(os.Args) > 1 {
		engine = os.Args[1]
	}

	git := func(args ...string) []byte {
		out, err := exec.Command("git", append([]string{"-C", engine}, args...)...).Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "git %v: %v\n", args, err)
			os.Exit(1)
		}

		return out
	}

	// [2] Copy the committed documents byte for byte.
	versions := map[string]string{}

	for _, name := range []string{"public", "admin"} {
		file := "meridian-" + name + ".json"
		data := git("show", "HEAD:deploy/openapi/"+file)

		if err := os.WriteFile(filepath.Join("spec", file), data, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		var doc struct {
			Info struct {
				Version string `json:"version"`
			} `json:"info"`
		}

		_ = json.Unmarshal(data, &doc)
		versions[name] = doc.Info.Version
	}

	// [3] Record where they came from.
	source := map[string]string{
		"engineCommit":  strings.TrimSpace(string(git("rev-parse", "--short=12", "HEAD"))),
		"pinnedAt":      time.Now().UTC().Format("2006-01-02"),
		"publicVersion": versions["public"],
		"adminVersion":  versions["admin"],
	}

	out, _ := json.MarshalIndent(source, "", "  ")
	if err := os.WriteFile(filepath.Join("spec", "SOURCE.json"), append(out, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Printf("Pinned engine spec at %s (%s).\n", source["engineCommit"], source["publicVersion"])
}
