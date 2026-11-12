package checkpoint

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

func TestPlanContextImportRejectsUnsafeFiles(t *testing.T) {
	plan, source, _ := contextPlanFixture(t)
	dir := filepath.Dir(plan)
	for _, kind := range []string{"symlink", "fifo", "directory", "binary", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(dir, kind)
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(source, path)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			case "directory":
				err = os.Mkdir(path, 0700)
			case "binary":
				err = os.WriteFile(path, []byte{0xff, 0}, 0600)
			case "oversize":
				err = os.WriteFile(path, bytes.Repeat([]byte("x"), contextImportLimit+1), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := LoadPlanWithContext(plan, []string{"source=" + path}); err == nil {
				t.Fatal("accepted unsafe file")
			}
		})
	}
	for _, spec := range []string{"source", "=x", "source=", "bad key=" + source} {
		if _, err := LoadPlanWithContext(plan, []string{spec}); err == nil {
			t.Fatal("accepted invalid spec", spec)
		}
	}
}

func TestPlanContextImportBoundsFinalPlan(t *testing.T) {
	p := model.Plan{Context: map[string]string{"existing": strings.Repeat("x", 8<<20)}}
	file := filepath.Join(t.TempDir(), "extra.txt")
	os.WriteFile(file, []byte("extra"), 0600)
	if err := MaterializePlanContext(&p, []string{"extra=" + file}); err == nil {
		t.Fatal("accepted oversized materialization")
	}
}
