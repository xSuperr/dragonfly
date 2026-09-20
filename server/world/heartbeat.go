package world

import (
	"context"
	"runtime/debug"
	"runtime/trace"
	"time"
)

// TickPhase identifies the part of a world tick that was most recently
// entered. It is intentionally small and stable because it is sampled by a
// watchdog while the owner may be blocked.
type TickPhase uint32

const (
	TickPhaseIdle TickPhase = iota
	TickPhaseTickStart
	TickPhaseEntities
	TickPhaseScheduledBlocks
	TickPhaseRandomTicks
	TickPhaseNeighbourUpdates
	TickPhaseRedstone
	TickPhaseChunkProcessing
	TickPhaseAutosave
	TickPhaseTransaction
)

// String returns the diagnostic name of the phase.
func (p TickPhase) String() string {
	switch p {
	case TickPhaseIdle:
		return "idle"
	case TickPhaseTickStart:
		return "tick_start"
	case TickPhaseEntities:
		return "entities"
	case TickPhaseScheduledBlocks:
		return "scheduled_blocks"
	case TickPhaseRandomTicks:
		return "random_ticks"
	case TickPhaseNeighbourUpdates:
		return "neighbour_updates"
	case TickPhaseRedstone:
		return "redstone"
	case TickPhaseChunkProcessing:
		return "chunk_processing"
	case TickPhaseAutosave:
		return "autosave"
	case TickPhaseTransaction:
		return "transaction"
	default:
		return "unknown"
	}
}

// Heartbeat is a lock-free snapshot of world execution state. The timestamp
// fields are zero only before a world has completed its bootstrap tick.
type Heartbeat struct {
	Name                    string
	CurrentTick             int64
	LastCompletedTick       int64
	LastCompletedAt         time.Time
	TickStartedAt           time.Time
	LastTickDuration        time.Duration
	Phase                   TickPhase
	OwnerTransactionStarted time.Time
	OwnerActive             bool
	QueueDepth              int
	PendingChunkRequests    int
	LoadedChunks            int
	Viewers                 int
	OwnerPanics             uint64
	Closed                  bool
	Synchronous             bool
}

func unixTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func (w *World) heartbeatWorldName() string {
	if value := w.heartbeatName.Load(); value != nil {
		if name, ok := value.(string); ok {
			return name
		}
	}
	return "World"
}

// Heartbeat returns a lock-free execution snapshot. It must not read the
// World's owner-owned maps: this method is deliberately safe to call while
// the owner is blocked in a transaction.
func (w *World) Heartbeat() Heartbeat {
	if w == nil {
		return Heartbeat{}
	}
	ownerStarted := w.heartbeatOwnerStarted.Load()
	return Heartbeat{
		Name:                    w.heartbeatWorldName(),
		CurrentTick:             w.heartbeatCurrentTick.Load(),
		LastCompletedTick:       w.heartbeatCompletedTick.Load(),
		LastCompletedAt:         unixTime(w.heartbeatLastCompleted.Load()),
		TickStartedAt:           unixTime(w.heartbeatTickStarted.Load()),
		LastTickDuration:        time.Duration(w.heartbeatLastDuration.Load()),
		Phase:                   TickPhase(w.heartbeatPhase.Load()),
		OwnerTransactionStarted: unixTime(ownerStarted),
		OwnerActive:             ownerStarted != 0,
		QueueDepth:              len(w.queue),
		PendingChunkRequests:    int(w.heartbeatPendingChunks.Load()),
		LoadedChunks:            int(w.heartbeatLoadedChunks.Load()),
		Viewers:                 int(w.heartbeatViewers.Load()),
		OwnerPanics:             w.heartbeatOwnerPanics.Load(),
		Closed:                  w.closed.Load(),
		Synchronous:             w.conf.Synchronous,
	}
}

// DebugState is an explicit diagnostic alias for Heartbeat.
func (w *World) DebugState() Heartbeat {
	return w.Heartbeat()
}

func (w *World) beginOwnerTransaction() {
	w.heartbeatOwnerStarted.Store(time.Now().UnixNano())
	w.heartbeatPhase.Store(uint32(TickPhaseTransaction))
}

func (w *World) endOwnerTransaction() {
	w.heartbeatOwnerStarted.Store(0)
	w.heartbeatPhase.Store(uint32(TickPhaseIdle))
}

func (w *World) beginTick() {
	w.heartbeatTickStarted.Store(time.Now().UnixNano())
	w.heartbeatPhase.Store(uint32(TickPhaseTickStart))
}

func (w *World) completeTick() {
	now := time.Now()
	started := unixTime(w.heartbeatTickStarted.Load())
	if !started.IsZero() {
		w.heartbeatLastDuration.Store(int64(now.Sub(started)))
	}
	current := w.heartbeatCurrentTick.Load()
	w.heartbeatCompletedTick.Store(current)
	w.heartbeatLastCompleted.Store(now.UnixNano())
	w.heartbeatPhase.Store(uint32(TickPhaseIdle))
}

func (w *World) setTickPhase(phase TickPhase) {
	w.heartbeatPhase.Store(uint32(phase))
}

func (w *World) recordOwnerPanic(kind string, value any) {
	w.heartbeatOwnerPanics.Add(1)
	w.conf.Log.Error("world owner transaction panicked",
		"world", w.heartbeatWorldName(),
		"transaction", kind,
		"panic", value,
		"stack", string(debug.Stack()),
	)
}

// tracePhase starts a runtime trace region and records the phase atomically.
// runtime/trace regions are cheap when tracing is disabled and become visible
// in the Go 1.25+ flight recorder when it is active.
func (w *World) tracePhase(phase TickPhase, name string) func() {
	previous := TickPhase(w.heartbeatPhase.Load())
	w.setTickPhase(phase)
	region := trace.StartRegion(context.Background(), name)
	return func() {
		region.End()
		w.setTickPhase(previous)
	}
}
