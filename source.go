package meridian

import (
	_ "embed"
	"encoding/json"
)

//go:embed spec/SOURCE.json
var sourceJSON []byte

// EngineSpecCommit is the engine commit whose OpenAPI documents this SDK's types were checked
// against. Support asks for it.
func EngineSpecCommit() string {
	var source struct {
		EngineCommit string `json:"engineCommit"`
	}

	_ = json.Unmarshal(sourceJSON, &source)

	return source.EngineCommit
}
