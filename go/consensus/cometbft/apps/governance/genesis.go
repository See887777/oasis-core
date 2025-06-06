package governance

import (
	"context"
	"fmt"

	"github.com/cometbft/cometbft/abci/types"

	abciAPI "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/api"
	governanceState "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/governance/state"
	genesis "github.com/oasisprotocol/oasis-core/go/genesis/api"
	governance "github.com/oasisprotocol/oasis-core/go/governance/api"
)

func (app *Application) InitChain(ctx *abciAPI.Context, _ types.RequestInitChain, doc *genesis.Document) error {
	st := doc.Governance

	epoch, err := app.state.GetCurrentEpoch(ctx)
	if err != nil {
		return fmt.Errorf("cometbft/governance: couldn't get current epoch: %w", err)
	}

	state := governanceState.NewMutableState(ctx.State())
	if err = state.SetConsensusParameters(ctx, &st.Parameters); err != nil {
		return fmt.Errorf("cometbft/governance: failed to set consensus parameters: %w", err)
	}

	// Insert proposals.
	var largestProposalID uint64
	for _, proposal := range st.Proposals {
		if proposal.ID > largestProposalID {
			largestProposalID = proposal.ID
		}
		switch proposal.State {
		case governance.StateActive:
			if err = state.SetActiveProposal(ctx, proposal); err != nil {
				return fmt.Errorf("cometbft/governance: failed to set active proposal: %w", err)
			}
		default:
			if err = state.SetProposal(ctx, proposal); err != nil {
				return fmt.Errorf("cometbft/governance: failed to set proposal: %w", err)
			}
		}
		// Insert votes for the proposal.
		for _, vote := range st.VoteEntries[proposal.ID] {
			if err = state.SetVote(ctx, proposal.ID, vote.Voter, vote.Vote); err != nil {
				return fmt.Errorf("cometbft/governance: failed to set vote: %w", err)
			}
		}
	}

	// Compute pending upgrades from proposals.
	upgrades, ids := governance.PendingUpgradesFromProposals(st.Proposals, epoch)
	for i, up := range upgrades {
		if err = state.SetPendingUpgrade(ctx, ids[i], up); err != nil {
			return fmt.Errorf("cometbft/governance: failed to set pending upgrade: %w", err)
		}
	}

	if err := state.SetNextProposalIdentifier(ctx, largestProposalID+1); err != nil {
		return fmt.Errorf("cometbft/governance: failed to set next proposal identifier: %w", err)
	}

	return nil
}

// Genesis implements governance.Query.
func (q *Query) Genesis(ctx context.Context) (*governance.Genesis, error) {
	params, err := q.state.ConsensusParameters(ctx)
	if err != nil {
		return nil, err
	}

	proposals, err := q.state.Proposals(ctx)
	if err != nil {
		return nil, err
	}

	voteEntries := make(map[uint64][]*governance.VoteEntry)
	for _, proposal := range proposals {
		var votes []*governance.VoteEntry
		votes, err = q.state.Votes(ctx, proposal.ID)
		if err != nil {
			return nil, err
		}
		voteEntries[proposal.ID] = votes
	}

	return &governance.Genesis{
		Parameters:  *params,
		Proposals:   proposals,
		VoteEntries: voteEntries,
	}, nil
}
