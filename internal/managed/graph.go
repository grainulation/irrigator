package managed

import (
	"github.com/grainulation/irrigator/internal/model"
)

func Descendants(s *model.State, id string) []string {
	result := []string{}
	seen := map[string]bool{}
	var visit func(string)
	visit = func(node string) {
		if seen[node] || s.Tasks[node] == nil {
			return
		}
		seen[node] = true
		result = append(result, node)
		for _, child := range s.Tasks[node].Children {
			visit(child)
		}
	}
	visit(id)
	return result
}
