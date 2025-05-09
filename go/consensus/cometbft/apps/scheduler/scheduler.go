package scheduler

import (
	"bytes"
	"crypto"
	"fmt"
	"math/rand"
	"sort"

	"github.com/cometbft/cometbft/abci/types"

	beacon "github.com/oasisprotocol/oasis-core/go/beacon/api"
	"github.com/oasisprotocol/oasis-core/go/common"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/drbg"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/mathrand"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/signature"
	"github.com/oasisprotocol/oasis-core/go/common/logging"
	"github.com/oasisprotocol/oasis-core/go/common/node"
	"github.com/oasisprotocol/oasis-core/go/common/quantity"
	"github.com/oasisprotocol/oasis-core/go/consensus/api/transaction"
	"github.com/oasisprotocol/oasis-core/go/consensus/cometbft/api"
	beaconapp "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/beacon"
	beaconState "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/beacon/state"
	governanceApi "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/governance/api"
	registryapp "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/registry"
	registryState "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/registry/state"
	schedulerApi "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/scheduler/api"
	schedulerState "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/scheduler/state"
	stakingapp "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/staking"
	stakingState "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/staking/state"
	registry "github.com/oasisprotocol/oasis-core/go/registry/api"
	scheduler "github.com/oasisprotocol/oasis-core/go/scheduler/api"
	staking "github.com/oasisprotocol/oasis-core/go/staking/api"
)

var (
	RNGContextExecutor   = []byte("EkS-ABCI-Compute")
	RNGContextValidators = []byte("EkS-ABCI-Validators")
	RNGContextEntities   = []byte("EkS-ABCI-Entities")

	RNGContextRoleWorker       = []byte("Worker")
	RNGContextRoleBackupWorker = []byte("Backup-Worker")
)

// Application is a scheduler application.
type Application struct {
	state api.ApplicationState
	md    api.MessageDispatcher
}

// New constructs a new scheduler application.
func New(state api.ApplicationState, md api.MessageDispatcher) *Application {
	return &Application{
		state: state,
		md:    md,
	}
}

// Name implements api.Application.
func (app *Application) Name() string {
	return AppName
}

// ID implements api.Application.
func (app *Application) ID() uint8 {
	return AppID
}

// Methods implements api.Application.
func (app *Application) Methods() []transaction.MethodName {
	return nil
}

// Blessed implements api.Application.
func (app *Application) Blessed() bool {
	return true
}

// Dependencies implements api.Application.
func (app *Application) Dependencies() []string {
	return []string{beaconapp.AppName, registryapp.AppName, stakingapp.AppName}
}

// Subscribe implements api.Application.
func (app *Application) Subscribe() {
	// Subscribe to messages emitted by other apps.
	app.md.Subscribe(governanceApi.MessageChangeParameters, app)
	app.md.Subscribe(governanceApi.MessageValidateParameterChanges, app)
}

// OnCleanup implements api.Application.
func (app *Application) OnCleanup() {}

// BeginBlock implements api.Application.
func (app *Application) BeginBlock(ctx *api.Context) error {
	// Check if any stake slashing has occurred in the staking layer.
	// NOTE: This will NOT trigger for any slashing that happens as part of
	//       any transactions being submitted to the chain.
	slashed := ctx.HasEvent(stakingapp.AppName, &staking.TakeEscrowEvent{})
	// Check if epoch has changed.
	// TODO: We'll later have this for each type of committee.
	epochChanged, epoch := app.state.EpochChanged(ctx)

	if epochChanged || slashed {
		// Notify applications that we are going to schedule committees.
		_, err := app.md.Publish(ctx, schedulerApi.MessageBeforeSchedule, epoch)
		if err != nil {
			return fmt.Errorf("cometbft/scheduler: before schedule notification failed: %w", err)
		}

		// The 0th epoch will not have suitable entropy for elections, nor
		// will it have useful node registrations.
		baseEpoch, err := app.state.GetBaseEpoch()
		if err != nil {
			return fmt.Errorf("cometbft/scheduler: couldn't get base epoch: %w", err)
		}

		if epoch == baseEpoch {
			ctx.Logger().Info("system in bootstrap period, skipping election",
				"epoch", epoch,
			)
			return nil
		}

		state := schedulerState.NewMutableState(ctx.State())
		params, err := state.ConsensusParameters(ctx)
		if err != nil {
			ctx.Logger().Error("failed to fetch consensus parameters",
				"err", err,
			)
			return err
		}

		beaconState := beaconState.NewMutableState(ctx.State())
		beaconParameters, err := beaconState.ConsensusParameters(ctx)
		if err != nil {
			return fmt.Errorf("cometbft/scheduler: couldn't get beacon parameters: %w", err)
		}
		// If weak alphas are allowed then skip the eligibility check as
		// well because the byzantine node and associated tests are extremely
		// fragile, and breaks in hard-to-debug ways if timekeeping isn't
		// exactly how it expects.
		filterCommitteeNodes := beaconParameters.Backend == beacon.BackendVRF && !params.DebugAllowWeakAlpha

		regState := registryState.NewMutableState(ctx.State())
		registryParameters, err := regState.ConsensusParameters(ctx)
		if err != nil {
			return fmt.Errorf("cometbft/scheduler: couldn't get registry parameters: %w", err)
		}
		runtimes, err := regState.Runtimes(ctx)
		if err != nil {
			return fmt.Errorf("cometbft/scheduler: couldn't get runtimes: %w", err)
		}
		allNodes, err := regState.Nodes(ctx)
		if err != nil {
			return fmt.Errorf("cometbft/scheduler: couldn't get nodes: %w", err)
		}

		// Filter nodes.
		var (
			nodes          []*node.Node
			committeeNodes []*nodeWithStatus
		)
		for _, node := range allNodes {
			var status *registry.NodeStatus
			status, err = regState.NodeStatus(ctx, node.ID)
			if err != nil {
				return fmt.Errorf("cometbft/scheduler: couldn't get node status: %w", err)
			}

			// Nodes which are currently frozen cannot be scheduled.
			if status.IsFrozen() {
				continue
			}
			// Expired nodes cannot be scheduled (nodes can be expired and not yet removed).
			if node.IsExpired(uint64(epoch)) {
				continue
			}

			nodes = append(nodes, node)
			if !filterCommitteeNodes || (status.ElectionEligibleAfter != beacon.EpochInvalid && epoch > status.ElectionEligibleAfter) {
				committeeNodes = append(committeeNodes, &nodeWithStatus{node, status})
			}
		}

		var stakeAcc *stakingState.StakeAccumulatorCache
		if !params.DebugBypassStake {
			stakeAcc, err = stakingState.NewStakeAccumulatorCache(ctx)
			if err != nil {
				return fmt.Errorf("cometbft/scheduler: failed to create stake accumulator cache: %w", err)
			}
			defer stakeAcc.Discard()
		}

		var entitiesEligibleForReward map[staking.Address]bool
		if epochChanged {
			// For elections on epoch changes, distribute rewards to entities with any eligible nodes.
			entitiesEligibleForReward = make(map[staking.Address]bool)
		}

		// Handle the validator election first, because no consensus is
		// catastrophic, while failing to elect other committees is not.
		var validatorEntities map[staking.Address]bool
		if validatorEntities, err = app.electValidators(
			ctx,
			app.state,
			beaconState,
			beaconParameters,
			stakeAcc,
			entitiesEligibleForReward,
			nodes,
			params,
		); err != nil {
			// It is unclear what the behavior should be if the validator
			// election fails.  The system can not ensure integrity, so
			// presumably manual intervention is required...
			return fmt.Errorf("cometbft/scheduler: couldn't elect validators: %w", err)
		}

		kinds := []scheduler.CommitteeKind{
			scheduler.KindComputeExecutor,
		}
		for _, kind := range kinds {
			if err = app.electAllCommittees(
				ctx,
				params,
				beaconState,
				beaconParameters,
				registryParameters,
				stakeAcc,
				entitiesEligibleForReward,
				validatorEntities,
				runtimes,
				committeeNodes,
				kind,
			); err != nil {
				return fmt.Errorf("cometbft/scheduler: couldn't elect %s committees: %w", kind, err)
			}
		}
		ctx.EmitEvent(api.NewEventBuilder(app.Name()).TypedAttribute(&scheduler.ElectedEvent{Kinds: kinds}))

		var kindNames []string
		for _, kind := range kinds {
			kindNames = append(kindNames, kind.String())
		}
		var runtimeIDs []string
		for _, rt := range runtimes {
			runtimeIDs = append(runtimeIDs, rt.ID.String())
		}
		ctx.Logger().Debug("finished electing committees",
			"epoch", epoch,
			"kinds", kindNames,
			"runtimes", runtimeIDs,
		)

		if entitiesEligibleForReward != nil {
			accountAddrs := stakingAddressMapToSortedSlice(entitiesEligibleForReward)
			stakingSt := stakingState.NewMutableState(ctx.State())
			if err = stakingSt.AddRewards(ctx, epoch, &params.RewardFactorEpochElectionAny, accountAddrs); err != nil {
				return fmt.Errorf("cometbft/scheduler: failed to add rewards: %w", err)
			}
		}
	}
	return nil
}

// ExecuteMessage implements api.MessageSubscriber.
func (app *Application) ExecuteMessage(ctx *api.Context, kind, msg any) (any, error) {
	switch kind {
	case governanceApi.MessageValidateParameterChanges:
		// A change parameters proposal is about to be submitted. Validate changes.
		return app.changeParameters(ctx, msg, false)
	case governanceApi.MessageChangeParameters:
		// A change parameters proposal has just been accepted and closed. Validate and apply
		// changes.
		return app.changeParameters(ctx, msg, true)
	default:
		return nil, fmt.Errorf("cometbft/scheduler: unexpected message")
	}
}

// ExecuteTx implements api.Application.
func (app *Application) ExecuteTx(*api.Context, *transaction.Transaction) error {
	return fmt.Errorf("cometbft/scheduler: unexpected transaction")
}

func diffValidators(logger *logging.Logger, current, pending map[signature.PublicKey]*scheduler.Validator) []types.ValidatorUpdate {
	var updates []types.ValidatorUpdate
	for v := range current {
		if _, ok := pending[v]; !ok {
			// Existing validator is not part of the new set, reduce its
			// voting power to 0, to indicate removal.
			logger.Debug("removing existing validator from validator set",
				"id", v,
			)
			updates = append(updates, api.PublicKeyToValidatorUpdate(v, 0))
		}
	}

	for v, new := range pending {
		if curr, ok := current[v]; ok && curr.VotingPower == new.VotingPower {
			logger.Debug("keeping existing validator in the validator set",
				"id", v,
			)
			continue
		}
		// We're adding this validator or changing its power.
		logger.Debug("upserting validator to validator set",
			"id", v,
			"power", new.VotingPower,
		)
		updates = append(updates, api.PublicKeyToValidatorUpdate(v, new.VotingPower))
	}
	return updates
}

// EndBlock implements api.Application.
func (app *Application) EndBlock(ctx *api.Context) (types.ResponseEndBlock, error) {
	var resp types.ResponseEndBlock

	state := schedulerState.NewMutableState(ctx.State())
	pendingValidators, err := state.PendingValidators(ctx)
	if err != nil {
		return resp, fmt.Errorf("cometbft/scheduler: failed to query pending validators: %w", err)
	}
	if pendingValidators == nil {
		// No validator updates to apply.
		return resp, nil
	}

	currentValidators, err := state.CurrentValidators(ctx)
	if err != nil {
		return resp, fmt.Errorf("cometbft/scheduler: failed to query current validators: %w", err)
	}

	// Clear out the pending validator update.
	if err = state.PutPendingValidators(ctx, nil); err != nil {
		return resp, fmt.Errorf("cometbft/scheduler: failed to clear validators: %w", err)
	}

	// CometBFT expects a vector of ValidatorUpdate that expresses
	// the difference between the current validator set (tracked manually
	// from InitChain), and the new validator set, which is a huge pain
	// in the ass.

	resp.ValidatorUpdates = diffValidators(ctx.Logger(), currentValidators, pendingValidators)

	// Stash the updated validator set.
	if err = state.PutCurrentValidators(ctx, pendingValidators); err != nil {
		return resp, fmt.Errorf("cometbft/scheduler: failed to set validators: %w", err)
	}

	return resp, nil
}

func (app *Application) isSuitableExecutorWorker(
	ctx *api.Context,
	n *nodeWithStatus,
	rt *registry.Runtime,
	epoch beacon.EpochTime,
	registryParams *registry.ConsensusParameters,
) bool {
	if !n.node.HasRoles(node.RoleComputeWorker) {
		return false
	}

	activeDeployment := rt.ActiveDeployment(epoch)
	if activeDeployment == nil {
		return false
	}

	for _, nrt := range n.node.Runtimes {
		if !nrt.ID.Equal(&rt.ID) {
			continue
		}

		if nrt.Version.ToU64() != activeDeployment.Version.ToU64() {
			continue
		}
		if n.status.IsSuspended(rt.ID, epoch) {
			return false
		}
		switch rt.TEEHardware {
		case node.TEEHardwareInvalid:
			if nrt.Capabilities.TEE != nil {
				return false
			}
			return true
		default:
			if nrt.Capabilities.TEE == nil {
				return false
			}
			if nrt.Capabilities.TEE.Hardware != rt.TEEHardware {
				return false
			}
			if err := nrt.Capabilities.TEE.Verify(
				registryParams.TEEFeatures,
				ctx.Now(),
				uint64(ctx.BlockHeight()),
				activeDeployment.TEE,
				n.node.ID,
			); err != nil {
				ctx.Logger().Warn("failed to verify node TEE attestation",
					"err", err,
					"node_id", n.node.ID,
					"timestamp", ctx.Now(),
					"runtime", rt.ID,
				)
				return false
			}
			return true
		}
	}
	return false
}

// GetPerm generates a permutation that we use to choose nodes from a list of eligible nodes to elect.
func GetPerm(beacon []byte, runtimeID common.Namespace, rngCtx []byte, nrNodes int) ([]int, error) {
	drbg, err := drbg.New(crypto.SHA512, beacon, runtimeID[:], rngCtx)
	if err != nil {
		return nil, fmt.Errorf("cometbft/scheduler: couldn't instantiate DRBG: %w", err)
	}
	rng := rand.New(mathrand.New(drbg))
	return rng.Perm(nrNodes), nil
}

// Operates on consensus connection.
func (app *Application) electAllCommittees(
	ctx *api.Context,
	schedulerParameters *scheduler.ConsensusParameters,
	beaconState *beaconState.MutableState,
	beaconParameters *beacon.ConsensusParameters,
	registryParameters *registry.ConsensusParameters,
	stakeAcc *stakingState.StakeAccumulatorCache,
	entitiesEligibleForReward map[staking.Address]bool,
	validatorEntities map[staking.Address]bool,
	runtimes []*registry.Runtime,
	nodeList []*nodeWithStatus,
	kind scheduler.CommitteeKind,
) error {
	for _, runtime := range runtimes {
		if err := app.electCommittee(
			ctx,
			schedulerParameters,
			beaconState,
			beaconParameters,
			registryParameters,
			stakeAcc,
			entitiesEligibleForReward,
			validatorEntities,
			runtime,
			nodeList,
			kind,
		); err != nil {
			return err
		}
	}
	return nil
}

func (app *Application) electValidators(
	ctx *api.Context,
	appState api.ApplicationQueryState,
	beaconState *beaconState.MutableState,
	beaconParameters *beacon.ConsensusParameters,
	stakeAcc *stakingState.StakeAccumulatorCache,
	entitiesEligibleForReward map[staking.Address]bool,
	nodes []*node.Node,
	params *scheduler.ConsensusParameters,
) (map[staking.Address]bool, error) {
	// Filter the node list based on eligibility and minimum required
	// entity stake.
	var nodeList []*node.Node
	entities := make(map[staking.Address]bool)
	for _, n := range nodes {
		if !n.HasRoles(node.RoleValidator) {
			continue
		}
		entAddr := staking.NewAddress(n.EntityID)
		if stakeAcc != nil {
			if err := stakeAcc.CheckStakeClaims(entAddr); err != nil {
				continue
			}
		}
		nodeList = append(nodeList, n)
		entities[entAddr] = true
	}

	// Sort all of the entities that are actually running eligible validator
	// nodes by descending stake.
	weakEntropy, err := beaconState.Beacon(ctx)
	if err != nil {
		return nil, fmt.Errorf("cometbft/scheduler: couldn't get beacon: %w", err)
	}
	sortedEntities, err := stakingAddressMapToSliceByStake(entities, stakeAcc, weakEntropy)
	if err != nil {
		return nil, err
	}

	// Shuffle the node list.
	shuffledNodes, err := shuffleValidators(ctx, appState, params, beaconState, beaconParameters, nodeList)
	if err != nil {
		return nil, err
	}

	// Gather all the entities nodes.  If the entity has more than one node,
	// ordering will be deterministically random due to the shuffle.
	entityNodesMap := make(map[staking.Address][]*node.Node)
	for i := range shuffledNodes {
		n := shuffledNodes[i] // This is due to the use of append.

		entityAddress := staking.NewAddress(n.EntityID)
		entityNodes := entityNodesMap[entityAddress]
		entityNodes = append(entityNodes, n)
		entityNodesMap[entityAddress] = entityNodes
	}

	// Go down the list of entities running nodes by stake, picking one node
	// to act as a validator till the maximum is reached.
	validatorEntities := make(map[staking.Address]bool)
	newValidators := make(map[signature.PublicKey]*scheduler.Validator)
electLoop:
	for _, entAddr := range sortedEntities {
		nodes := entityNodesMap[entAddr]

		// This is usually a maximum of 1, but if more are allowed,
		// like in certain test scenarios, then pick as many nodes
		// as the entity's stake allows
		for i := 0; i < params.MaxValidatorsPerEntity; i++ {
			if i >= len(nodes) {
				break
			}

			n := nodes[i]

			// If the entity gets a validator elected, it is eligible
			// for rewards, but only once regardless of the number
			// of validators owned by the entity in the set.
			if entitiesEligibleForReward != nil {
				entitiesEligibleForReward[entAddr] = true
			}

			var power int64
			if stakeAcc == nil {
				// In simplified no-stake deployments, make validators have flat voting power.
				power = 1
			} else {
				var stake *quantity.Quantity
				stake, err = stakeAcc.GetEscrowBalance(entAddr)
				if err != nil {
					return nil, fmt.Errorf("failed to fetch escrow balance for account %s: %w", entAddr, err)
				}
				power, err = scheduler.VotingPowerFromStake(stake, params.VotingPowerDistribution)
				if err != nil {
					return nil, fmt.Errorf("computing voting power for account %s with balance %v: %w",
						entAddr, stake, err,
					)
				}
			}

			validatorEntities[entAddr] = true
			newValidators[n.Consensus.ID] = &scheduler.Validator{
				ID:          n.ID,
				EntityID:    n.EntityID,
				VotingPower: power,
			}
			if len(newValidators) >= params.MaxValidators {
				break electLoop
			}
		}
	}

	if len(newValidators) == 0 {
		return nil, fmt.Errorf("cometbft/scheduler: failed to elect any validators")
	}
	if len(newValidators) < params.MinValidators {
		return nil, fmt.Errorf("cometbft/scheduler: insufficient validators")
	}

	// Set the new pending validator set in the ABCI state.  It needs to be
	// applied in EndBlock.
	state := schedulerState.NewMutableState(ctx.State())
	if err = state.PutPendingValidators(ctx, newValidators); err != nil {
		return nil, fmt.Errorf("failed to set pending validators: %w", err)
	}

	return validatorEntities, nil
}

func stakingAddressMapToSliceByStake(
	entMap map[staking.Address]bool,
	stakeAcc *stakingState.StakeAccumulatorCache,
	beacon []byte,
) ([]staking.Address, error) {
	// Convert the map of entity's stake account addresses to a lexicographically
	// sorted slice (i.e. make it deterministic).
	entities := stakingAddressMapToSortedSlice(entMap)

	// Shuffle the sorted slice to make tie-breaks "random".
	drbg, err := drbg.New(crypto.SHA512, beacon, nil, RNGContextEntities)
	if err != nil {
		return nil, fmt.Errorf("cometbft/scheduler: couldn't instantiate DRBG: %w", err)
	}
	rngSrc := mathrand.New(drbg)
	rng := rand.New(rngSrc)

	rng.Shuffle(len(entities), func(i, j int) {
		entities[i], entities[j] = entities[j], entities[i]
	})

	if stakeAcc == nil {
		return entities, nil
	}

	// Stable-sort the shuffled slice by descending escrow balance.
	var balanceErr error
	sort.SliceStable(entities, func(i, j int) bool {
		iBal, err := stakeAcc.GetEscrowBalance(entities[i])
		if err != nil {
			balanceErr = err
			return false
		}
		jBal, err := stakeAcc.GetEscrowBalance(entities[j])
		if err != nil {
			balanceErr = err
			return false
		}
		return iBal.Cmp(jBal) == 1 // Note: Not -1 to get a reversed sort.
	})
	if balanceErr != nil {
		return nil, fmt.Errorf("failed to fetch escrow balance: %w", balanceErr)
	}

	return entities, nil
}

func stakingAddressMapToSortedSlice(m map[staking.Address]bool) []staking.Address {
	sorted := make([]staking.Address, 0, len(m))
	for mk := range m {
		sorted = append(sorted, mk)
	}
	sort.Slice(sorted, func(i, j int) bool {
		return bytes.Compare(sorted[i][:], sorted[j][:]) < 0
	})
	return sorted
}
