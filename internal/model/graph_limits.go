package model

// MaxGraphTasks is the supported graph/execution count ceiling. Worker slots,
// provider limits, depth and byte bounds remain separate admission controls.
const MaxGraphTasks = 10_000

// DefaultGraphTasks preserves the omitted max_tasks behavior of existing plans.
const DefaultGraphTasks = 128
