package session

import (
	"github.com/df-mc/dragonfly/server/world"
	"github.com/google/uuid"
)

// SnapshotView is the session-owned half of a DWS SessionSnapshot: chunk
// subscriptions, entity runtime IDs, and the self runtime ID (always 1).
// It contains no live pointers.
type SnapshotView struct {
	Chunks           []world.ChunkPos
	EntityRuntimeIDs map[uuid.UUID]uint64
	SelfRuntimeID    uint64
	ChunkRadius      int32
	Hidden           []uuid.UUID
}

// SnapshotView captures the session's client-visible mappings for ExportSession.
func (s *Session) SnapshotView() SnapshotView {
	if s == nil || s == Nop {
		return SnapshotView{SelfRuntimeID: selfEntityRuntimeID}
	}
	v := SnapshotView{
		SelfRuntimeID:    selfEntityRuntimeID,
		ChunkRadius:      s.chunkRadius,
		EntityRuntimeIDs: make(map[uuid.UUID]uint64),
	}
	if s.chunkLoader != nil {
		v.Chunks = s.chunkLoader.LoadedPositions()
	}

	s.entityMutex.RLock()
	for h, id := range s.entityRuntimeIDs {
		if h == nil {
			continue
		}
		v.EntityRuntimeIDs[h.UUID()] = id
	}
	if len(s.hiddenEntities) > 0 {
		v.Hidden = make([]uuid.UUID, 0, len(s.hiddenEntities))
		for id := range s.hiddenEntities {
			v.Hidden = append(v.Hidden, id)
		}
	}
	s.entityMutex.RUnlock()
	return v
}

// ApplySnapshotView restores client-visible mappings before Spawn on import.
// The Conn must already be spawned; this does not send StartGame.
func (s *Session) ApplySnapshotView(v SnapshotView) {
	if s == nil || s == Nop {
		return
	}
	if v.ChunkRadius > 0 {
		r := v.ChunkRadius
		if r > s.maxChunkRadius {
			r = s.maxChunkRadius
		}
		s.chunkRadius = r
	}
	s.blobMu.Lock()
	s.knownChunks = make(map[world.ChunkPos]struct{}, len(v.Chunks))
	for _, pos := range v.Chunks {
		s.knownChunks[pos] = struct{}{}
	}
	s.blobMu.Unlock()

	s.entityMutex.Lock()
	s.reservedRuntimeIDs = make(map[uuid.UUID]uint64, len(v.EntityRuntimeIDs))
	maxID := s.currentEntityRuntimeID
	for id, rid := range v.EntityRuntimeIDs {
		if rid == selfEntityRuntimeID {
			continue
		}
		s.reservedRuntimeIDs[id] = rid
		if rid > maxID {
			maxID = rid
		}
	}
	s.currentEntityRuntimeID = maxID
	if len(v.Hidden) > 0 {
		if s.hiddenEntities == nil {
			s.hiddenEntities = make(map[uuid.UUID]struct{}, len(v.Hidden))
		}
		for _, id := range v.Hidden {
			s.hiddenEntities[id] = struct{}{}
		}
	}
	s.entityMutex.Unlock()
}

func (s *Session) takeKnownChunk(pos world.ChunkPos) bool {
	if s == nil {
		return false
	}
	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	if _, ok := s.knownChunks[pos]; !ok {
		return false
	}
	delete(s.knownChunks, pos)
	return true
}
