package model

// ObserverShape is an explicit, optional, read-only capacity declaration in
// the canonical A x D x S notation (see Shape): A roots, D TOTAL levels
// including roots, S max children. It is observation metadata only: it never
// grants spawn capability, depth, tasks, providers or context.
type ObserverShape struct {
	Roots       int `json:"roots"`
	Levels      int `json:"levels"`
	MaxChildren int `json:"max_children"`
}
