package session

import (
	"fmt"
	"sync"
	"time"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// ParkHint may be implemented by a Conn that should park the player instead of
// taking the stock quit path when ReadPacket fails. *minecraft.Conn does not
// implement this: vanilla RakNet still destroys the player on disconnect.
type ParkHint interface {
	// ParkOnDisconnect is consulted once ReadPacket fails. park=false keeps
	// stock save+remove. ttl is passed to HandlePark; a zero ttl means the
	// server default.
	ParkOnDisconnect() (ttl time.Duration, park bool)
}

// UUID is the player UUID attached via SetHandle. Nil UUID if unset.
func (s *Session) UUID() uuid.UUID {
	if s == nil || s.ent == nil {
		return uuid.Nil
	}
	return s.ent.UUID()
}

// Parked reports whether the session has closed its Conn but kept the player.
func (s *Session) Parked() bool {
	return s != nil && s.parked.Load()
}

// Park closes the network Conn, keeps the player in the world, suppresses the
// quit message, and notifies HandlePark. It does not remove the entity or save
// player data; TTL expiry follows the stock quit path.
func (s *Session) Park(reason string, ttl time.Duration) {
	if s == nil || s == Nop {
		return
	}
	s.enterPark(reason, ttl)
	s.CloseConnection()
}

func (s *Session) enterPark(reason string, ttl time.Duration) {
	if !s.parked.CompareAndSwap(false, true) {
		return
	}
	if s.conf.HandlePark != nil {
		s.conf.HandlePark(s, reason, ttl)
	}
}

func (s *Session) maybeParkOnDisconnect() {
	if s.parked.Load() || s.closed.Load() {
		return
	}
	h, ok := s.conn.(ParkHint)
	if !ok {
		return
	}
	ttl, park := h.ParkOnDisconnect()
	if !park {
		return
	}
	s.enterPark("conn closed", ttl)
	s.CloseConnection()
}

// WaitParkedLoops waits until packet/background goroutines started before Park
// have exited. Call it off the world owner before Rebind.
func (s *Session) WaitParkedLoops() {
	if s == nil || s == Nop {
		return
	}
	s.CloseConnection()
	s.loopWG.Wait()
}

// Rebind attaches a new Conn to a parked session. The caller must have already
// run StartGameContext on conn (the Bedrock client is new). It must not create
// a new player. Join messages are not sent. WaitParkedLoops must have returned.
func (s *Session) Rebind(conn Conn, c Controllable, tx *world.Tx) error {
	if s == nil || s == Nop {
		return fmt.Errorf("session: rebind: nil session")
	}
	if conn == nil {
		return fmt.Errorf("session: rebind: nil conn")
	}
	if !s.parked.Load() {
		return fmt.Errorf("session: rebind: session is not parked")
	}
	if tx == nil || c == nil {
		return fmt.Errorf("session: rebind: missing world transaction")
	}

	s.drainPackets()
	s.conn = conn
	s.connOnce = sync.Once{}
	s.closeBackground = make(chan struct{})

	r := conn.ChunkRadius()
	if r > int(s.maxChunkRadius) {
		r = int(s.maxChunkRadius)
		_ = conn.WritePacket(&packet.ChunkRadiusUpdated{ChunkRadius: int32(r)})
	}
	s.chunkRadius = int32(r)

	s.resetEntityViews()
	s.list.ResendTo(s)

	s.parked.Store(false)
	s.loopWG.Add(1)
	go s.writeLoop(conn)
	s.sendJoinPackets()
	s.spawn(c, tx, false)
	return nil
}

func (s *Session) drainPackets() {
	for {
		select {
		case <-s.packets:
		default:
			return
		}
	}
}

func (s *Session) resetEntityViews() {
	s.entityMutex.Lock()
	clear(s.entityRuntimeIDs)
	clear(s.entities)
	if s.ent != nil {
		s.entityRuntimeIDs[s.ent] = selfEntityRuntimeID
		s.entities[selfEntityRuntimeID] = s.ent
	}
	s.currentEntityRuntimeID = 1
	s.entityMutex.Unlock()

	s.blobMu.Lock()
	s.blobs = map[uint64][]byte{}
	s.openChunkTransactions = s.openChunkTransactions[:0]
	s.blobMu.Unlock()
}
