package server

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/df-mc/dragonfly/server/player"
	"github.com/df-mc/dragonfly/server/session"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/go-gl/mathgl/mgl32"
	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

const defaultParkTTL = 5 * time.Minute

type parkState struct {
	sess    *session.Session
	reason  string
	cancel  context.CancelFunc
	rebind  atomic.Bool
	expires time.Time
}

// Park closes the player's network Conn but keeps the same *player.Player in
// the world. Quit messages are suppressed. After ttl the stock save+remove
// path runs. A zero ttl uses Config.DefaultParkTTL (5 minutes if unset). A
// negative ttl disables expiry until Server.Close or an explicit Disconnect.
func (srv *Server) Park(id uuid.UUID, reason string, ttl time.Duration) error {
	h, ok := srv.Player(id)
	if !ok {
		return fmt.Errorf("park %s: player not connected", id)
	}
	if srv.Parked(id) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := player.Call(ctx, h, func(_ *world.Tx, p *player.Player) (struct{}, error) {
		p.Park(reason, ttl)
		return struct{}{}, nil
	})
	if err != nil {
		return fmt.Errorf("park %s: %w", id, err)
	}
	return nil
}

// Parked reports whether the UUID is parked awaiting Rebind.
func (srv *Server) Parked(id uuid.UUID) bool {
	srv.parkMu.Lock()
	defer srv.parkMu.Unlock()
	_, ok := srv.parked[id]
	return ok
}

// ParkMetrics returns park, rebind, and TTL-expiry counters.
func (srv *Server) ParkMetrics() (parked, rebind, expire uint64) {
	return srv.parkTotal.Load(), srv.rebindTotal.Load(), srv.parkTTLExpire.Load()
}

func (srv *Server) handlePark(s *session.Session, reason string, ttl time.Duration) {
	if s == nil {
		return
	}
	id := s.UUID()
	if id == uuid.Nil {
		return
	}
	ttl = srv.resolveParkTTL(ttl)
	ctx, cancel := context.WithCancel(context.Background())
	st := &parkState{sess: s, reason: reason, cancel: cancel}
	if ttl > 0 {
		st.expires = time.Now().Add(ttl)
	}
	srv.parkMu.Lock()
	if _, exists := srv.parked[id]; exists {
		srv.parkMu.Unlock()
		cancel()
		return
	}
	srv.parked[id] = st
	srv.parkMu.Unlock()
	srv.parkTotal.Add(1)
	srv.conf.Log.Info("player parked", "uuid", id, "reason", reason, "ttl", ttl)
	if ttl > 0 {
		go srv.watchParkTTL(id, ttl, ctx)
	}
}

func (srv *Server) resolveParkTTL(ttl time.Duration) time.Duration {
	if ttl != 0 {
		return ttl
	}
	if srv.conf.DefaultParkTTL != 0 {
		return srv.conf.DefaultParkTTL
	}
	return defaultParkTTL
}

func (srv *Server) watchParkTTL(id uuid.UUID, ttl time.Duration, ctx context.Context) {
	t := time.NewTimer(ttl)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-t.C:
		srv.expirePark(id)
	}
}

func (srv *Server) expirePark(id uuid.UUID) {
	if !srv.Parked(id) {
		return
	}
	h, ok := srv.Player(id)
	if !ok {
		srv.unpark(id)
		return
	}
	srv.parkTTLExpire.Add(1)
	srv.conf.Log.Info("park ttl expired", "uuid", id)
	player.Do(h, func(_ *world.Tx, p *player.Player) {
		p.Disconnect("park ttl expired")
	})
}

func (srv *Server) unpark(id uuid.UUID) {
	srv.parkMu.Lock()
	st, ok := srv.parked[id]
	if ok {
		delete(srv.parked, id)
	}
	srv.parkMu.Unlock()
	if ok && st.cancel != nil {
		st.cancel()
	}
}

func (srv *Server) cancelAllParks() {
	srv.parkMu.Lock()
	for id, st := range srv.parked {
		if st.cancel != nil {
			st.cancel()
		}
		delete(srv.parked, id)
	}
	srv.parkMu.Unlock()
}

func (srv *Server) rebindParked(ctx context.Context, conn session.Conn, l Listener, id uuid.UUID) {
	srv.parkMu.Lock()
	st, ok := srv.parked[id]
	if !ok || st == nil || st.sess == nil {
		srv.parkMu.Unlock()
		_ = l.Disconnect(conn, "Already logged in.")
		return
	}
	if !st.rebind.CompareAndSwap(false, true) {
		srv.parkMu.Unlock()
		_ = l.Disconnect(conn, "Already logged in.")
		return
	}
	sess := st.sess
	srv.parkMu.Unlock()

	h, ok := srv.Player(id)
	if !ok {
		st.rebind.Store(false)
		_ = l.Disconnect(conn, "Connection timeout.")
		return
	}

	type live struct {
		data player.Config
		w    *world.World
	}
	got, err := player.Call(ctx, h, func(tx *world.Tx, p *player.Player) (live, error) {
		return live{data: p.Data(), w: tx.World()}, nil
	})
	if err != nil {
		st.rebind.Store(false)
		_ = l.Disconnect(conn, "Connection timeout.")
		srv.conf.Log.Debug("rebind load: "+err.Error(), "uuid", id)
		return
	}

	sess.WaitParkedLoops()

	data := srv.defaultGameData()
	data.PlayerPosition = vec64To32(got.data.Position).Add(mgl32.Vec3{0, 1.62})
	if got.w != nil {
		dim, _ := world.DimensionID(got.w.Dimension())
		data.Dimension = int32(dim)
	}
	data.Yaw, data.Pitch = float32(got.data.Rotation.Yaw()), float32(got.data.Rotation.Pitch())
	if gm, ok := world.GameModeID(got.data.GameMode); ok {
		data.PlayerGameMode = int32(gm)
	}
	data.EmoteChatMuted = srv.conf.MuteEmoteChat

	if err := conn.StartGameContext(ctx, data); err != nil {
		st.rebind.Store(false)
		_ = l.Disconnect(conn, "Connection timeout.")
		srv.conf.Log.Debug("rebind spawn failed: "+err.Error(), "raddr", conn.RemoteAddr())
		return
	}
	_ = conn.WritePacket(&packet.ItemRegistry{Items: srv.customItems})

	_, err = player.Call(ctx, h, func(tx *world.Tx, p *player.Player) (struct{}, error) {
		if err := p.Rebind(conn); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, nil
	})
	if err != nil {
		st.rebind.Store(false)
		_ = l.Disconnect(conn, "Connection timeout.")
		srv.conf.Log.Debug("rebind attach: "+err.Error(), "uuid", id)
		return
	}
	srv.unpark(id)
	srv.rebindTotal.Add(1)
	srv.conf.Log.Info("player rebound", "uuid", id)
}
