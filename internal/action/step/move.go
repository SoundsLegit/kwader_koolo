package step

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hectorgimenez/d2go/pkg/data"
	"github.com/hectorgimenez/d2go/pkg/data/skill"
	"github.com/hectorgimenez/d2go/pkg/data/state"
	"github.com/hectorgimenez/koolo/internal/context"
	"github.com/hectorgimenez/koolo/internal/game"
	"github.com/hectorgimenez/koolo/internal/ui"
	"github.com/hectorgimenez/koolo/internal/utils"
)

const DistanceToFinishMoving = 4
const stepMonsterCheckInterval = 100 * time.Millisecond

var (
	ErrMonstersInPath  = errors.New("monsters detected in movement path")
	ErrPlayerStuck     = errors.New("player is stuck")
	ErrPlayerRoundTrip = errors.New("player round trip")
	ErrNoPath          = errors.New("path couldn't be calculated")
)

type MoveOpts struct {
	distanceOverride      *int
	stationaryMinDistance *int
	stationaryMaxDistance *int
	ignoreShrines         bool
	ignoreMonsters        bool
	ignoreItems           bool
	monsterFilters        []data.MonsterFilter
	clearPathOverride     *int
}

type MoveOption func(*MoveOpts)

// WithDistanceToFinish overrides the default DistanceToFinishMoving
func WithDistanceToFinish(distance int) MoveOption {
	return func(opts *MoveOpts) {
		opts.distanceOverride = &distance
	}
}

// WithStationaryDistance configures MoveTo to stop when within a specific range of the destination.
func WithStationaryDistance(min, max int) MoveOption {
	return func(opts *MoveOpts) {
		opts.stationaryMinDistance = &min
		opts.stationaryMaxDistance = &max
	}
}

func WithIgnoreMonsters() MoveOption {
	return func(opts *MoveOpts) {
		opts.ignoreMonsters = true
	}
}

func WithIgnoreItems() MoveOption {
	return func(opts *MoveOpts) {
		opts.ignoreItems = true
	}
}

func IgnoreShrines() MoveOption {
	return func(opts *MoveOpts) {
		opts.ignoreShrines = true
	}
}

func WithMonsterFilter(filters ...data.MonsterFilter) MoveOption {
	return func(opts *MoveOpts) {
		opts.monsterFilters = append(opts.monsterFilters, filters...)
	}
}

func WithClearPathOverride(clearPathOverride int) MoveOption {
	return func(opts *MoveOpts) {
		opts.clearPathOverride = &clearPathOverride
	}
}

func (opts MoveOpts) DistanceToFinish() *int {
	return opts.distanceOverride
}

func (opts MoveOpts) IgnoreMonsters() bool {
	return opts.ignoreMonsters
}

func (opts MoveOpts) IgnoreItems() bool {
	return opts.ignoreItems
}

func (opts MoveOpts) MonsterFilters() []data.MonsterFilter {
	return opts.monsterFilters
}

func (opts MoveOpts) ClearPathOverride() *int {
	return opts.clearPathOverride
}

func MoveTo(dest data.Position, options ...MoveOption) error {
	// Initialize options
	opts := &MoveOpts{}

	// Apply any provided options
	for _, o := range options {
		o(opts)
	}

	minDistanceToFinishMoving := DistanceToFinishMoving
	if opts.distanceOverride != nil {
		minDistanceToFinishMoving = *opts.distanceOverride
	}

	ctx := context.Get()
	isDragondin := strings.EqualFold(ctx.CharacterCfg.Character.Class, "dragondin")
	ctx.SetLastStep("MoveTo")

	opts.ignoreShrines = !ctx.CharacterCfg.Game.InteractWithShrines
	stepLastMonsterCheck := time.Time{}

	blockThreshold := 200 * time.Millisecond
	stuckThreshold := 2 * time.Second
	stuckCheckStartTime := time.Now()

	roundTripReferencePosition := ctx.Data.PlayerUnit.Position
	roundTripCheckStartTime := time.Now()
	const roundTripThreshold = 10 * time.Second
	const roundTripMaxRadius = 8

	// Adaptive movement refresh intervals based on ping
	// Adjust polling frequency based on network latency
	var walkDuration time.Duration
	if !ctx.Data.AreaData.Area.IsTown() {
		// In dungeons: faster refresh for combat
		baseMin, baseMax := 300, 400
		pingAdjustment := int(float64(ctx.Data.Game.Ping) * 0.5) // Add half ping to base
		walkDuration = utils.RandomDurationMs(baseMin+pingAdjustment, baseMax+pingAdjustment)
	} else {
		// In town: slower refresh is acceptable
		baseMin, baseMax := 500, 800
		pingAdjustment := int(float64(ctx.Data.Game.Ping) * 0.5)
		walkDuration = utils.RandomDurationMs(baseMin+pingAdjustment, baseMax+pingAdjustment)
	}

	lastRun := time.Time{}
	previousPosition := data.Position{}
	clearPathDist := ctx.CharacterCfg.Character.ClearPathDist
	overrideClearPathDist := false
	blocked := false
	obstacleBypassAttempts := 0
	const maxObstacleBypassAttempts = 3
	if opts.ClearPathOverride() != nil {
		clearPathDist = *opts.ClearPathOverride()
		overrideClearPathDist = true
	}

	startArea := ctx.Data.PlayerUnit.Area

	for {
		ctx.PauseIfNotPriority()

		// Check if a Drop request is pending and interrupt
		// the current movement early so the Drop flow can take over

		if err := interruptDropIfRequested(); err != nil {
			return err
		}
		ctx.RefreshGameData()

		// If area changed during movement, the destination is no longer valid
		// This happens during portal interactions - area transition means objective achieved
		if ctx.Data.PlayerUnit.Area != startArea {
			// Wait for collision data to be loaded for the new area before returning
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if ctx.Data.AreaData.Grid != nil &&
					ctx.Data.AreaData.Grid.CollisionGrid != nil &&
					len(ctx.Data.AreaData.Grid.CollisionGrid) > 0 {
					// Area transitioned and collision data loaded - movement objective achieved
					return nil
				}
				utils.Sleep(100)
				ctx.RefreshGameData()
			}
			// If we timeout waiting for collision data, return error
			return fmt.Errorf("area transition detected but collision data failed to load for area %s", ctx.Data.PlayerUnit.Area.Area().Name)
		}

		currentDest := dest

		//Compute distance to destination
		currentDistanceToDest := ctx.PathFinder.DistanceFromMe(currentDest)

		// Check for obstacles when teleporting and close to destination (BEFORE returning)
		if ctx.Data.CanTeleport() && currentDistanceToDest <= minDistanceToFinishMoving && obstacleBypassAttempts < maxObstacleBypassAttempts {
			isObstacle := ctx.PathFinder.IsObstacleBetween(ctx.Data.PlayerUnit.Position, currentDest)
			if isObstacle {
				obstacleBypassAttempts++
				// Move just past the position to bypass the obstacle
				ctx.Logger.Debug("Object is not walkable, moving past it",
					slog.Int("attempt", obstacleBypassAttempts),
					slog.Int("distance", currentDistanceToDest))
				movePastPos := ctx.PathFinder.GetPositionPast(currentDest)

				// Convert to grid-relative coordinates for MoveThroughPath
				areaOrigin := ctx.Data.AreaOrigin
				movePastGridPos := data.Position{
					X: movePastPos.X - areaOrigin.X,
					Y: movePastPos.Y - areaOrigin.Y,
				}

				// Update values before movement to maintain stuck detection
				lastRun = time.Now()
				if previousPosition != ctx.Data.PlayerUnit.Position {
					obstacleBypassAttempts = 0 // Reset counter when player successfully moves
				}
				previousPosition = ctx.Data.PlayerUnit.Position

				// Try to move past the obstacle
				ctx.PathFinder.MoveThroughPath([]data.Position{movePastGridPos}, walkDuration)
				utils.Sleep(100)

				// Continue to next iteration to re-evaluate position
				continue
			}
		}

		//We've reached the destination, stop movement
		if currentDistanceToDest <= minDistanceToFinishMoving {
			return nil
		} else if blocked {
			//Add tolerance to reach destination if blocked
			if currentDistanceToDest <= minDistanceToFinishMoving*2 {
				return nil
			}
		}

		//Check for Doors on path & open them
		if !ctx.Data.CanTeleport() {
			if doorFound, doorObj := ctx.PathFinder.HasDoorBetween(ctx.Data.PlayerUnit.Position, currentDest); doorFound {
				doorToOpen := *doorObj
				interactErr := error(nil)
				//Retry a few times (maggot lair slime door fix)
				for range 5 {
					if interactErr = InteractObject(doorToOpen, func() bool {
						door, found := ctx.Data.Objects.FindByID(doorToOpen.ID)
						return found && !door.Selectable
					}); interactErr == nil {
						break
					}
					ctx.PathFinder.RandomMovement()
					utils.Sleep(250)
				}
				if interactErr != nil {
					return interactErr
				}
			}
		}

		//Handle stationary distance (not sure what it refers to...)
		if opts.stationaryMinDistance != nil && opts.stationaryMaxDistance != nil {
			if currentDistanceToDest >= *opts.stationaryMinDistance && currentDistanceToDest <= *opts.stationaryMaxDistance {
				ctx.Logger.Debug(fmt.Sprintf("MoveTo: Reached stationary distance %d-%d (current %d)", *opts.stationaryMinDistance, *opts.stationaryMaxDistance, currentDistanceToDest))
				return nil
			}
		}

		//If teleporting, sleep for the cast duration
		if ctx.Data.CanTeleport() {
			if time.Since(lastRun) < ctx.Data.PlayerCastDuration() {
				time.Sleep(ctx.Data.PlayerCastDuration() - time.Since(lastRun))
				continue
			}
		}

		//Handle monsters if needed
		if !opts.ignoreMonsters && !ctx.Data.AreaData.Area.IsTown() && (!ctx.Data.CanTeleport() || overrideClearPathDist) && clearPathDist > 0 && time.Since(stepLastMonsterCheck) > stepMonsterCheckInterval {
			stepLastMonsterCheck = time.Now()
			monsterFound := false

			for _, m := range ctx.Data.Monsters.Enemies(opts.monsterFilters...) {
				if ctx.Char.ShouldIgnoreMonster(m) {
					continue
				}
				//Check distance first as it is cheaper
				distanceToMonster := ctx.PathFinder.DistanceFromMe(m.Position)
				if distanceToMonster <= clearPathDist {
					//Line of sight second
					if ctx.PathFinder.LineOfSight(ctx.Data.PlayerUnit.Position, m.Position) {
						//Finally door check as it computes path
						if hasDoorBetween, _ := ctx.PathFinder.HasDoorBetween(ctx.Data.PlayerUnit.Position, m.Position); !hasDoorBetween {
							monsterFound = true
							break
						}
					}
				}
			}

			if monsterFound {
				return ErrMonstersInPath
			}
		}

		currentPosition := ctx.Data.PlayerUnit.Position
		blocked = false
		//Detect if player is doing round trips around a position for too long and return error if it's the case
		if utils.CalculateDistance(currentPosition, roundTripReferencePosition) <= roundTripMaxRadius {
			timeInRoundtrip := time.Since(roundTripCheckStartTime)
			if timeInRoundtrip > roundTripThreshold {
				ctx.Logger.Warn("Player is doing round trips. Current area: [" + ctx.Data.PlayerUnit.Area.Area().Name + "]. Trying to path to Destination: [" + fmt.Sprintf("%d,%d", currentDest.X, currentDest.Y) + "]")
				return ErrPlayerRoundTrip
			} else if timeInRoundtrip > roundTripThreshold/2.0 {
				blocked = true
			}
		} else {
			//Player moved significantly, reset Round Trip detection
			roundTripReferencePosition = currentPosition
			roundTripCheckStartTime = time.Now()
		}

		if currentPosition == previousPosition && !ctx.Data.PlayerUnit.States.HasState(state.Stunned) {
			stuckTime := time.Since(stuckCheckStartTime)
			if stuckTime > stuckThreshold {
				//if stuck for too long, abort movement
				return ErrPlayerStuck
			} else if stuckTime > blockThreshold {
				//Detect blocked after short threshold
				blocked = true
			}
		} else {
			//Player moved, reset stuck detection timer
			stuckCheckStartTime = time.Now()
		}

		if blocked {
			//First check if there's a destructible nearby
			if obj, found := ctx.PathFinder.GetClosestDestructible(ctx.Data.PlayerUnit.Position); found {
				if !obj.Selectable {
					// Already destroyed, move on
					continue
				}
				x, y := ui.GameCoordsToScreenCords(obj.Position.X, obj.Position.Y)
				ctx.HID.Click(game.LeftButton, x, y)

				// Adaptive delay for obstacle interaction based on ping
				time.Sleep(time.Millisecond * time.Duration(utils.PingMultiplier(utils.Light, 100)))
			} else if door, found := ctx.PathFinder.GetClosestDoor(ctx.Data.PlayerUnit.Position); found {
				//There's a door really close, try to open it
				doorToOpen := *door
				InteractObject(doorToOpen, func() bool {
					door, found := ctx.Data.Objects.FindByID(door.ID)
					return found && !door.Selectable
				})
			}
		}

		//Handle skills for navigation
		if ctx.Data.CanTeleport() {
			if ctx.Data.PlayerUnit.RightSkill != skill.Teleport {
				ctx.HID.PressKeyBinding(ctx.Data.KeyBindings.MustKBForSkill(skill.Teleport))
			}
		} else if isDragondin {
			// Dragondin: keep Conviction active while moving (instead of Vigor).
			// Fallback to Vigor if Conviction isn't bound.
			if kb, found := ctx.Data.KeyBindings.KeyBindingForSkill(skill.Conviction); found {
				if ctx.Data.PlayerUnit.RightSkill != skill.Conviction {
					ctx.HID.PressKeyBinding(kb)
				}
			} else if kb, found := ctx.Data.KeyBindings.KeyBindingForSkill(skill.Vigor); found {
				if ctx.Data.PlayerUnit.RightSkill != skill.Vigor {
					ctx.HID.PressKeyBinding(kb)
				}
			}
		} else if kb, found := ctx.Data.KeyBindings.KeyBindingForSkill(skill.Vigor); found {
			if ctx.Data.PlayerUnit.RightSkill != skill.Vigor {
				ctx.HID.PressKeyBinding(kb)
			}
		}

		//Compute path to reach destination
		path, _, found := ctx.PathFinder.GetPath(currentDest)
		if !found {
			//Couldn't find path, abort movement
			ctx.Logger.Warn("path could not be calculated. Current area: [" + ctx.Data.PlayerUnit.Area.Area().Name + "]. Trying to path to Destination: [" + fmt.Sprintf("%d,%d", currentDest.X, currentDest.Y) + "]")
			return ErrNoPath
		} else if len(path) == 0 {
			//Path found but it's empty, consider movement done
			//Not sure if it can happen
			ctx.Logger.Warn("path found but it's empty: [" + ctx.Data.PlayerUnit.Area.Area().Name + "]. Trying to path to Destination: [" + fmt.Sprintf("%d,%d", currentDest.X, currentDest.Y) + "]")
			return nil
		}

		// When teleporting, validate only the next few steps ahead for obstacles
		// Checking the entire path is inefficient and unnecessary since we recalculate frequently
		if ctx.Data.CanTeleport() && len(path) > 1 {
			playerPos := ctx.Data.PlayerUnit.Position
			areaOrigin := ctx.Data.AreaOrigin

			// Only check the next 10 path segments (or less if path is shorter)
			// This is efficient and sufficient since teleport moves quickly and recalculates often
			maxSegmentsToCheck := 10
			if maxSegmentsToCheck > len(path) {
				maxSegmentsToCheck = len(path)
			}

			validPathIndex := -1
			lastValidIndex := -1

			// Start by checking if we can reach the first path point from current position
			currentCheckPos := playerPos

			for i := 0; i < maxSegmentsToCheck; i++ {
				pathPoint := path[i]
				// Convert path coordinates (grid-relative) to world coordinates
				pathWorldPos := data.Position{
					X: pathPoint.X + areaOrigin.X,
					Y: pathPoint.Y + areaOrigin.Y,
				}

				// Check if we can teleport to this point from the last valid position
				if !ctx.PathFinder.IsObstacleBetween(currentCheckPos, pathWorldPos) {
					lastValidIndex = i
					// Update reference position for next segment check
					currentCheckPos = pathWorldPos
				} else {
					// Found an obstacle in the path, stop checking further segments
					break
				}
			}

			// If we found any valid points, use the furthest one we can reach
			if lastValidIndex >= 0 {
				// Check if there are invalid points we're skipping (within our checked range)
				if lastValidIndex < maxSegmentsToCheck-1 && lastValidIndex < len(path)-1 {
					// There are blocked points after our last valid point
					// Check if we can reach the destination directly from the last valid point
					lastValidWorldPos := data.Position{
						X: path[lastValidIndex].X + areaOrigin.X,
						Y: path[lastValidIndex].Y + areaOrigin.Y,
					}

					if !ctx.PathFinder.IsObstacleBetween(lastValidWorldPos, currentDest) {
						ctx.Logger.Debug("Path has obstacles but destination is reachable, truncating path",
							slog.Int("originalPathLength", len(path)),
							slog.Int("truncatedToIndex", lastValidIndex))
						// We can reach destination from this point, truncate the path
						path = path[:lastValidIndex+1]
					} else {
						// Destination not directly reachable, skip to furthest valid point
						ctx.Logger.Debug("Skipping blocked path segments",
							slog.Int("skippedPoints", lastValidIndex),
							slog.Int("remainingPath", len(path)-lastValidIndex))
						validPathIndex = lastValidIndex
					}
				}
			} else {
				// No valid points found in the checked segments - immediate path is blocked
				ctx.Logger.Debug("Immediate path is blocked by obstacles, attempting bypass",
					slog.Int("distance", currentDistanceToDest))

				// Try to move to a position that bypasses the obstacle
				movePastPos := ctx.PathFinder.GetPositionPast(currentDest)
				// Convert to grid-relative coordinates for MoveThroughPath
				movePastGridPos := data.Position{
					X: movePastPos.X - areaOrigin.X,
					Y: movePastPos.Y - areaOrigin.Y,
				}

				// Update values before movement to maintain stuck detection
				lastRun = time.Now()
				if previousPosition != ctx.Data.PlayerUnit.Position {
					obstacleBypassAttempts = 0 // Reset counter when player successfully moves
				}
				previousPosition = ctx.Data.PlayerUnit.Position

				ctx.PathFinder.MoveThroughPath([]data.Position{movePastGridPos}, walkDuration)
				utils.Sleep(100)
				continue
			}

			// If we need to skip to a valid starting point in the path
			if validPathIndex > 0 {
				path = path[validPathIndex:]
			}
		}

		//Update values
		lastRun = time.Now()
		if previousPosition != ctx.Data.PlayerUnit.Position {
			obstacleBypassAttempts = 0 // Reset counter when player successfully moves
		}
		previousPosition = ctx.Data.PlayerUnit.Position

		//Perform the movement
		ctx.PathFinder.MoveThroughPath(path, walkDuration)
	}
}
