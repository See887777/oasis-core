package roothash

import (
	"context"

	"github.com/oasisprotocol/oasis-core/go/common"
	roothashState "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/roothash/state"
	roothash "github.com/oasisprotocol/oasis-core/go/roothash/api"
	"github.com/oasisprotocol/oasis-core/go/roothash/api/block"
	"github.com/oasisprotocol/oasis-core/go/roothash/api/message"
)

// Query is the roothash query.
type Query struct {
	state *roothashState.ImmutableState
}

// NewQuery returns a new roothash query backed by the given state.
func NewQuery(state *roothashState.ImmutableState) *Query {
	return &Query{
		state: state,
	}
}

// GenesisBlock implements roothash.Query.
func (q *Query) GenesisBlock(ctx context.Context, id common.Namespace) (*block.Block, error) {
	runtime, err := q.state.RuntimeState(ctx, id)
	if err != nil {
		return nil, err
	}
	return runtime.GenesisBlock, nil
}

// LatestBlock implements roothash.Query.
func (q *Query) LatestBlock(ctx context.Context, id common.Namespace) (*block.Block, error) {
	runtime, err := q.state.RuntimeState(ctx, id)
	if err != nil {
		return nil, err
	}
	return runtime.LastBlock, nil
}

// RuntimeState implements roothash.Query.
func (q *Query) RuntimeState(ctx context.Context, id common.Namespace) (*roothash.RuntimeState, error) {
	return q.state.RuntimeState(ctx, id)
}

// RoundRoots implements roothash.Query.
func (q *Query) RoundRoots(ctx context.Context, id common.Namespace, round uint64) (*roothash.RoundRoots, error) {
	return q.state.RoundRoots(ctx, id, round)
}

// PastRoundRoots implements roothash.Query.
func (q *Query) PastRoundRoots(ctx context.Context, id common.Namespace) (map[uint64]roothash.RoundRoots, error) {
	return q.state.PastRoundRoots(ctx, id)
}

// LastRoundResults implements roothash.Query.
func (q *Query) LastRoundResults(ctx context.Context, id common.Namespace) (*roothash.RoundResults, error) {
	return q.state.LastRoundResults(ctx, id)
}

// IncomingMessageQueueMeta implements roothash.Query.
func (q *Query) IncomingMessageQueueMeta(ctx context.Context, id common.Namespace) (*message.IncomingMessageQueueMeta, error) {
	return q.state.IncomingMessageQueueMeta(ctx, id)
}

// IncomingMessageQueue implements roothash.Query.
func (q *Query) IncomingMessageQueue(ctx context.Context, id common.Namespace, offset uint64, limit uint32) ([]*message.IncomingMessage, error) {
	return q.state.IncomingMessageQueue(ctx, id, offset, limit)
}

// ConsensusParameters implements roothash.Query.
func (q *Query) ConsensusParameters(ctx context.Context) (*roothash.ConsensusParameters, error) {
	return q.state.ConsensusParameters(ctx)
}
