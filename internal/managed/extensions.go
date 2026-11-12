package managed

import (
	"github.com/grainulation/irrigator/internal/model"
)

func TaskContext(p model.Plan, t model.Task) map[string]string {
	c := map[string]string{}
	for _, key := range t.Context {
		c[key] = p.Context[key]
	}
	return c
}
