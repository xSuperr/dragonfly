package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/df-mc/dragonfly/server/item"
	"github.com/df-mc/dragonfly/server/player"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

func TestImportSessionSkipsStartGame(t *testing.T) {
	src, ln := startParkTestServer(t)
	id := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	first := newStubConn("ImportBot", id, false)
	ln.push(first)
	h := waitSpawned(t, src, id)
	if n := first.startGames.Load(); n != 1 {
		t.Fatalf("first join StartGameContext: got %d want 1", n)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := player.Call(ctx, h, func(_ *world.Tx, p *player.Player) (int, error) {
		return p.Inventory().AddItem(item.NewStack(item.Apple{}, 3))
	}); err != nil {
		t.Fatalf("give apples: %v", err)
	}

	snap, err := src.ExportSession(id)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if snap.SelfRuntimeID != 1 {
		t.Fatalf("self runtime ID: got %d want 1", snap.SelfRuntimeID)
	}
	if snap.UUID != id.String() {
		t.Fatalf("snapshot uuid: got %s want %s", snap.UUID, id)
	}

	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if jsonLooksLikeLiveConfig(raw) {
		t.Fatalf("snapshot JSON still looks like live player.Config pointers: %s", raw)
	}
	var round SessionSnapshot
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}

	dest, _ := startParkTestServer(t)
	second := newStubConn("ImportBot", id, false)
	if err := dest.ImportSession(second, round); err != nil {
		t.Fatalf("import: %v", err)
	}
	if n := second.startGames.Load(); n != 0 {
		t.Fatalf("import StartGameContext: got %d want 0", n)
	}
	if hasPacketType[*packet.StartGame](second) {
		t.Fatal("import wrote StartGame to the Conn")
	}
	if hasPacketType[*packet.ItemRegistry](second) {
		t.Fatal("import wrote ItemRegistry to the Conn")
	}

	h2, ok := dest.Player(id)
	if !ok {
		t.Fatal("player missing after import")
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
		t.Fatalf("apples after import: got %d want 3", n)
	}

	if first.startGames.Load() != 1 {
		t.Fatalf("first-join StartGameContext changed after import: got %d want 1", first.startGames.Load())
	}
}

func TestReleaseSessionThenImportHasNoOverlap(t *testing.T) {
	src, ln := startParkTestServer(t)
	id := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	first := newStubConn("ReleaseBot", id, false)
	ln.push(first)
	waitSpawned(t, src, id)

	snap, err := src.ExportSession(id)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if err := src.ReleaseSession(id); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, ok := src.Player(id); ok {
		t.Fatal("source still has uuid after release")
	}
	if src.Parked(id) {
		t.Fatal("release must not Park")
	}
	if hasPacketType[*packet.Disconnect](first) {
		t.Fatal("release must not write vanilla Disconnect")
	}

	dest, _ := startParkTestServer(t)
	second := newStubConn("ReleaseBot", id, false)
	if _, ok := src.Player(id); ok {
		t.Fatal("source uuid returned before dest import")
	}
	if err := dest.ImportSession(second, snap); err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, ok := src.Player(id); ok {
		t.Fatal("dual authority: source still has uuid after dest import")
	}
	if _, ok := dest.Player(id); !ok {
		t.Fatal("dest missing imported player")
	}
	if n := second.startGames.Load(); n != 0 {
		t.Fatalf("import StartGameContext: got %d want 0", n)
	}
}

func TestImportSessionRejectsDuplicateUUID(t *testing.T) {
	srv, ln := startParkTestServer(t)
	id := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	first := newStubConn("DupImport", id, false)
	ln.push(first)
	waitSpawned(t, srv, id)

	snap, err := srv.ExportSession(id)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	second := newStubConn("DupImport", id, false)
	if err := srv.ImportSession(second, snap); err == nil {
		t.Fatal("expected import of live UUID to fail")
	}
	if second.startGames.Load() != 0 {
		t.Fatalf("failed import must not StartGame, got %d", second.startGames.Load())
	}
}

func jsonLooksLikeLiveConfig(raw []byte) bool {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return true
	}
	if _, ok := m["Session"]; ok {
		return true
	}
	player, _ := m["player"].(map[string]any)
	if player == nil {
		return true
	}
	if _, ok := player["Session"]; ok {
		return true
	}
	return false
}

func hasPacketType[T packet.Packet](c *stubConn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, pk := range c.written {
		if _, ok := pk.(T); ok {
			return true
		}
	}
	return false
}
