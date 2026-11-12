package model

import "sort"

const

// Authoritative lifecycle projection from the full State. Read-only and bounded.
// dependency_gate=satisfied means ONLY that every dependency is verified: it is
// not scheduler admission (admission is always "not_evaluated"). No lease or
// busy state is inferred. Generation 0 means unknown.

(
	LifecycleMaxIDs = 8
)

func LifecycleCap(ids []string) ([]string, int, int) {
	sort.Strings(ids)
	total := len(ids)
	if total > LifecycleMaxIDs {
		ids = ids[:LifecycleMaxIDs]
	}
	return append([]string{}, ids...), total, total - len(ids)
}
