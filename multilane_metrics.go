package main

import "sync"

// MultiLaneSnapshot is a value-free count of lane throughput, conflicts, and
// waits. It never carries paths, prompts, models, resource identifiers, or
// payloads, and it never authorizes model work.
type MultiLaneSnapshot struct {
	Throughput uint64
	Conflicts  uint64
	Waits      uint64
}

// MultiLaneMetrics is a counts-only observer for parallel write lanes. Adds
// accept magnitudes only; they trigger no engine, prompt, or dispatch work.
type MultiLaneMetrics struct {
	mu         sync.Mutex
	throughput uint64
	conflicts  uint64
	waits      uint64
}

func NewMultiLaneMetrics() *MultiLaneMetrics {
	return &MultiLaneMetrics{}
}

func (m *MultiLaneMetrics) AddThroughput(n uint64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.throughput += n
	m.mu.Unlock()
}

func (m *MultiLaneMetrics) AddConflicts(n uint64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.conflicts += n
	m.mu.Unlock()
}

func (m *MultiLaneMetrics) AddWaits(n uint64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.waits += n
	m.mu.Unlock()
}

func (m *MultiLaneMetrics) Snapshot() MultiLaneSnapshot {
	if m == nil {
		return MultiLaneSnapshot{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return MultiLaneSnapshot{
		Throughput: m.throughput,
		Conflicts:  m.conflicts,
		Waits:      m.waits,
	}
}

// MultiLaneTriggersModelWork is the closed policy gate for this core: lane
// counters never select a model, never build a prompt, and never enqueue
// engine work.
func MultiLaneTriggersModelWork(MultiLaneSnapshot) bool {
	return false
}
