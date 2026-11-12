package launch

import (
	"os"

	"github.com/grainulation/irrigator/internal/localfs"
)

func OpenContextFile(name string, flags int, perm os.FileMode) (*os.File, error) {
	return localfs.OpenContextFile(name, flags, perm)
}
