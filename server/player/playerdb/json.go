package playerdb

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/item"
	"github.com/df-mc/dragonfly/server/item/inventory"
	"github.com/df-mc/dragonfly/server/player"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/go-gl/mathgl/mgl64"
	"github.com/google/uuid"
)

func (p *Provider) fromJson(d jsonData, lookupWorld func(world.Dimension) *world.World) (player.Config, *world.World) {
	dim, _ := world.DimensionByID(int(d.Dimension))
	mode, _ := world.GameModeByID(int(d.GameMode))
	conf := player.Config{
		UUID:                uuid.MustParse(d.UUID),
		XUID:                d.XUID,
		Name:                d.Username,
		Position:            d.Position,
		Rotation:            cube.Rotation{d.Yaw, d.Pitch},
		Velocity:            d.Velocity,
		Health:              d.Health,
		MaxHealth:           d.MaxHealth,
		Food:                d.Hunger,
		FoodTick:            d.FoodTick,
		Exhaustion:          d.ExhaustionLevel,
		Saturation:          d.SaturationLevel,
		Experience:          d.Experience,
		AirSupply:           d.AirSupply,
		MaxAirSupply:        d.MaxAirSupply,
		EnchantmentSeed:     d.EnchantmentSeed,
		GameMode:            mode,
		Effects:             dataToEffects(d.Effects),
		FireTicks:           d.FireTicks,
		FallDistance:        d.FallDistance,
		Inventory:           inventory.New(36, nil),
		EnderChestInventory: inventory.New(27, nil),
		OffHand:             inventory.New(1, nil),
		Armour:              inventory.NewArmour(nil),
	}
	echest := make([]item.Stack, 27)
	decodeItems(d.EnderChestInventory, echest)
	invData := dataToInv(d.Inventory)

	for slot, stack := range invData.Items {
		_ = conf.Inventory.SetItem(slot, stack)
	}
	_ = conf.OffHand.SetItem(0, invData.OffHand)
	conf.Armour.Set(invData.Helmet, invData.Chestplate, invData.Leggings, invData.Boots)
	conf.HeldSlot = int(invData.MainHandSlot)

	for slot, stack := range echest {
		_ = conf.EnderChestInventory.SetItem(slot, stack)
	}
	return conf, lookupWorld(dim)
}

func (p *Provider) toJson(d player.Config, w *world.World) jsonData {
	dim, _ := world.DimensionID(w.Dimension())
	mode, _ := world.GameModeID(d.GameMode)
	offHand, _ := d.OffHand.Item(0)
	return jsonData{
		UUID:            d.UUID.String(),
		XUID:            d.XUID,
		Username:        d.Name,
		Position:        d.Position,
		Velocity:        d.Velocity,
		Yaw:             d.Rotation.Yaw(),
		Pitch:           d.Rotation.Pitch(),
		Health:          d.Health,
		MaxHealth:       d.MaxHealth,
		Hunger:          d.Food,
		FoodTick:        d.FoodTick,
		ExhaustionLevel: d.Exhaustion,
		SaturationLevel: d.Saturation,
		Experience:      d.Experience,
		AirSupply:       d.AirSupply,
		MaxAirSupply:    d.MaxAirSupply,
		EnchantmentSeed: d.EnchantmentSeed,
		GameMode:        uint8(mode),
		Effects:         effectsToData(d.Effects),
		FireTicks:       d.FireTicks,
		FallDistance:    d.FallDistance,
		Inventory: invToData(InventoryData{
			Items:        d.Inventory.Slots(),
			Boots:        d.Armour.Boots(),
			Leggings:     d.Armour.Leggings(),
			Chestplate:   d.Armour.Chestplate(),
			Helmet:       d.Armour.Helmet(),
			OffHand:      offHand,
			MainHandSlot: uint32(d.HeldSlot),
		}),
		EnderChestInventory: encodeItems(d.EnderChestInventory.Slots()),
		Dimension:           int32(dim),
	}
}

type jsonData struct {
	UUID                             string
	XUID                             string
	Username                         string
	Position, Velocity               mgl64.Vec3
	Yaw, Pitch                       float64
	Health, MaxHealth                float64
	Hunger                           int
	FoodTick                         int
	ExhaustionLevel, SaturationLevel float64
	EnchantmentSeed                  int64
	Experience                       int
	AirSupply, MaxAirSupply          int
	GameMode                         uint8
	Inventory                        jsonInventoryData
	EnderChestInventory              []jsonSlot
	Effects                          []jsonEffect
	FireTicks                        int64
	FallDistance                     float64
	Dimension                        int32
}

type jsonInventoryData struct {
	Items        []jsonSlot
	Boots        []byte
	Leggings     []byte
	Chestplate   []byte
	Helmet       []byte
	OffHand      []byte
	MainHandSlot uint32
}

type jsonSlot struct {
	Item []byte
	Slot int
}

type jsonEffect struct {
	ID              int
	Level           int
	Duration        time.Duration
	Ambient         bool
	ParticlesHidden bool
	Infinite        bool
}

// MarshalConfig encodes player.Config to a serialisable JSON DTO. Live
// inventory pointers, Session, Skin, and Locale are not included: inventories
// are NBT, and identity/skin are restored from the destination Conn.
func MarshalConfig(d player.Config, w *world.World) ([]byte, error) {
	if w == nil {
		return nil, fmt.Errorf("playerdb: marshal config: nil world")
	}
	return json.Marshal((&Provider{}).toJson(d, w))
}

// UnmarshalConfig decodes a MarshalConfig DTO into a fresh player.Config.
func UnmarshalConfig(b []byte, lookupWorld func(world.Dimension) *world.World) (player.Config, *world.World, error) {
	if lookupWorld == nil {
		return player.Config{}, nil, fmt.Errorf("playerdb: unmarshal config: nil world lookup")
	}
	var d jsonData
	if err := json.Unmarshal(b, &d); err != nil {
		return player.Config{}, nil, err
	}
	conf, w := (&Provider{}).fromJson(d, lookupWorld)
	return conf, w, nil
}
