package session

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestContextImportInvalidKeyNamesAndEscapesKey(t *testing.T) {
	for _, key := range []string{".github-ci", "_workflow", "bad\nkey", "bad\x1bkey"} {
		var p model.Plan
		err := checkpoint.MaterializePlanContext(&p, []string{key + "=/not-read"})
		if err == nil || !strings.Contains(err.Error(), strconv.Quote(key)) || !strings.Contains(err.Error(), "starting with a letter or digit") {
			t.Fatalf("unhelpful key diagnostic: %v", err)
		}
		if strings.ContainsAny(err.Error(), "\n\x1b") {
			t.Fatalf("unescaped key: %q", err.Error())
		}
	}
}

func TestDefaultRegistryAbsentInitializesAndMalformedNamesPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("IRRIGATOR_SESSION_REGISTRY", "")
	path, err := ResolveRegistryPath("")
	if err != nil {
		t.Fatal(err)
	}
	root, id := runfixture.V2Run(t, dir, "run")
	if err := PublishSession(path, root, id); err != nil {
		t.Fatalf("absent default registry warned: %v", err)
	}
	bad := []byte(`{"version":2,"surprise":true}`)
	if err := os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	err = PublishSession(path, root, id)
	if !errors.Is(err, errRegistryUnreadable) || !strings.Contains(err.Error(), strconv.Quote(filepath.Clean(path))) || !strings.Contains(err.Error(), "--session-registry") {
		t.Fatalf("unhelpful default registry error: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(bad) {
		t.Fatal("malformed registry modified", err)
	}
}
