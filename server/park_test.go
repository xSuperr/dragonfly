package server

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/df-mc/dragonfly/server/item"
	"github.com/df-mc/dragonfly/server/player"
	"github.com/df-mc/dragonfly/server/session"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

func TestFinaliseConnFirstJoinStartGames(t *testing.T) {
	srv, ln := startParkTestServer(t)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	c := newStubConn("JoinBot", id, false)
	ln.push(c)
	waitSpawned(t, srv, id)
	if n := c.startGames.Load(); n != 1 {
		t.Fatalf("StartGameContext calls: got %d want 1", n)
	}
}

func TestUnparkedDuplicateUUIDKicked(t *testing.T) {
	srv, ln := startParkTestServer(t)
	id := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	first := newStubConn("DupBot", id, false)
	ln.push(first)
	waitSpawned(t, srv, id)

	second := newStubConn("DupBot", id, false)
	ln.push(second)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if reason := ln.disconnectReason(second); reason == "Already logged in." {
			if n := second.startGames.Load(); n < 1 {
				t.Fatal("stock path must still StartGame before the duplicate check")
			}
			if srv.Parked(id) {
				t.Fatal("unparked duplicate must not park")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("duplicate not kicked, last reason %q", ln.disconnectReason(second))
}

func TestParkRebindSamePlayer(t *testing.T) {
	srv, ln := startParkTestServer(t)
	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	first := newStubConn("ParkBot", id, false)
	ln.push(first)
	h1 := waitSpawned(t, srv, id)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := player.Call(ctx, h1, func(_ *world.Tx, p *player.Player) (int, error) {
		return p.Inventory().AddItem(item.NewStack(item.Apple{}, 3))
	}); err != nil {
		t.Fatalf("give apples: %v", err)
	}

	if err := srv.Park(id, "stream lost", time.Minute); err != nil {
		t.Fatalf("park: %v", err)
	}
	if !srv.Parked(id) {
		t.Fatal("expected parked")
	}
	hParked, ok := srv.Player(id)
	if !ok || hParked != h1 {
		t.Fatal("parked player must remain on the server")
	}

	second := newStubConn("ParkBot", id, false)
	ln.push(second)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !srv.Parked(id) && second.startGames.Load() >= 1 {
			break
		}
		if reason := ln.disconnectReason(second); reason == "Already logged in." {
			t.Fatal("parked UUID must not be kicked as already logged in")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if srv.Parked(id) {
		t.Fatal("still parked after rebind")
	}
	if second.startGames.Load() < 1 {
		t.Fatal("rebind must StartGameContext on the new Conn")
	}
	h2, ok := srv.Player(id)
	if !ok {
		t.Fatal("player missing after rebind")
	}
	if h2 != h1 {
		t.Fatal("rebind created a new player object")
	}
	n, err := player.Call(ctx, h2, func(_ *world.Tx, p *player.Player) (int, error) {
		var apples int
		for _, it := range p.Inventory().Items() {
			if _, ok := it.Item().(item.Apple); ok {
				apples += it.Count()
			}
		}
		return apples, nil
	})
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if n != 3 {
		t.Fatalf("apples after rebind: got %d want 3", n)
	}
}

func TestParkTTLExpiresToQuit(t *testing.T) {
	srv, ln := startParkTestServer(t)
	id := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	c := newStubConn("TTLBot", id, false)
	ln.push(c)
	waitSpawned(t, srv, id)
	if err := srv.Park(id, "ttl test", 200*time.Millisecond); err != nil {
		t.Fatalf("park: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := srv.Player(id); !ok && !srv.Parked(id) {
			_, _, expire := srv.ParkMetrics()
			if expire < 1 {
				t.Fatal("ttl expire counter not incremented")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("parked player was not removed after TTL")
}

func TestParkOnDisconnectHint(t *testing.T) {
	srv, ln := startParkTestServer(t)
	id := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	c := newStubConn("HintBot", id, true)
	ln.push(c)
	waitSpawned(t, srv, id)
	_ = c.Close()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if srv.Parked(id) {
			if _, ok := srv.Player(id); !ok {
				t.Fatal("auto-park removed the player")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("ParkHint Conn close did not park")
}

func startParkTestServer(t *testing.T) (*Server, *feedListener) {
	t.Helper()
	ln := newFeedListener()
	conf := Config{
		Log:                     slog.New(slog.DiscardHandler),
		DisableResourceBuilding: true,
		DefaultParkTTL:          time.Minute,
		Listeners: []func(Config) (Listener, error){
			func(Config) (Listener, error) { return ln, nil },
		},
	}
	srv := conf.New()
	srv.Listen()
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for range srv.Accept() {
		}
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		select {
		case <-acceptDone:
		case <-time.After(10 * time.Second):
			t.Error("Accept iterator did not end after Close")
		}
	})
	return srv, ln
}

func waitSpawned(t *testing.T, srv *Server, id uuid.UUID) *world.EntityHandle {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var h *world.EntityHandle
	for time.Now().Before(deadline) {
		var ok bool
		h, ok = srv.Player(id)
		if ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if h == nil {
		t.Fatalf("player %s never joined", id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := player.Call(ctx, h, func(*world.Tx, *player.Player) (struct{}, error) {
		return struct{}{}, nil
	}); err != nil {
		t.Fatalf("wait spawned: %v", err)
	}
	return h
}

type feedListener struct {
	mu           sync.Mutex
	ch           chan session.Conn
	closed       chan struct{}
	closeOnce    sync.Once
	disconnected map[session.Conn]string
}

func newFeedListener() *feedListener {
	return &feedListener{
		ch:           make(chan session.Conn, 8),
		closed:       make(chan struct{}),
		disconnected: make(map[session.Conn]string),
	}
}

func (l *feedListener) push(c session.Conn) {
	select {
	case l.ch <- c:
	case <-l.closed:
	}
}

func (l *feedListener) Accept() (session.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *feedListener) Disconnect(conn session.Conn, reason string) error {
	l.mu.Lock()
	l.disconnected[conn] = reason
	l.mu.Unlock()
	return conn.Close()
}

func (l *feedListener) disconnectReason(conn session.Conn) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.disconnected[conn]
}

func (l *feedListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

type stubConn struct {
	identity   login.IdentityData
	client     login.ClientData
	addr       net.Addr
	closed     chan struct{}
	closeOnce  sync.Once
	inbound    chan packet.Packet
	startGames atomic.Int32
	autoPark   bool
}

func newStubConn(name string, id uuid.UUID, autoPark bool) *stubConn {
	pix := make([]byte, 64*64*4)
	return &stubConn{
		identity: login.IdentityData{
			Identity:    id.String(),
			DisplayName: name,
			XUID:        "0",
		},
		client: login.ClientData{
			LanguageCode:   "en_US",
			SkinImageWidth: 64, SkinImageHeight: 64,
			SkinData: base64.StdEncoding.EncodeToString(pix),
		},
		addr:     &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 19132},
		closed:   make(chan struct{}),
		inbound:  make(chan packet.Packet, 16),
		autoPark: autoPark,
	}
}

func (c *stubConn) IdentityData() login.IdentityData { return c.identity }
func (c *stubConn) ClientData() login.ClientData     { return c.client }
func (c *stubConn) ClientCacheEnabled() bool         { return false }
func (c *stubConn) ChunkRadius() int                 { return 8 }
func (c *stubConn) Latency() time.Duration           { return 0 }
func (c *stubConn) Flush() error                     { return nil }
func (c *stubConn) RemoteAddr() net.Addr             { return c.addr }
func (c *stubConn) WritePacket(packet.Packet) error  { return nil }
func (c *stubConn) StartGameContext(context.Context, minecraft.GameData) error {
	c.startGames.Add(1)
	return nil
}
func (c *stubConn) ReadPacket() (packet.Packet, error) {
	select {
	case <-c.closed:
		return nil, net.ErrClosed
	case pk := <-c.inbound:
		return pk, nil
	}
}
func (c *stubConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}
func (c *stubConn) ParkOnDisconnect() (time.Duration, bool) {
	if c.autoPark {
		return time.Minute, true
	}
	return 0, false
}

var (
	_ session.Conn     = (*stubConn)(nil)
	_ session.ParkHint = (*stubConn)(nil)
	_ io.Closer        = (*stubConn)(nil)
)
