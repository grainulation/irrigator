package checkpoint

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDecodeStateSharesFileCheckpointValidation(t *testing.T) {
	root := fixture(t, task("worker", 0))
	raw, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	fromFile, err := ReadState(root)
	if err != nil {
		t.Fatal(err)
	}
	fromBytes, err := DecodeState(root, raw)
	if err != nil || !reflect.DeepEqual(fromFile, fromBytes) {
		t.Fatalf("decode changed checkpoint behavior: %v", err)
	}
	changed := strings.Replace(string(raw), `"prompt": "worker"`, `"prompt": "changed"`, 1)
	if changed == string(raw) {
		// The fixture's wording is private to its helper; change the plan name,
		// whose exact bytes also belong to the plan hash.
		changed = strings.Replace(string(raw), `"name": "test"`, `"name": "changed"`, 1)
	}
	if changed == string(raw) {
		t.Fatal("fixture did not expose the hashed plan")
	}
	if _, err := DecodeState(root, []byte(changed)); err == nil {
		t.Fatal("decode accepted changed immutable plan bytes")
	}
}
