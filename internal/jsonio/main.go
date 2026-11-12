package jsonio

import (
	"encoding/json"
	"os"
)

func PrintJSON(v any) error {
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	return e.Encode(v)
}
