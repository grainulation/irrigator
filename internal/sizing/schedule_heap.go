package sizing

import (
	"container/heap"
	"math"
	"sort"
)

type scheduleReady struct {
	ids      []int
	priority []float64
}

func (h scheduleReady) Len() int { return len(h.ids) }
func (h scheduleReady) Less(i, j int) bool {
	a, b := h.ids[i], h.ids[j]
	if h.priority[a] != h.priority[b] {
		return h.priority[a] > h.priority[b]
	}
	return a < b
}
func (h scheduleReady) Swap(i, j int) { h.ids[i], h.ids[j] = h.ids[j], h.ids[i] }
func (h *scheduleReady) Push(x any)   { h.ids = append(h.ids, x.(int)) }
func (h *scheduleReady) Pop() any     { x := h.ids[len(h.ids)-1]; h.ids = h.ids[:len(h.ids)-1]; return x }

type scheduledFinish struct {
	id int
	at float64
}
type scheduleRunning []scheduledFinish

func (h scheduleRunning) Len() int { return len(h) }
func (h scheduleRunning) Less(i, j int) bool {
	if h[i].at != h[j].at {
		return h[i].at < h[j].at
	}
	return h[i].id < h[j].id
}
func (h scheduleRunning) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *scheduleRunning) Push(x any)   { *h = append(*h, x.(scheduledFinish)) }
func (h *scheduleRunning) Pop() any     { old := *h; x := old[len(old)-1]; *h = old[:len(old)-1]; return x }

// The common independent-output case need not scan every task for every start.
// Preserve the original epsilon tie rule: if distinct priorities are within
// that epsilon, retain the original scan instead of changing heuristic order.
// Output-conflicting graphs keep their existing scheduling path as well.
func scheduleWithoutConflicts(n int, durations []float64, preds, succs, conflicts [][]int, priority []float64, slots int) (float64, bool) {
	for _, keys := range conflicts {
		if len(keys) != 0 {
			return 0, false
		}
	}
	ordered := append([]float64(nil), priority...)
	sort.Float64s(ordered)
	for i := 1; i < len(ordered); i++ {
		if ordered[i] != ordered[i-1] && ordered[i]-ordered[i-1] <= 1e-12 {
			return 0, false
		}
	}
	ready := &scheduleReady{priority: priority}
	running := &scheduleRunning{}
	remaining := make([]int, n)
	for i := range preds {
		remaining[i] = len(preds[i])
		if remaining[i] == 0 {
			heap.Push(ready, i)
		}
	}
	now, done := 0.0, 0
	for done < n {
		for running.Len() < slots && ready.Len() > 0 {
			id := heap.Pop(ready).(int)
			heap.Push(running, scheduledFinish{id: id, at: now + durations[id]})
		}
		if running.Len() == 0 {
			return math.MaxFloat64, true
		}
		finished := heap.Pop(running).(scheduledFinish)
		now = finished.at
		done++
		for _, child := range succs[finished.id] {
			remaining[child]--
			if remaining[child] == 0 {
				heap.Push(ready, child)
			}
		}
	}
	return now, true
}
