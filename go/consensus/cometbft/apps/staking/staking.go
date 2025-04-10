// Package staking implements the staking application.
package staking

import (
	"fmt"

	"github.com/cometbft/cometbft/abci/types"

	beacon "github.com/oasisprotocol/oasis-core/go/beacon/api"
	"github.com/oasisprotocol/oasis-core/go/common/cbor"
	"github.com/oasisprotocol/oasis-core/go/common/quantity"
	"github.com/oasisprotocol/oasis-core/go/consensus/api/transaction"
	"github.com/oasisprotocol/oasis-core/go/consensus/cometbft/api"
	governanceApi "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/governance/api"
	registryState "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/registry/state"
	roothashApi "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/roothash/api"
	stakingState "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/staking/state"
	"github.com/oasisprotocol/oasis-core/go/roothash/api/message"
	staking "github.com/oasisprotocol/oasis-core/go/staking/api"
)

// Application is a staking application.
type Application struct {
	state api.ApplicationState
	md    api.MessageDispatcher
}

// New constructs a new staking application.
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
	return staking.Methods
}

// Blessed implements api.Application.
func (app *Application) Blessed() bool {
	return false
}

// Dependencies implements api.Application.
func (app *Application) Dependencies() []string {
	return nil
}

// Subscribe implements api.Application.
func (app *Application) Subscribe() {
	// Subscribe to messages emitted by other apps.
	app.md.Subscribe(roothashApi.RuntimeMessageStaking, app)
	app.md.Subscribe(governanceApi.MessageChangeParameters, app)
	app.md.Subscribe(governanceApi.MessageValidateParameterChanges, app)
}

// OnCleanup implements api.Application.
func (app *Application) OnCleanup() {
}

// BeginBlock implements api.Application.
func (app *Application) BeginBlock(ctx *api.Context) error {
	regState := registryState.NewMutableState(ctx.State())
	stakeState := stakingState.NewMutableState(ctx.State())

	// Look up the proposer's entity.
	proposingEntity, err := app.resolveEntityIDFromProposer(ctx, regState)
	if err != nil {
		return fmt.Errorf("failed to resolve proposer entity ID: %w", err)
	}

	// Go through all voters of the previous block and resolve entities.
	// numEligibleValidators is how many total validators are in the validator set, while
	// votingEntities is from the validators which actually voted.
	lastCommitInfo := ctx.BlockContext().LastCommitInfo
	numEligibleValidators := len(lastCommitInfo.Votes)
	votingEntities, err := app.resolveEntityIDsFromVotes(ctx, regState, lastCommitInfo)
	if err != nil {
		return fmt.Errorf("failed to resolve entity IDs from votes: %w", err)
	}

	// Disburse fees from previous block.
	if err = app.disburseFeesVQ(ctx, stakeState, proposingEntity, numEligibleValidators, votingEntities); err != nil {
		return fmt.Errorf("disburse fees voters and next proposer: %w", err)
	}

	// Save block proposer for fee disbursements.
	stakingState.SetBlockProposer(ctx, proposingEntity)

	// Add rewards for proposer.
	if err = app.rewardBlockProposing(ctx, stakeState, proposingEntity, numEligibleValidators, len(votingEntities)); err != nil {
		return fmt.Errorf("staking: block proposing reward: %w", err)
	}

	// Track signing for rewards.
	if err = app.updateEpochSigning(ctx, stakeState, votingEntities); err != nil {
		return fmt.Errorf("staking: failed to update epoch signing info: %w", err)
	}

	// Iterate over any submitted evidence of a validator misbehaving. Note that
	// the actual evidence has already been verified by CometBFT to be valid.
	for _, evidence := range ctx.BlockContext().ValidatorMisbehavior {
		var reason staking.SlashReason
		switch evidence.Type {
		case types.MisbehaviorType_DUPLICATE_VOTE:
			reason = staking.SlashConsensusEquivocation
		case types.MisbehaviorType_LIGHT_CLIENT_ATTACK:
			reason = staking.SlashConsensusLightClientAttack
		default:
			ctx.Logger().Debug("ignoring unknown evidence type",
				"evidence_type", evidence.Type,
			)
			continue
		}

		if err = onEvidenceByzantineConsensus(ctx, reason, evidence.Validator.Address); err != nil {
			return err
		}
	}

	return nil
}

// ExecuteMessage implements api.MessageSubscriber.
func (app *Application) ExecuteMessage(ctx *api.Context, kind, msg any) (any, error) {
	switch kind {
	case roothashApi.RuntimeMessageStaking:
		state := stakingState.NewMutableState(ctx.State())
		m := msg.(*message.StakingMessage)
		switch {
		case m.Transfer != nil:
			return app.transfer(ctx, state, m.Transfer)
		case m.Withdraw != nil:
			return app.withdraw(ctx, state, m.Withdraw)
		case m.AddEscrow != nil:
			return app.addEscrow(ctx, state, m.AddEscrow)
		case m.ReclaimEscrow != nil:
			return app.reclaimEscrow(ctx, state, m.ReclaimEscrow)
		default:
			return nil, staking.ErrInvalidArgument
		}
	case governanceApi.MessageValidateParameterChanges:
		// A change parameters proposal is about to be submitted. Validate changes.
		return app.changeParameters(ctx, msg, false)
	case governanceApi.MessageChangeParameters:
		// A change parameters proposal has just been accepted and closed. Validate and apply
		// changes.
		return app.changeParameters(ctx, msg, true)
	default:
		return nil, staking.ErrInvalidArgument
	}
}

// ExecuteTx implements api.Application.
func (app *Application) ExecuteTx(ctx *api.Context, tx *transaction.Transaction) error {
	state := stakingState.NewMutableState(ctx.State())

	ctx.SetPriority(AppPriority)

	switch tx.Method {
	case staking.MethodTransfer:
		var xfer staking.Transfer
		if err := cbor.Unmarshal(tx.Body, &xfer); err != nil {
			return staking.ErrInvalidArgument
		}

		_, err := app.transfer(ctx, state, &xfer)
		return err
	case staking.MethodBurn:
		var burn staking.Burn
		if err := cbor.Unmarshal(tx.Body, &burn); err != nil {
			return staking.ErrInvalidArgument
		}

		return app.burn(ctx, state, &burn)
	case staking.MethodAddEscrow:
		var escrow staking.Escrow
		if err := cbor.Unmarshal(tx.Body, &escrow); err != nil {
			return staking.ErrInvalidArgument
		}

		_, err := app.addEscrow(ctx, state, &escrow)
		return err
	case staking.MethodReclaimEscrow:
		var reclaim staking.ReclaimEscrow
		if err := cbor.Unmarshal(tx.Body, &reclaim); err != nil {
			return staking.ErrInvalidArgument
		}

		_, err := app.reclaimEscrow(ctx, state, &reclaim)
		return err
	case staking.MethodAmendCommissionSchedule:
		var amend staking.AmendCommissionSchedule
		if err := cbor.Unmarshal(tx.Body, &amend); err != nil {
			return staking.ErrInvalidArgument
		}

		return app.amendCommissionSchedule(ctx, state, &amend)
	case staking.MethodAllow:
		var allow staking.Allow
		if err := cbor.Unmarshal(tx.Body, &allow); err != nil {
			return staking.ErrInvalidArgument
		}

		return app.allow(ctx, state, &allow)
	case staking.MethodWithdraw:
		var withdraw staking.Withdraw
		if err := cbor.Unmarshal(tx.Body, &withdraw); err != nil {
			return staking.ErrInvalidArgument
		}

		_, err := app.withdraw(ctx, state, &withdraw)
		return err
	default:
		return staking.ErrInvalidArgument
	}
}

// EndBlock implements api.Application.
func (app *Application) EndBlock(ctx *api.Context) (types.ResponseEndBlock, error) {
	fees := stakingState.BlockFees(ctx)
	if err := app.disburseFeesP(ctx, stakingState.NewMutableState(ctx.State()), stakingState.BlockProposer(ctx), &fees); err != nil {
		return types.ResponseEndBlock{}, fmt.Errorf("disburse fees proposer: %w", err)
	}

	if changed, epoch := app.state.EpochChanged(ctx); changed {
		return types.ResponseEndBlock{}, app.onEpochChange(ctx, epoch)
	}
	return types.ResponseEndBlock{}, nil
}

func (app *Application) onEpochChange(ctx *api.Context, epoch beacon.EpochTime) error {
	state := stakingState.NewMutableState(ctx.State())

	// Delegation unbonding after debonding period elapses.
	expiredDebondingQueue, err := state.ExpiredDebondingQueue(ctx, epoch)
	if err != nil {
		return fmt.Errorf("failed to query expired debonding queue: %w", err)
	}
	for _, e := range expiredDebondingQueue {
		deb := e.Delegation
		shareAmount := deb.Shares.Clone()
		delegator, err := state.Account(ctx, e.DelegatorAddr)
		if err != nil {
			return fmt.Errorf("failed to query delegator account: %w", err)
		}
		// NOTE: Could be the same account, so make sure to not have two duplicate
		//       copies of it and overwrite it later.
		var escrow *staking.Account
		if e.DelegatorAddr.Equal(e.EscrowAddr) {
			escrow = delegator
		} else {
			escrow, err = state.Account(ctx, e.EscrowAddr)
			if err != nil {
				return fmt.Errorf("failed to query escrow account: %w", err)
			}
		}

		var baseUnits quantity.Quantity
		if err = escrow.Escrow.Debonding.Withdraw(&baseUnits, &deb.Shares, shareAmount); err != nil {
			ctx.Logger().Error("failed to redeem debonding shares",
				"err", err,
				"escrow_addr", e.EscrowAddr,
				"delegator_addr", e.DelegatorAddr,
				"shares", deb.Shares,
			)
			return fmt.Errorf("cometbft/staking: failed to redeem debonding shares: %w", err)
		}
		stakeAmount := baseUnits.Clone()

		if err = quantity.Move(&delegator.General.Balance, &baseUnits, stakeAmount); err != nil {
			ctx.Logger().Error("failed to move debonded stake",
				"err", err,
				"escrow_addr", e.EscrowAddr,
				"delegator_addr", e.DelegatorAddr,
				"shares", deb.Shares,
				"base_units", stakeAmount,
			)
			return fmt.Errorf("cometbft/staking: failed to redeem debonding shares: %w", err)
		}

		// Update state.
		if err = state.RemoveFromDebondingQueue(ctx, e.Epoch, e.DelegatorAddr, e.EscrowAddr); err != nil {
			return fmt.Errorf("failed to remove from debonding queue: %w", err)
		}
		if err = state.SetDebondingDelegation(ctx, e.DelegatorAddr, e.EscrowAddr, e.Delegation.DebondEndTime, nil); err != nil {
			return fmt.Errorf("failed to set debonding delegation: %w", err)
		}
		if err = state.SetAccount(ctx, e.DelegatorAddr, delegator); err != nil {
			return fmt.Errorf("failed to set delegator (%s) account: %w", e.DelegatorAddr, err)
		}
		if !e.DelegatorAddr.Equal(e.EscrowAddr) {
			if err = state.SetAccount(ctx, e.EscrowAddr, escrow); err != nil {
				return fmt.Errorf("failed to set escrow (%s) account: %w", e.EscrowAddr, err)
			}
		}

		ctx.Logger().Debug("released stake",
			"escrow_addr", e.EscrowAddr,
			"delegator_addr", e.DelegatorAddr,
			"base_units", stakeAmount,
			"num_shares", shareAmount,
		)

		ctx.EmitEvent(api.NewEventBuilder(app.Name()).TypedAttribute(&staking.ReclaimEscrowEvent{
			Owner:  e.DelegatorAddr,
			Escrow: e.EscrowAddr,
			Amount: *stakeAmount,
			Shares: *shareAmount,
		}))
	}

	// Add signing rewards.
	if err := app.rewardEpochSigning(ctx, epoch); err != nil {
		ctx.Logger().Error("failed to add signing rewards",
			"err", err,
		)
		return fmt.Errorf("cometbft/staking: failed to add signing rewards: %w", err)
	}

	return nil
}
