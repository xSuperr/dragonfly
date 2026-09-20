package item

import (
	"github.com/df-mc/dragonfly/server/item/potion"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/sound"
	"math"
	"time"
)

// Bow is a ranged weapon that fires arrows.
type Bow struct{}

// BowMinChargeTicks is the minimum number of server ticks a bow must be
// charged before it can fire.
const BowMinChargeTicks = 3

// BowLaunchSpeed is the Bedrock/Dragonfly full-charge arrow launch speed
// multiplier applied as velocity = look * force * BowLaunchSpeed.
const BowLaunchSpeed = 5.0

// BowForce returns the vanilla bow charge force for the number of server ticks
// the bow was held. The result is capped at 1 once the bow is fully charged.
func BowForce(ticks int) float64 {
	if ticks < 0 {
		return 0
	}
	p := float64(ticks) / 20
	return math.Min((p*p+p*2)/3, 1)
}

// MaxCount always returns 1.
func (Bow) MaxCount() int {
	return 1
}

// DurabilityInfo ...
func (Bow) DurabilityInfo() DurabilityInfo {
	return DurabilityInfo{
		MaxDurability: 385,
		BrokenItem:    simpleItem(Stack{}),
	}
}

// FuelInfo ...
func (Bow) FuelInfo() FuelInfo {
	return newFuelInfo(time.Second * 10)
}

// Release ...
func (Bow) Release(releaser Releaser, tx *world.Tx, ctx *UseContext, duration time.Duration) {
	creative := releaser.GameMode().CreativeInventory()
	ticks := int(duration / (time.Second / 20))
	if ticks < BowMinChargeTicks {
		// The player must hold the bow for at least three ticks.
		return
	}

	force := BowForce(ticks)
	if force < 0.1 {
		// The force must be at least 0.1.
		return
	}

	arrow, ok := ctx.FirstFunc(func(stack Stack) bool {
		_, ok := stack.Item().(Arrow)
		return ok
	})
	if !ok && !creative {
		// No arrows in inventory and not in creative mode.
		return
	}

	var tip potion.Potion
	if !arrow.Empty() {
		// Arrow is empty if not found in the creative inventory.
		tip = arrow.Item().(Arrow).Tip
	}

	held, _ := releaser.HeldItems()
	powerLevel, punchLevel, burnDuration, consume := 0, 0, time.Duration(0), !creative
	for _, enchant := range held.Enchantments() {
		if f, ok := enchant.Type().(interface{ BurnDuration() time.Duration }); ok {
			burnDuration = f.BurnDuration()
		}
		if _, ok := enchant.Type().(interface{ KnockBackMultiplier() float64 }); ok {
			punchLevel = enchant.Level()
		}
		if _, ok := enchant.Type().(interface{ PowerDamage(int) float64 }); ok {
			powerLevel = enchant.Level()
		}
		if i, ok := enchant.Type().(interface{ ConsumesArrows() bool }); ok && !i.ConsumesArrows() {
			consume = false
		}
	}

	create := tx.World().EntityRegistry().Config().Arrow
	opts := world.EntitySpawnOpts{
		Position: eyePosition(releaser),
		Velocity: releaser.Rotation().Vec3().Mul(force * BowLaunchSpeed),
		Rotation: releaser.Rotation().Neg(),
	}
	projectile := tx.AddEntity(create(opts, world.ArrowSpawnConfig{
		Damage:              1,
		PowerLevel:          powerLevel,
		Owner:               releaser,
		Critical:            force >= 1,
		ObtainArrowOnPickup: !creative && consume,
		PunchLevel:          punchLevel,
		Tip:                 tip,
	}))
	if f, ok := projectile.(interface{ SetOnFire(duration time.Duration) }); ok {
		f.SetOnFire(burnDuration)
	}

	ctx.DamageItem(1)
	if consume {
		ctx.Consume(arrow.Grow(-arrow.Count() + 1))
	}

	tx.PlaySound(releaser.Position(), sound.BowShoot{})
}

// EnchantmentValue ...
func (Bow) EnchantmentValue() int {
	return 1
}

// Requirements returns the required items to release this item.
func (Bow) Requirements() []Stack {
	return []Stack{NewStack(Arrow{}, 1)}
}

// EncodeItem ...
func (Bow) EncodeItem() (name string, meta int16) {
	return "minecraft:bow", 0
}
