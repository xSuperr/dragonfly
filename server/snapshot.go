package server

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/df-mc/dragonfly/server/player"
	"github.com/df-mc/dragonfly/server/player/playerdb"
	"github.com/df-mc/dragonfly/server/session"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/google/uuid"
)

// sessionSnapshotVersion is the DTO version for SessionSnapshot JSON.
const sessionSnapshotVersion = 1

// SessionSnapshot is a serialisable DTO of a player's session for DWS
// Export/Import. It does not contain live player.Config pointers, inventories,
// or *session.Session. Inventories are NBT inside Player JSON.
type SessionSnapshot struct {
	Version          int               `json:"version"`
	UUID             string            `json:"uuid"`
	Player           json.RawMessage   `json:"player"`
	Chunks           []world.ChunkPos  `json:"chunks"`
	EntityRuntimeIDs map[string]uint64 `json:"entity_runtime_ids"`
	SelfRuntimeID    uint64            `json:"self_runtime_id"`
	ChunkRadius      int32             `json:"chunk_radius"`
	HiddenEntities   []string          `json:"hidden_entities,omitempty"`
}

// PlayerUUID parses the snapshot UUID.
func (s SessionSnapshot) PlayerUUID() (uuid.UUID, error) {
	id, err := uuid.Parse(s.UUID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("session snapshot: uuid: %w", err)
	}
	return id, nil
}

// ExportSession captures a serialisable snapshot of a connected player.
// The caller must freeze mutations before export (DWS P4-T04). This method
// does not stop the source player or close the Conn.
func (srv *Server) ExportSession(id uuid.UUID) (SessionSnapshot, error) {
	h, ok := srv.Player(id)
	if !ok {
		return SessionSnapshot{}, fmt.Errorf("export session %s: player not connected", id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type live struct {
		data player.Config
		w    *world.World
		view session.SnapshotView
	}
	got, err := player.Call(ctx, h, func(tx *world.Tx, p *player.Player) (live, error) {
		d := p.Data()
		v := session.SnapshotView{SelfRuntimeID: 1}
		if d.Session != nil {
			v = d.Session.SnapshotView()
		}
		d.Session = nil
		return live{data: d, w: tx.World(), view: v}, nil
	})
	if err != nil {
		return SessionSnapshot{}, fmt.Errorf("export session %s: %w", id, err)
	}
	playerJSON, err := playerdb.MarshalConfig(got.data, got.w)
	if err != nil {
		return SessionSnapshot{}, fmt.Errorf("export session %s: %w", id, err)
	}
	ids := make(map[string]uint64, len(got.view.EntityRuntimeIDs))
	for entID, rid := range got.view.EntityRuntimeIDs {
		ids[entID.String()] = rid
	}
	hidden := make([]string, 0, len(got.view.Hidden))
	for _, hid := range got.view.Hidden {
		hidden = append(hidden, hid.String())
	}
	self := got.view.SelfRuntimeID
	if self == 0 {
		self = 1
	}
	return SessionSnapshot{
		Version:          sessionSnapshotVersion,
		UUID:             id.String(),
		Player:           playerJSON,
		Chunks:           got.view.Chunks,
		EntityRuntimeIDs: ids,
		SelfRuntimeID:    self,
		ChunkRadius:      got.view.ChunkRadius,
		HiddenEntities:   hidden,
	}, nil
}

// ImportSession restores a player from a SessionSnapshot onto conn without
// StartGameContext or the post-finalise ItemRegistry. The Bedrock client must
// already be spawned. Stock finaliseConn remains the first-join path.
func (srv *Server) ImportSession(conn session.Conn, snap SessionSnapshot) error {
	if conn == nil {
		return fmt.Errorf("import session: nil conn")
	}
	id, err := snap.PlayerUUID()
	if err != nil {
		return err
	}
	connID, err := uuid.Parse(conn.IdentityData().Identity)
	if err != nil {
		return fmt.Errorf("import session: conn identity: %w", err)
	}
	if connID != id {
		return fmt.Errorf("import session: conn uuid %s != snapshot %s", connID, id)
	}
	if snap.Version != 0 && snap.Version != sessionSnapshotVersion {
		return fmt.Errorf("import session %s: unsupported snapshot version %d", id, snap.Version)
	}
	if _, ok := srv.Player(id); ok {
		return fmt.Errorf("import session %s: already logged in", id)
	}
	if srv.Parked(id) {
		return fmt.Errorf("import session %s: uuid is parked; use Rebind", id)
	}

	conf, w, err := playerdb.UnmarshalConfig(snap.Player, srv.dimension)
	if err != nil {
		return fmt.Errorf("import session %s: decode player: %w", id, err)
	}
	if w == nil {
		w = srv.world
	}

	inc := srv.newIncoming(id, conn, conf, w, true)
	inc.s.ApplySnapshotView(snapshotViewFromDTO(snap))

	srv.pmu.Lock()
	if _, ok := srv.p[id]; ok {
		srv.pmu.Unlock()
		srv.pwg.Done()
		inc.s.Disconnect("Already logged in.")
		inc.s.CloseConnection()
		_ = inc.p.handle.Close()
		return fmt.Errorf("import session %s: already logged in", id)
	}
	srv.p[id] = inc.p
	srv.pmu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err = world.Call(ctx, inc.w, func(tx *world.Tx) (struct{}, error) {
		p := tx.AddEntity(inc.p.handle).(*player.Player)
		inc.s.Spawn(p, tx)
		return struct{}{}, nil
	})
	if err != nil {
		srv.pmu.Lock()
		delete(srv.p, id)
		srv.pmu.Unlock()
		srv.pwg.Done()
		_ = inc.p.handle.Close()
		inc.s.Disconnect("join failed")
		inc.s.CloseConnection()
		return fmt.Errorf("import session %s: spawn: %w", id, err)
	}
	srv.conf.Log.Info("player imported", "uuid", id)
	return nil
}

// ReleaseSession removes a connected player from the world after ExportSession
// so dest ImportSession cannot overlap a live source UUID. It does not Park,
// does not write a vanilla Disconnect packet, and leaves the Conn open so the
// Player Server can still abort-before-commit (before this call) or detach
// after dest import. After this returns, Player(id) is false.
func (srv *Server) ReleaseSession(id uuid.UUID) error {
	h, ok := srv.Player(id)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := player.Call(ctx, h, func(_ *world.Tx, p *player.Player) (struct{}, error) {
		p.ReleaseForMigration()
		return struct{}{}, nil
	})
	if err != nil {
		return fmt.Errorf("release session %s: %w", id, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := srv.Player(id); !ok && !srv.Parked(id) {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := srv.Player(id); ok || srv.Parked(id) {
		return fmt.Errorf("release session %s: uuid still present", id)
	}
	return nil
}

func snapshotViewFromDTO(snap SessionSnapshot) session.SnapshotView {
	v := session.SnapshotView{
		Chunks:           snap.Chunks,
		EntityRuntimeIDs: make(map[uuid.UUID]uint64, len(snap.EntityRuntimeIDs)),
		SelfRuntimeID:    snap.SelfRuntimeID,
		ChunkRadius:      snap.ChunkRadius,
	}
	if v.SelfRuntimeID == 0 {
		v.SelfRuntimeID = 1
	}
	for s, rid := range snap.EntityRuntimeIDs {
		id, err := uuid.Parse(s)
		if err != nil {
			continue
		}
		v.EntityRuntimeIDs[id] = rid
	}
	for _, s := range snap.HiddenEntities {
		id, err := uuid.Parse(s)
		if err != nil {
			continue
		}
		v.Hidden = append(v.Hidden, id)
	}
	return v
}
