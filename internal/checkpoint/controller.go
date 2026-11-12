package checkpoint

import (
	"github.com/grainulation/irrigator/internal/model"
)

func SetControl(root, control string) error {
	return Update(root, func(s *model.State) error { s.Control = control; s.Event(control+"_requested", "", nil); return nil })
}
