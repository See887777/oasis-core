package governance

import (
	"errors"
	"fmt"

	cmtabcitypes "github.com/cometbft/cometbft/abci/types"
	cmttypes "github.com/cometbft/cometbft/types"

	"github.com/oasisprotocol/oasis-core/go/common/crypto/hash"
	eventsAPI "github.com/oasisprotocol/oasis-core/go/consensus/api/events"
	app "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/governance"
	"github.com/oasisprotocol/oasis-core/go/governance/api"
)

// EventsFromCometBFT extracts governance events from CometBFT events.
func EventsFromCometBFT(
	tx cmttypes.Tx,
	height int64,
	tmEvents []cmtabcitypes.Event,
) ([]*api.Event, error) {
	var txHash hash.Hash
	switch tx {
	case nil:
		txHash.Empty()
	default:
		txHash = hash.NewFromBytes(tx)
	}

	var events []*api.Event
	var errs error
	for _, tmEv := range tmEvents {
		// Ignore events that don't relate to the governance app.
		if tmEv.GetType() != app.EventType {
			continue
		}

		for _, pair := range tmEv.GetAttributes() {
			key := pair.GetKey()
			val := pair.GetValue()

			switch {
			case eventsAPI.IsAttributeKind(key, &api.ProposalSubmittedEvent{}):
				// Proposal submitted event.
				var e api.ProposalSubmittedEvent
				if err := eventsAPI.DecodeValue(val, &e); err != nil {
					errs = errors.Join(errs, fmt.Errorf("governance: corrupt ProposalSubmitted event: %w", err))
					continue
				}

				evt := &api.Event{Height: height, TxHash: txHash, ProposalSubmitted: &e}
				events = append(events, evt)
			case eventsAPI.IsAttributeKind(key, &api.ProposalExecutedEvent{}):
				//  Proposal executed event.
				var e api.ProposalExecutedEvent
				if err := eventsAPI.DecodeValue(val, &e); err != nil {
					errs = errors.Join(errs, fmt.Errorf("governance: corrupt ProposalExecuted event: %w", err))
					continue
				}

				evt := &api.Event{Height: height, TxHash: txHash, ProposalExecuted: &e}
				events = append(events, evt)
			case eventsAPI.IsAttributeKind(key, &api.ProposalFinalizedEvent{}):
				// Proposal finalized event.
				var e api.ProposalFinalizedEvent
				if err := eventsAPI.DecodeValue(val, &e); err != nil {
					errs = errors.Join(errs, fmt.Errorf("governance: corrupt ProposalFinalized event: %w", err))
					continue
				}

				evt := &api.Event{Height: height, TxHash: txHash, ProposalFinalized: &e}
				events = append(events, evt)
			case eventsAPI.IsAttributeKind(key, &api.VoteEvent{}):
				// Vote event.
				var e api.VoteEvent
				if err := eventsAPI.DecodeValue(val, &e); err != nil {
					errs = errors.Join(errs, fmt.Errorf("governance: corrupt Vote event: %w", err))
					continue
				}

				evt := &api.Event{Height: height, TxHash: txHash, Vote: &e}
				events = append(events, evt)
			default:
				errs = errors.Join(errs, fmt.Errorf("governance: unknown event type: key: %s, val: %s", key, val))
			}
		}
	}

	return events, errs
}
