package session

import (
	"slices"
	"sync"

	"github.com/df-mc/dragonfly/server/internal/sliceutil"
	"github.com/df-mc/dragonfly/server/player/skin"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// processSessions is used only when Config.List is nil (sessions created
// outside Server). Each Server owns a List so in-process World Servers do not
// mix PlayerList/skins.
var processSessions = NewList()

// List is the PlayerList namespace for one Server.
type List struct {
	mu sync.Mutex
	s  []*Session
}

// NewList returns an empty PlayerList namespace.
func NewList() *List {
	return &List{}
}

func (l *List) Add(s *Session) {
	if l == nil || s == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, other := range l.s {
		l.sendSessionTo(s, other)
		l.sendSessionTo(other, s)
	}
	l.sendSessionTo(s, s)
	l.s = append(l.s, s)
}

// ResendTo sends the player list of every session to s. Used after Rebind so a
// new client sees everyone who was already online.
func (l *List) ResendTo(s *Session) {
	if l == nil || s == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, other := range l.s {
		l.sendSessionTo(other, s)
	}
}

func (l *List) Remove(s *Session, entity world.Entity) {
	if l == nil || s == nil {
		return
	}
	l.mu.Lock()
	removedFrom := slices.Clone(l.s)
	for _, other := range l.s {
		l.unsendSessionFrom(s, other)
	}
	l.s = sliceutil.DeleteVal(l.s, s)
	l.mu.Unlock()

	if entity == nil {
		return
	}
	for _, other := range removedFrom {
		if other != nil && other.viewLayer != nil {
			other.viewLayer.Remove(entity)
		}
	}
}

func (l *List) Lookup(id uuid.UUID) (*Session, bool) {
	if l == nil {
		return nil, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if index := slices.IndexFunc(l.s, func(session *Session) bool {
		return session != nil && session.ent != nil && session.ent.UUID() == id
	}); index != -1 {
		return l.s[index], true
	}
	return nil, false
}

func (l *List) sendSessionTo(s, to *Session) {
	if s == nil || to == nil || s.ent == nil || to.entityRuntimeIDs == nil || to.Parked() {
		return
	}
	runtimeID := uint64(selfEntityRuntimeID)

	to.entityMutex.Lock()
	if s != to {
		to.currentEntityRuntimeID += 1
		runtimeID = to.currentEntityRuntimeID
	}
	to.entityRuntimeIDs[s.ent] = runtimeID
	to.entities[runtimeID] = s.ent
	to.entityMutex.Unlock()

	to.writePacket(&packet.PlayerList{
		Entries: []protocol.PlayerListEntry{{
			ActionType:     protocol.PlayerListActionAdd,
			UUID:           s.ent.UUID(),
			EntityUniqueID: int64(runtimeID),
			Username:       s.conn.IdentityData().DisplayName,
			XUID:           s.conn.IdentityData().XUID,
			BuildPlatform:  int32(protocol.DeviceUnknown),
			Skin:           skinToProtocol(s.joinSkin),
		}},
	})
}

func (l *List) unsendSessionFrom(s, from *Session) {
	if s == nil || from == nil || s.ent == nil || from.entityRuntimeIDs == nil {
		return
	}
	from.entityMutex.Lock()
	delete(from.entities, from.entityRuntimeIDs[s.ent])
	delete(from.entityRuntimeIDs, s.ent)
	from.entityMutex.Unlock()

	from.writePacket(&packet.PlayerList{
		Entries: []protocol.PlayerListEntry{{
			ActionType: protocol.PlayerListActionRemove,
			UUID:       s.ent.UUID(),
		}},
	})
}

// skinToProtocol converts a skin to its protocol representation.
func skinToProtocol(s skin.Skin) protocol.Skin {
	var animations []protocol.SkinAnimation
	for _, animation := range s.Animations {
		protocolAnim := protocol.SkinAnimation{
			ImageWidth:  uint32(animation.Bounds().Max.X),
			ImageHeight: uint32(animation.Bounds().Max.Y),
			ImageData:   animation.Pix,
			FrameCount:  float32(animation.FrameCount),
		}
		switch animation.Type() {
		case skin.AnimationHead:
			protocolAnim.AnimationType = protocol.SkinAnimationHead
		case skin.AnimationBody32x32:
			protocolAnim.AnimationType = protocol.SkinAnimationBody32x32
		case skin.AnimationBody128x128:
			protocolAnim.AnimationType = protocol.SkinAnimationBody128x128
		}
		protocolAnim.ExpressionType = uint32(animation.AnimationExpression)
		animations = append(animations, protocolAnim)
	}

	fullID := s.FullID
	if fullID == "" {
		fullID = uuid.New().String()
	}
	model := s.Model
	if len(model) == 0 {
		model = []byte("{}")
	}
	return protocol.Skin{
		PlayFabID:                 s.PlayFabID,
		SkinID:                    uuid.New().String(),
		SkinResourcePatch:         s.ModelConfig.Encode(),
		SkinImageWidth:            uint32(s.Bounds().Max.X),
		SkinImageHeight:           uint32(s.Bounds().Max.Y),
		SkinData:                  s.Pix,
		CapeImageWidth:            uint32(s.Cape.Bounds().Max.X),
		CapeImageHeight:           uint32(s.Cape.Bounds().Max.Y),
		CapeData:                  s.Cape.Pix,
		SkinGeometry:              model,
		PersonaSkin:               s.Persona,
		CapeID:                    uuid.New().String(),
		FullID:                    fullID,
		Animations:                animations,
		Trusted:                   true,
		OverrideAppearance:        true,
		GeometryDataEngineVersion: []byte(protocol.CurrentVersion),
	}
}
