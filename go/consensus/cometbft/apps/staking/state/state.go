package state

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"sort"

	beacon "github.com/oasisprotocol/oasis-core/go/beacon/api"
	"github.com/oasisprotocol/oasis-core/go/common/cbor"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/signature"
	"github.com/oasisprotocol/oasis-core/go/common/errors"
	"github.com/oasisprotocol/oasis-core/go/common/keyformat"
	"github.com/oasisprotocol/oasis-core/go/common/logging"
	"github.com/oasisprotocol/oasis-core/go/common/quantity"
	consensus "github.com/oasisprotocol/oasis-core/go/consensus/api"
	"github.com/oasisprotocol/oasis-core/go/consensus/cometbft/api"
	abciAPI "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/api"
	staking "github.com/oasisprotocol/oasis-core/go/staking/api"
	"github.com/oasisprotocol/oasis-core/go/storage/mkvs"
)

var (
	// AppName is the ABCI application name.
	AppName = "100_staking"

	// accountKeyFmt is the key format used for accounts (account addresses).
	//
	// Value is a CBOR-serialized account address.
	accountKeyFmt = consensus.KeyFormat.New(0x50, &staking.Address{})
	// totalSupplyKeyFmt is the key format used for the total supply.
	//
	// Value is a CBOR-serialized quantity.
	totalSupplyKeyFmt = consensus.KeyFormat.New(0x51)
	// commonPoolKeyFmt is the key format used for the common pool balance.
	//
	// Value is a CBOR-serialized quantity.
	commonPoolKeyFmt = consensus.KeyFormat.New(0x52)
	// delegationKeyFmt is the key format used for delegations (escrow address,
	// delegator address).
	//
	// Value is CBOR-serialized delegation.
	delegationKeyFmt = consensus.KeyFormat.New(0x53, &staking.Address{}, &staking.Address{})
	// debondingDelegationKeyFmt is the key format used for debonding delegations
	// (delegator address, escrow address, epoch).
	//
	// Value is CBOR-serialized debonding delegation.
	debondingDelegationKeyFmt = consensus.KeyFormat.New(0x54, &staking.Address{}, &staking.Address{}, uint64(0))
	// debondingQueueKeyFmt is the debonding queue key format
	// (epoch, delegator address, escrow address).
	//
	// Value is empty.
	debondingQueueKeyFmt = consensus.KeyFormat.New(0x55, uint64(0), &staking.Address{}, &staking.Address{})
	// parametersKeyFmt is the key format used for consensus parameters.
	//
	// Value is CBOR-serialized staking.ConsensusParameters.
	parametersKeyFmt = consensus.KeyFormat.New(0x56)
	// lastBlockFeesKeyFmt is the accumulated fee balance for the previous block.
	//
	// Value is CBOR-serialized quantity.
	lastBlockFeesKeyFmt = consensus.KeyFormat.New(0x57)
	// epochSigningKeyFmt is the key format for epoch signing information.
	//
	// Value is CBOR-serialized EpochSigning.
	epochSigningKeyFmt = consensus.KeyFormat.New(0x58)
	// governanceDepositsKeyFmt is the key format used for the governance deposits balance.
	//
	// Value is a CBOR-serialized quantity.
	governanceDepositsKeyFmt = consensus.KeyFormat.New(0x59)

	// delegationKeyReverseFmt is the key format used for reverse mapping of
	// delegations (delegator address, escrow address).
	//
	// Value is CBOR-serialized delegation.
	delegationKeyReverseFmt = consensus.KeyFormat.New(0x5A, &staking.Address{}, &staking.Address{})

	// commissionScheduleAddressesKeyFmt is the key format used for index of addresses
	// with non empty commission schedule.
	//
	// Value is empty.
	commissionScheduleAddressesKeyFmt = consensus.KeyFormat.New(0x5B, &staking.Address{})

	logger = logging.GetLogger("cometbft/staking")
)

// ImmutableState is an immutable staking state wrapper.
type ImmutableState struct {
	state *abciAPI.ImmutableState
}

// NewImmutableState creates a new immutable staking state wrapper.
func NewImmutableState(tree mkvs.ImmutableKeyValueTree) *ImmutableState {
	return &ImmutableState{
		state: api.NewImmutableState(tree),
	}
}

func (s *ImmutableState) loadStoredBalance(ctx context.Context, key *keyformat.KeyFormat) (*quantity.Quantity, error) {
	value, err := s.state.Get(ctx, key.Encode())
	if err != nil {
		return nil, abciAPI.UnavailableStateError(err)
	}
	if value == nil {
		return &quantity.Quantity{}, nil
	}

	var q quantity.Quantity
	if err = cbor.Unmarshal(value, &q); err != nil {
		return nil, abciAPI.UnavailableStateError(err)
	}
	return &q, nil
}

// TotalSupply returns the total supply balance.
func (s *ImmutableState) TotalSupply(ctx context.Context) (*quantity.Quantity, error) {
	return s.loadStoredBalance(ctx, totalSupplyKeyFmt)
}

// CommonPool returns the balance of the global common pool.
func (s *ImmutableState) CommonPool(ctx context.Context) (*quantity.Quantity, error) {
	return s.loadStoredBalance(ctx, commonPoolKeyFmt)
}

// ConsensusParameters returns the consensus parameters.
func (s *ImmutableState) ConsensusParameters(ctx context.Context) (*staking.ConsensusParameters, error) {
	raw, err := s.state.Get(ctx, parametersKeyFmt.Encode())
	if err != nil {
		return nil, abciAPI.UnavailableStateError(err)
	}
	if raw == nil {
		return nil, fmt.Errorf("cometbft/staking: expected consensus parameters to be present in app state")
	}

	var params staking.ConsensusParameters
	if err = cbor.Unmarshal(raw, &params); err != nil {
		return nil, abciAPI.UnavailableStateError(err)
	}
	return &params, nil
}

func (s *ImmutableState) DebondingInterval(ctx context.Context) (beacon.EpochTime, error) {
	params, err := s.ConsensusParameters(ctx)
	if err != nil {
		return beacon.EpochInvalid, err
	}

	return params.DebondingInterval, nil
}

func (s *ImmutableState) RewardSchedule(ctx context.Context) ([]staking.RewardStep, error) {
	params, err := s.ConsensusParameters(ctx)
	if err != nil {
		return nil, err
	}

	return params.RewardSchedule, nil
}

func (s *ImmutableState) CommissionScheduleRules(ctx context.Context) (*staking.CommissionScheduleRules, error) {
	params, err := s.ConsensusParameters(ctx)
	if err != nil {
		return nil, err
	}

	return &params.CommissionScheduleRules, nil
}

// Thresholds returns the currently configured thresholds if any.
func (s *ImmutableState) Thresholds(ctx context.Context) (map[staking.ThresholdKind]quantity.Quantity, error) {
	params, err := s.ConsensusParameters(ctx)
	if err != nil {
		return nil, err
	}

	return params.Thresholds, nil
}

// Addresses returns the non-empty addresses from the staking ledger.
func (s *ImmutableState) Addresses(ctx context.Context) ([]staking.Address, error) {
	it := s.state.NewIterator(ctx)
	defer it.Close()

	var addresses []staking.Address
	for it.Seek(accountKeyFmt.Encode()); it.Valid(); it.Next() {
		var addr staking.Address
		if !accountKeyFmt.Decode(it.Key(), &addr) {
			break
		}

		addresses = append(addresses, addr)
	}
	if it.Err() != nil {
		return nil, abciAPI.UnavailableStateError(it.Err())
	}
	return addresses, nil
}

// CommissionScheduleAddresses returns addresses that have a non empty commission schedule configured.
func (s *ImmutableState) CommissionScheduleAddresses(ctx context.Context) ([]staking.Address, error) {
	it := s.state.NewIterator(ctx)
	defer it.Close()

	var addresses []staking.Address
	for it.Seek(commissionScheduleAddressesKeyFmt.Encode()); it.Valid(); it.Next() {
		var addr staking.Address
		if !commissionScheduleAddressesKeyFmt.Decode(it.Key(), &addr) {
			break
		}

		addresses = append(addresses, addr)
	}
	if it.Err() != nil {
		return nil, abciAPI.UnavailableStateError(it.Err())
	}
	return addresses, nil
}

// Account returns the staking account for the given account address.
func (s *ImmutableState) Account(ctx context.Context, address staking.Address) (*staking.Account, error) {
	if !address.IsValid() {
		return nil, fmt.Errorf("cometbft/staking: invalid account address: %s", address)
	}

	value, err := s.state.Get(ctx, accountKeyFmt.Encode(&address))
	if err != nil {
		return nil, abciAPI.UnavailableStateError(err)
	}
	if value == nil {
		return &staking.Account{}, nil
	}

	var ent staking.Account
	if err = cbor.Unmarshal(value, &ent); err != nil {
		return nil, abciAPI.UnavailableStateError(err)
	}
	return &ent, nil
}

// EscrowBalance returns the escrow balance for the given account address.
func (s *ImmutableState) EscrowBalance(ctx context.Context, address staking.Address) (*quantity.Quantity, error) {
	account, err := s.Account(ctx, address)
	if err != nil {
		return nil, err
	}
	return &account.Escrow.Active.Balance, nil
}

// Delegations returns all active delegations.
func (s *ImmutableState) Delegations(
	ctx context.Context,
) (map[staking.Address]map[staking.Address]*staking.Delegation, error) {
	it := s.state.NewIterator(ctx)
	defer it.Close()

	delegations := make(map[staking.Address]map[staking.Address]*staking.Delegation)
	for it.Seek(delegationKeyFmt.Encode()); it.Valid(); it.Next() {
		var escrowAddr staking.Address
		var delegatorAddr staking.Address
		if !delegationKeyFmt.Decode(it.Key(), &escrowAddr, &delegatorAddr) {
			break
		}

		var del staking.Delegation
		if err := cbor.Unmarshal(it.Value(), &del); err != nil {
			return nil, abciAPI.UnavailableStateError(err)
		}

		if delegations[escrowAddr] == nil {
			delegations[escrowAddr] = make(map[staking.Address]*staking.Delegation)
		}
		delegations[escrowAddr][delegatorAddr] = &del
	}
	if it.Err() != nil {
		return nil, abciAPI.UnavailableStateError(it.Err())
	}
	return delegations, nil
}

// Delegation returns the delegation descriptor.
func (s *ImmutableState) Delegation(
	ctx context.Context,
	delegatorAddr, escrowAddr staking.Address,
) (*staking.Delegation, error) {
	value, err := s.state.Get(ctx, delegationKeyFmt.Encode(&escrowAddr, &delegatorAddr))
	if err != nil {
		return nil, abciAPI.UnavailableStateError(err)
	}
	if value == nil {
		return &staking.Delegation{}, nil
	}

	var del staking.Delegation
	if err = cbor.Unmarshal(value, &del); err != nil {
		return nil, abciAPI.UnavailableStateError(err)
	}
	return &del, nil
}

func (s *ImmutableState) DelegationsFor(
	ctx context.Context,
	delegatorAddr staking.Address,
) (map[staking.Address]*staking.Delegation, error) {
	it := s.state.NewIterator(ctx)
	defer it.Close()

	delegations := make(map[staking.Address]*staking.Delegation)
	for it.Seek(delegationKeyReverseFmt.Encode(delegatorAddr)); it.Valid(); it.Next() {
		var escrowAddr staking.Address
		var decDelegatorAddr staking.Address
		if !delegationKeyReverseFmt.Decode(it.Key(), &decDelegatorAddr, &escrowAddr) {
			break
		}
		if !decDelegatorAddr.Equal(delegatorAddr) {
			break
		}

		var del staking.Delegation
		if err := cbor.Unmarshal(it.Value(), &del); err != nil {
			return nil, abciAPI.UnavailableStateError(err)
		}

		delegations[escrowAddr] = &del
	}
	if it.Err() != nil {
		return nil, abciAPI.UnavailableStateError(it.Err())
	}
	return delegations, nil
}

func (s *ImmutableState) DelegationsTo(
	ctx context.Context,
	destAddr staking.Address,
) (map[staking.Address]*staking.Delegation, error) {
	it := s.state.NewIterator(ctx)
	defer it.Close()

	delegations := make(map[staking.Address]*staking.Delegation)
	for it.Seek(delegationKeyFmt.Encode(destAddr)); it.Valid(); it.Next() {
		var escrowAddr staking.Address
		var delegatorAddr staking.Address
		if !delegationKeyFmt.Decode(it.Key(), &escrowAddr, &delegatorAddr) {
			break
		}
		if !escrowAddr.Equal(destAddr) {
			break
		}

		var del staking.Delegation
		if err := cbor.Unmarshal(it.Value(), &del); err != nil {
			return nil, abciAPI.UnavailableStateError(err)
		}

		delegations[delegatorAddr] = &del
	}
	if it.Err() != nil {
		return nil, abciAPI.UnavailableStateError(it.Err())
	}
	return delegations, nil
}

func (s *ImmutableState) DebondingDelegations(
	ctx context.Context,
) (map[staking.Address]map[staking.Address][]*staking.DebondingDelegation, error) {
	it := s.state.NewIterator(ctx)
	defer it.Close()

	delegations := make(map[staking.Address]map[staking.Address][]*staking.DebondingDelegation)
	for it.Seek(debondingDelegationKeyFmt.Encode()); it.Valid(); it.Next() {
		var escrowAddr staking.Address
		var delegatorAddr staking.Address
		if !debondingDelegationKeyFmt.Decode(it.Key(), &delegatorAddr, &escrowAddr) {
			break
		}

		var deb staking.DebondingDelegation
		if err := cbor.Unmarshal(it.Value(), &deb); err != nil {
			return nil, abciAPI.UnavailableStateError(err)
		}

		if delegations[escrowAddr] == nil {
			delegations[escrowAddr] = make(map[staking.Address][]*staking.DebondingDelegation)
		}
		delegations[escrowAddr][delegatorAddr] = append(delegations[escrowAddr][delegatorAddr], &deb)
	}
	if it.Err() != nil {
		return nil, abciAPI.UnavailableStateError(it.Err())
	}
	return delegations, nil
}

func (s *ImmutableState) DebondingDelegation(
	ctx context.Context,
	delegatorAddr, escrowAddr staking.Address,
	epoch beacon.EpochTime,
) (*staking.DebondingDelegation, error) {
	value, err := s.state.Get(ctx, debondingDelegationKeyFmt.Encode(&delegatorAddr, &escrowAddr, uint64(epoch)))
	if err != nil {
		return nil, abciAPI.UnavailableStateError(err)
	}
	if value == nil {
		return &staking.DebondingDelegation{}, nil
	}

	var deb staking.DebondingDelegation
	if err = cbor.Unmarshal(value, &deb); err != nil {
		return nil, abciAPI.UnavailableStateError(err)
	}
	return &deb, nil
}

func (s *ImmutableState) DebondingDelegationsFor(
	ctx context.Context,
	delegatorAddr staking.Address,
) (map[staking.Address][]*staking.DebondingDelegation, error) {
	it := s.state.NewIterator(ctx)
	defer it.Close()

	delegations := make(map[staking.Address][]*staking.DebondingDelegation)
	for it.Seek(debondingDelegationKeyFmt.Encode(&delegatorAddr)); it.Valid(); it.Next() {
		var escrowAddr staking.Address
		var decDelegatorAddr staking.Address
		if !debondingDelegationKeyFmt.Decode(it.Key(), &decDelegatorAddr, &escrowAddr) {
			break
		}
		if !decDelegatorAddr.Equal(delegatorAddr) {
			break
		}

		var deb staking.DebondingDelegation
		if err := cbor.Unmarshal(it.Value(), &deb); err != nil {
			return nil, abciAPI.UnavailableStateError(err)
		}

		delegations[escrowAddr] = append(delegations[escrowAddr], &deb)
	}
	if it.Err() != nil {
		return nil, abciAPI.UnavailableStateError(it.Err())
	}
	return delegations, nil
}

func (s *ImmutableState) DebondingDelegationsTo(
	ctx context.Context,
	destAddr staking.Address,
) (map[staking.Address][]*staking.DebondingDelegation, error) {
	it := s.state.NewIterator(ctx)
	defer it.Close()

	delegations := make(map[staking.Address][]*staking.DebondingDelegation)
	for it.Seek(debondingDelegationKeyFmt.Encode()); it.Valid(); it.Next() {
		var escrowAddr staking.Address
		var delegatorAddr staking.Address
		if !debondingDelegationKeyFmt.Decode(it.Key(), &delegatorAddr, &escrowAddr) {
			break
		}
		if !escrowAddr.Equal(destAddr) {
			continue
		}

		var deb staking.DebondingDelegation
		if err := cbor.Unmarshal(it.Value(), &deb); err != nil {
			return nil, abciAPI.UnavailableStateError(err)
		}

		delegations[delegatorAddr] = append(delegations[delegatorAddr], &deb)
	}
	if it.Err() != nil {
		return nil, abciAPI.UnavailableStateError(it.Err())
	}
	return delegations, nil
}

type DebondingQueueEntry struct {
	Epoch         beacon.EpochTime
	DelegatorAddr staking.Address
	EscrowAddr    staking.Address
	Delegation    *staking.DebondingDelegation
}

func (s *ImmutableState) ExpiredDebondingQueue(ctx context.Context, epoch beacon.EpochTime) ([]*DebondingQueueEntry, error) {
	it := s.state.NewIterator(ctx)
	defer it.Close()

	var entries []*DebondingQueueEntry
	for it.Seek(debondingQueueKeyFmt.Encode()); it.Valid(); it.Next() {
		var decEpoch uint64
		var escrowAddr staking.Address
		var delegatorAddr staking.Address
		if !debondingQueueKeyFmt.Decode(it.Key(), &decEpoch, &delegatorAddr, &escrowAddr) || decEpoch > uint64(epoch) {
			break
		}

		deb, err := s.DebondingDelegation(ctx, delegatorAddr, escrowAddr, beacon.EpochTime(decEpoch))
		if err != nil {
			return nil, err
		}
		entries = append(entries, &DebondingQueueEntry{
			Epoch:         beacon.EpochTime(decEpoch),
			DelegatorAddr: delegatorAddr,
			EscrowAddr:    escrowAddr,
			Delegation:    deb,
		})
	}
	if it.Err() != nil {
		return nil, abciAPI.UnavailableStateError(it.Err())
	}
	return entries, nil
}

func (s *ImmutableState) Slashing(ctx context.Context) (map[staking.SlashReason]staking.Slash, error) {
	params, err := s.ConsensusParameters(ctx)
	if err != nil {
		return nil, err
	}
	if params.Slashing == nil {
		return make(map[staking.SlashReason]staking.Slash), nil
	}

	return params.Slashing, nil
}

// LastBlockFees returns the last block fees balance.
func (s *ImmutableState) LastBlockFees(ctx context.Context) (*quantity.Quantity, error) {
	return s.loadStoredBalance(ctx, lastBlockFeesKeyFmt)
}

// GovernanceDeposits returns the governance deposits balance.
func (s *ImmutableState) GovernanceDeposits(ctx context.Context) (*quantity.Quantity, error) {
	return s.loadStoredBalance(ctx, governanceDepositsKeyFmt)
}

type EpochSigning struct {
	Total    uint64
	ByEntity map[signature.PublicKey]uint64
}

func (es *EpochSigning) Update(signingEntities []signature.PublicKey) error {
	oldTotal := es.Total
	es.Total = oldTotal + 1
	if es.Total <= oldTotal {
		return fmt.Errorf("incrementing total blocks count: overflow, old_total=%d", oldTotal)
	}

	for _, entityID := range signingEntities {
		oldCount := es.ByEntity[entityID]
		es.ByEntity[entityID] = oldCount + 1
		if es.ByEntity[entityID] <= oldCount {
			return fmt.Errorf("incrementing count for entity %s: overflow, old_count=%d", entityID, oldCount)
		}
	}

	return nil
}

func (es *EpochSigning) EligibleEntities(thresholdNumerator, thresholdDenominator uint64) ([]signature.PublicKey, error) {
	var eligibleEntities []signature.PublicKey
	if es.Total > math.MaxUint64/thresholdNumerator {
		return nil, fmt.Errorf("overflow in total blocks, total=%d", es.Total)
	}
	thresholdPremultiplied := es.Total * thresholdNumerator
	for entityID, count := range es.ByEntity {
		if count > math.MaxUint64/thresholdDenominator {
			return nil, fmt.Errorf("entity %s: overflow in threshold comparison, count=%d", entityID, count)
		}
		if count*thresholdDenominator < thresholdPremultiplied {
			continue
		}
		eligibleEntities = append(eligibleEntities, entityID)
	}
	sort.Slice(eligibleEntities, func(i, j int) bool {
		return bytes.Compare(eligibleEntities[i][:], eligibleEntities[j][:]) < 0
	})
	return eligibleEntities, nil
}

func (s *ImmutableState) EpochSigning(ctx context.Context) (*EpochSigning, error) {
	value, err := s.state.Get(ctx, epochSigningKeyFmt.Encode())
	if err != nil {
		return nil, abciAPI.UnavailableStateError(err)
	}
	if value == nil {
		// Not present means zero everything.
		return &EpochSigning{
			ByEntity: make(map[signature.PublicKey]uint64),
		}, nil
	}

	var es EpochSigning
	if err = cbor.Unmarshal(value, &es); err != nil {
		return nil, abciAPI.UnavailableStateError(err)
	}
	return &es, nil
}

// MutableState is a mutable staking state wrapper.
type MutableState struct {
	*ImmutableState

	ms mkvs.KeyValueTree
}

// NewMutableState creates a new mutable staking state wrapper.
func NewMutableState(tree mkvs.KeyValueTree) *MutableState {
	return &MutableState{
		ImmutableState: NewImmutableState(tree),
		ms:             tree,
	}
}

func (s *MutableState) SetAccount(ctx context.Context, addr staking.Address, account *staking.Account) error {
	// Update account.
	if err := s.ms.Insert(ctx, accountKeyFmt.Encode(&addr), cbor.Marshal(account)); err != nil {
		return abciAPI.UnavailableStateError(err)
	}

	// Update commission schedule index.
	switch account.Escrow.CommissionSchedule.IsEmpty() {
	case true:
		return abciAPI.UnavailableStateError(s.ms.Remove(ctx, commissionScheduleAddressesKeyFmt.Encode(addr)))
	default:
		return abciAPI.UnavailableStateError(s.ms.Insert(ctx, commissionScheduleAddressesKeyFmt.Encode(addr), []byte{}))
	}
}

func (s *MutableState) SetAccountHook(ctx context.Context, addr staking.Address, kind staking.HookKind, dst *staking.HookDestination) error {
	acct, err := s.Account(ctx, addr)
	if err != nil {
		return err
	}

	if acct.General.Hooks == nil {
		acct.General.Hooks = make(map[staking.HookKind]staking.HookDestination)
	}
	acct.General.Hooks[kind] = *dst

	return s.SetAccount(ctx, addr, acct)
}

func (s *MutableState) SetTotalSupply(ctx context.Context, q *quantity.Quantity) error {
	err := s.ms.Insert(ctx, totalSupplyKeyFmt.Encode(), cbor.Marshal(q))
	return abciAPI.UnavailableStateError(err)
}

func (s *MutableState) SetCommonPool(ctx context.Context, q *quantity.Quantity) error {
	err := s.ms.Insert(ctx, commonPoolKeyFmt.Encode(), cbor.Marshal(q))
	return abciAPI.UnavailableStateError(err)
}

// SetConsensusParameters sets staking consensus parameters.
//
// NOTE: This method must only be called from InitChain/EndBlock contexts.
func (s *MutableState) SetConsensusParameters(ctx context.Context, params *staking.ConsensusParameters) error {
	if err := s.state.CheckContextMode(ctx, []abciAPI.ContextMode{abciAPI.ContextInitChain, abciAPI.ContextEndBlock}); err != nil {
		return err
	}
	err := s.ms.Insert(ctx, parametersKeyFmt.Encode(), cbor.Marshal(params))
	return abciAPI.UnavailableStateError(err)
}

func (s *MutableState) SetDelegation(
	ctx context.Context,
	delegatorAddr, escrowAddr staking.Address,
	d *staking.Delegation,
) error {
	// Remove delegation if there are no more shares in it.
	if d.Shares.IsZero() {
		if err := s.ms.Remove(ctx, delegationKeyFmt.Encode(&escrowAddr, &delegatorAddr)); err != nil {
			return abciAPI.UnavailableStateError(err)
		}

		err := s.ms.Remove(ctx, delegationKeyReverseFmt.Encode(&delegatorAddr, &escrowAddr))
		return abciAPI.UnavailableStateError(err)
	}

	if err := s.ms.Insert(ctx, delegationKeyFmt.Encode(&escrowAddr, &delegatorAddr), cbor.Marshal(d)); err != nil {
		return abciAPI.UnavailableStateError(err)
	}
	err := s.ms.Insert(ctx, delegationKeyReverseFmt.Encode(&delegatorAddr, &escrowAddr), cbor.Marshal(d))
	return abciAPI.UnavailableStateError(err)
}

func (s *MutableState) SetDebondingDelegation(
	ctx context.Context,
	delegatorAddr, escrowAddr staking.Address,
	epoch beacon.EpochTime,
	d *staking.DebondingDelegation,
) error {
	key := debondingDelegationKeyFmt.Encode(&delegatorAddr, &escrowAddr, uint64(epoch))

	if d == nil {
		// Remove descriptor.
		err := s.ms.Remove(ctx, key)
		return abciAPI.UnavailableStateError(err)
	}

	// Create a copy so we don't modify the passed in object in case we are merging
	// it with an existing delegation.
	debDel := staking.DebondingDelegation{
		Shares:        *d.Shares.Clone(),
		DebondEndTime: d.DebondEndTime,
	}

	// If a debonding delegation for the account and same end epoch already exists,
	// merge the debonding delegations.
	value, err := s.state.Get(ctx, key)
	if err != nil {
		return abciAPI.UnavailableStateError(err)
	}
	if value != nil {
		var deb staking.DebondingDelegation
		if err = cbor.Unmarshal(value, &deb); err != nil {
			return abciAPI.UnavailableStateError(err)
		}
		if err = debDel.Merge(deb); err != nil {
			return fmt.Errorf("error merging debonding delegations: %w", err)
		}
	}

	// Add to debonding queue.
	if err := s.ms.Insert(
		ctx,
		debondingQueueKeyFmt.Encode(uint64(d.DebondEndTime),
			&delegatorAddr,
			&escrowAddr,
		),
		[]byte{},
	); err != nil {
		return abciAPI.UnavailableStateError(err)
	}
	// Add descriptor.
	if err := s.ms.Insert(ctx, key, cbor.Marshal(debDel)); err != nil {
		return abciAPI.UnavailableStateError(err)
	}
	return nil
}

func (s *MutableState) RemoveFromDebondingQueue(
	ctx context.Context,
	epoch beacon.EpochTime,
	delegatorAddr, escrowAddr staking.Address,
) error {
	err := s.ms.Remove(ctx, debondingQueueKeyFmt.Encode(uint64(epoch), &delegatorAddr, &escrowAddr))
	return abciAPI.UnavailableStateError(err)
}

func (s *MutableState) SetLastBlockFees(ctx context.Context, q *quantity.Quantity) error {
	err := s.ms.Insert(ctx, lastBlockFeesKeyFmt.Encode(), cbor.Marshal(q))
	return abciAPI.UnavailableStateError(err)
}

func (s *MutableState) SetEpochSigning(ctx context.Context, es *EpochSigning) error {
	err := s.ms.Insert(ctx, epochSigningKeyFmt.Encode(), cbor.Marshal(es))
	return abciAPI.UnavailableStateError(err)
}

func (s *MutableState) ClearEpochSigning(ctx context.Context) error {
	err := s.ms.Remove(ctx, epochSigningKeyFmt.Encode())
	return abciAPI.UnavailableStateError(err)
}

func (s *MutableState) SetGovernanceDeposits(ctx context.Context, q *quantity.Quantity) error {
	err := s.ms.Insert(ctx, governanceDepositsKeyFmt.Encode(), cbor.Marshal(q))
	return abciAPI.UnavailableStateError(err)
}

func slashPool(dst *quantity.Quantity, p *staking.SharePool, amount, total *quantity.Quantity) error {
	if total.IsZero() {
		// Nothing to slash.
		return nil
	}
	// slashAmount = amount * p.Balance / total
	slashAmount := p.Balance.Clone()
	if err := slashAmount.Mul(amount); err != nil {
		return fmt.Errorf("cometbft/staking: slashAmount.Mul: %w", err)
	}
	if err := slashAmount.Quo(total); err != nil {
		return fmt.Errorf("cometbft/staking: slashAmount.Quo: %w", err)
	}

	if _, err := quantity.MoveUpTo(dst, &p.Balance, slashAmount); err != nil {
		return fmt.Errorf("cometbft/staking: failed moving stake: %w", err)
	}

	return nil
}

// SlashEscrow slashes the escrow balance and the escrow-but-undergoing-debonding
// balance of the account, transferring it to the global common pool, returning
// the amount actually slashed.
//
// WARNING: This is an internal routine to be used to implement staking policy,
// and MUST NOT be exposed outside of backend implementations.
func (s *MutableState) SlashEscrow(
	ctx *abciAPI.Context,
	fromAddr staking.Address,
	amount *quantity.Quantity,
) (*quantity.Quantity, error) {
	var activeSlashed quantity.Quantity
	var debondingSlashed quantity.Quantity

	commonPool, err := s.CommonPool(ctx)
	if err != nil {
		return nil, fmt.Errorf("cometbft/staking: failed to query common pool for slash: %w", err)
	}

	from, err := s.Account(ctx, fromAddr)
	if err != nil {
		return nil, fmt.Errorf("cometbft/staking: failed to query account %s: %w", fromAddr, err)
	}

	// Compute the amount we need to slash each pool. The amount is split
	// between the pools based on relative total balance.
	total := from.Escrow.Active.Balance.Clone()
	if err = total.Add(&from.Escrow.Debonding.Balance); err != nil {
		return nil, fmt.Errorf("cometbft/staking: account total balance: %w", err)
	}
	if err = slashPool(&activeSlashed, &from.Escrow.Active, amount, total); err != nil {
		return nil, fmt.Errorf("cometbft/staking: failed slashing active escrow: %w", err)
	}
	if err = slashPool(&debondingSlashed, &from.Escrow.Debonding, amount, total); err != nil {
		return nil, fmt.Errorf("cometbft/staking: failed slashing debonding escrow: %w", err)
	}

	totalSlashed := activeSlashed.Clone()
	if err = totalSlashed.Add(&debondingSlashed); err != nil {
		return nil, fmt.Errorf("cometbft/staking: failed totalling slashed amounts: %w", err)
	}
	// Nothing was slashed.
	if totalSlashed.IsZero() {
		return totalSlashed, nil
	}

	if err = quantity.Move(commonPool, totalSlashed.Clone(), totalSlashed); err != nil {
		return nil, fmt.Errorf("cometbft/staking: failed moving stake to common pool: %w", err)
	}

	if err = s.SetCommonPool(ctx, commonPool); err != nil {
		return nil, fmt.Errorf("cometbft/staking: failed to set common pool: %w", err)
	}
	if err = s.SetAccount(ctx, fromAddr, from); err != nil {
		return nil, fmt.Errorf("cometbft/staking: failed to set account: %w", err)
	}

	if !ctx.IsCheckOnly() {
		ctx.EmitEvent(abciAPI.NewEventBuilder(AppName).TypedAttribute(&staking.TakeEscrowEvent{
			Owner:           fromAddr,
			Amount:          *totalSlashed,
			DebondingAmount: debondingSlashed,
		}))
	}

	return totalSlashed, nil
}

// Transfer performs a transfer between two general account balances.
func (s *MutableState) Transfer(ctx *abciAPI.Context, fromAddr, toAddr staking.Address, amount *quantity.Quantity) error {
	if fromAddr.Equal(toAddr) || amount.IsZero() {
		return nil
	}

	from, err := s.Account(ctx, fromAddr)
	if err != nil {
		return err
	}
	to, err := s.Account(ctx, toAddr)
	if err != nil {
		return err
	}

	if err = quantity.Move(&to.General.Balance, &from.General.Balance, amount); err != nil {
		return staking.ErrInsufficientBalance
	}

	// Check against minimum balance.
	params, err := s.ConsensusParameters(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch consensus parameters: %w", err)
	}
	if from.General.Balance.Cmp(&params.MinTransactBalance) < 0 {
		ctx.Logger().Debug("source account balance too low",
			"account_addr", fromAddr,
			"account_balance", from.General.Balance,
			"min_transact_balance", params.MinTransactBalance,
		)
		return errors.WithContext(staking.ErrBalanceTooLow, "source account")
	}
	if to.General.Balance.Cmp(&params.MinTransactBalance) < 0 {
		ctx.Logger().Debug("after transfer dest account balance too low",
			"account_addr", toAddr,
			"account_balance", to.General.Balance,
			"min_transact_balance", params.MinTransactBalance,
		)
		return errors.WithContext(staking.ErrBalanceTooLow, "dest account")
	}

	if err = s.SetAccount(ctx, fromAddr, from); err != nil {
		return err
	}
	if err = s.SetAccount(ctx, toAddr, to); err != nil {
		return err
	}

	if !ctx.IsCheckOnly() {
		ctx.EmitEvent(abciAPI.NewEventBuilder(AppName).TypedAttribute(&staking.TransferEvent{
			From:   fromAddr,
			To:     toAddr,
			Amount: *amount,
		}))
	}

	return nil
}

// Computes commission for a given rate and total amount. Returns the commission and the remaining amount.
func (s *MutableState) computeCommission(ctx context.Context, rate *quantity.Quantity, total *quantity.Quantity) (*quantity.Quantity, *quantity.Quantity, error) {
	if rate == nil {
		var params *staking.ConsensusParameters
		params, err := s.ConsensusParameters(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("cometbft/staking: failed to query consensus parameters: %w", err)
		}
		rate = &params.CommissionScheduleRules.MinCommissionRate
	}
	com := total.Clone()
	remaining := total.Clone()
	// Multiply first.
	if err := com.Mul(rate); err != nil {
		return nil, nil, fmt.Errorf("cometbft/staking: failed multiplying by commission rate: %w", err)
	}
	if err := com.Quo(staking.CommissionRateDenominator); err != nil {
		return nil, nil, fmt.Errorf("cometbft/staking: failed dividing by commission rate denominator: %w", err)
	}

	if err := remaining.Sub(com); err != nil {
		return nil, nil, fmt.Errorf("cometbft/staking: failed subtracting commission: %w", err)
	}

	return com, remaining, nil
}

// TransferFromCommon transfers up to the amount from the global common pool
// to the general balance of the account, returning true iff the
// amount transferred is > 0.
//
// If the escrow flag is true then the amount is escrowed instead of being
// transferred. The escrow operation takes the entity's commission rate into
// account and the rest is distributed to all delegators equally.
//
// WARNING: This is an internal routine to be used to implement incentivization
// policy, and MUST NOT be exposed outside of backend implementations.
func (s *MutableState) TransferFromCommon(
	ctx *abciAPI.Context,
	toAddr staking.Address,
	amount *quantity.Quantity,
	escrow bool,
) (bool, error) {
	commonPool, err := s.CommonPool(ctx)
	if err != nil {
		return false, fmt.Errorf("cometbft/staking: failed to query common pool for transfer: %w", err)
	}

	// Transfer up to the given amount from the common pool to the general account balance.
	to, err := s.Account(ctx, toAddr)
	if err != nil {
		return false, fmt.Errorf("cometbft/staking: failed to query account %s: %w", toAddr, err)
	}
	transferred, err := quantity.MoveUpTo(&to.General.Balance, commonPool, amount)
	if err != nil {
		return false, fmt.Errorf("cometbft/staking: failed to transfer from common pool: %w", err)
	}
	if transferred.IsZero() {
		// Common pool has been depleated, nothing to transfer.
		return false, nil
	}

	switch escrow {
	case true:
		// If escrow is requested, escrow the transferred stake immediately.
		var com *quantity.Quantity
		switch to.Escrow.Active.TotalShares.IsZero() {
		case false:
			// Compute commission.
			var epoch beacon.EpochTime
			epoch, err = ctx.AppState().GetCurrentEpoch(ctx)
			if err != nil {
				return false, fmt.Errorf("cometbft/staking: failed to get current epoch: %w", err)
			}

			rate := to.Escrow.CommissionSchedule.CurrentRate(epoch)
			com, transferred, err = s.computeCommission(ctx, rate, transferred)
			if err != nil {
				return false, err
			}

			// Escrow everything except the commission (increases value of all shares).
			if err = quantity.Move(&to.Escrow.Active.Balance, &to.General.Balance, transferred); err != nil {
				return false, fmt.Errorf("cometbft/staking: failed transferring to active escrow balance from common pool: %w", err)
			}

			// Emit non commissioned reward event.
			if !ctx.IsCheckOnly() {
				ctx.EmitEvent(abciAPI.NewEventBuilder(AppName).TypedAttribute(&staking.AddEscrowEvent{
					Owner:  staking.CommonPoolAddress,
					Escrow: toAddr,
					Amount: *transferred,
					// No new shares as this is the reward. As a result existing share price increases.
					NewShares: quantity.Quantity{},
				}))
			}
		case true:
			// If nothing has been escrowed before, everything counts as commission.
			com = transferred.Clone()
		}

		// Escrow commission.
		if com != nil && !com.IsZero() {
			var delegation *staking.Delegation
			delegation, err = s.Delegation(ctx, toAddr, toAddr)
			if err != nil {
				return false, fmt.Errorf("cometbft/staking: failed to query delegation: %w", err)
			}

			var obtainedShares *quantity.Quantity
			obtainedShares, err = to.Escrow.Active.Deposit(&delegation.Shares, &to.General.Balance, com)
			if err != nil {
				return false, fmt.Errorf("cometbft/staking: failed to deposit to escrow: %w", err)
			}

			if err = s.SetDelegation(ctx, toAddr, toAddr, delegation); err != nil {
				return false, fmt.Errorf("cometbft/staking: failed to set delegation: %w", err)
			}

			// Emit events.
			// Commission was transferred to the account, and automatically escrowed.
			if !ctx.IsCheckOnly() {
				ctx.EmitEvent(abciAPI.NewEventBuilder(AppName).TypedAttribute(&staking.TransferEvent{
					From:   staking.CommonPoolAddress,
					To:     toAddr,
					Amount: *com,
				}))

				ctx.EmitEvent(abciAPI.NewEventBuilder(AppName).TypedAttribute(&staking.AddEscrowEvent{
					Owner:     toAddr,
					Escrow:    toAddr,
					Amount:    *com,
					NewShares: *obtainedShares,
				}))
			}
		}
	case false:
		if !ctx.IsCheckOnly() {
			ctx.EmitEvent(abciAPI.NewEventBuilder(AppName).TypedAttribute(&staking.TransferEvent{
				From:   staking.CommonPoolAddress,
				To:     toAddr,
				Amount: *transferred,
			}))
		}

	}

	if err = s.SetCommonPool(ctx, commonPool); err != nil {
		return false, fmt.Errorf("cometbft/staking: failed to set common pool: %w", err)
	}
	if err = s.SetAccount(ctx, toAddr, to); err != nil {
		return false, fmt.Errorf("cometbft/staking: failed to set account %s: %w", toAddr, err)
	}

	return true, nil
}

// TransferToGovernanceDeposits transfers the amount from the submitter to the
// governance deposits pool.
func (s *MutableState) TransferToGovernanceDeposits(
	ctx *abciAPI.Context,
	fromAddr staking.Address,
	amount *quantity.Quantity,
) error {
	from, err := s.Account(ctx, fromAddr)
	if err != nil {
		return fmt.Errorf("cometbft/staking: failed to query account %s: %w", fromAddr, err)
	}

	deposits, err := s.GovernanceDeposits(ctx)
	if err != nil {
		return fmt.Errorf("cometbft/staking: failed to query governance deposit for deposit %w", err)
	}

	if err = quantity.Move(deposits, &from.General.Balance, amount); err != nil {
		return fmt.Errorf("cometbft/staking: failed to transfer to governance deposits, from: %s: %w", fromAddr, err)
	}

	if err = s.SetAccount(ctx, fromAddr, from); err != nil {
		return fmt.Errorf("cometbft/staking: failed to set governance deposit submitter account: %w", err)
	}
	if err = s.SetGovernanceDeposits(ctx, deposits); err != nil {
		return fmt.Errorf("cometbft/staking: failed to set governance deposits: %w", err)
	}

	if !ctx.IsCheckOnly() {
		ctx.EmitEvent(abciAPI.NewEventBuilder(AppName).TypedAttribute(&staking.TransferEvent{
			From:   fromAddr,
			To:     staking.GovernanceDepositsAddress,
			Amount: *amount,
		}))
	}

	return nil
}

// TransferFromGovernanceDeposits transfers the amount from the governance
// deposits pool to the specified address.
func (s *MutableState) TransferFromGovernanceDeposits(
	ctx *abciAPI.Context,
	toAddr staking.Address,
	amount *quantity.Quantity,
) error {
	to, err := s.Account(ctx, toAddr)
	if err != nil {
		return fmt.Errorf("cometbft/staking: failed to query account %s: %w", toAddr, err)
	}

	deposits, err := s.GovernanceDeposits(ctx)
	if err != nil {
		return fmt.Errorf("cometbft/staking: failed to query governance deposit %w", err)
	}

	if err = quantity.Move(&to.General.Balance, deposits, amount); err != nil {
		return fmt.Errorf("cometbft/staking: failed to transfer from governance deposits, to: %s: %w", toAddr, err)
	}

	if err = s.SetAccount(ctx, toAddr, to); err != nil {
		return fmt.Errorf("cometbft/staking: failed to set governance deposit submitter account: %w", err)
	}
	if err = s.SetGovernanceDeposits(ctx, deposits); err != nil {
		return fmt.Errorf("cometbft/staking: failed to set governance deposits: %w", err)
	}

	if !ctx.IsCheckOnly() {
		ctx.EmitEvent(abciAPI.NewEventBuilder(AppName).TypedAttribute(&staking.TransferEvent{
			From:   staking.GovernanceDepositsAddress,
			To:     toAddr,
			Amount: *amount,
		}))
	}

	return nil
}

// DiscardGovernanceDeposit discards the amount from the governance
// deposits pool to the common pool.
func (s *MutableState) DiscardGovernanceDeposit(
	ctx *abciAPI.Context,
	amount *quantity.Quantity,
) error {
	commonPool, err := s.CommonPool(ctx)
	if err != nil {
		return fmt.Errorf("cometbft/staking: failed to query common pool for transfer: %w", err)
	}

	deposits, err := s.GovernanceDeposits(ctx)
	if err != nil {
		return fmt.Errorf("cometbft/staking: failed to query governance deposit %w", err)
	}

	if err = quantity.Move(commonPool, deposits, amount); err != nil {
		return fmt.Errorf("cometbft/staking: failed to transfer from governance deposits, to common pool: %w", err)
	}

	if err = s.SetGovernanceDeposits(ctx, deposits); err != nil {
		return fmt.Errorf("cometbft/staking: failed to set governance deposits: %w", err)
	}
	if err = s.SetCommonPool(ctx, commonPool); err != nil {
		return fmt.Errorf("cometbft/staking: failed to set common pool: %w", err)
	}

	if !ctx.IsCheckOnly() {
		ctx.EmitEvent(abciAPI.NewEventBuilder(AppName).TypedAttribute(&staking.TransferEvent{
			From:   staking.GovernanceDepositsAddress,
			To:     staking.CommonPoolAddress,
			Amount: *amount,
		}))
	}

	return nil
}

// AddRewards computes and transfers a staking reward to active escrow accounts.
// If the common pool runs out, the rewards that are too big to be paid out are
// skipped and no error is returned.
func (s *MutableState) AddRewards(
	ctx *abciAPI.Context,
	time beacon.EpochTime,
	factor *quantity.Quantity,
	addresses []staking.Address,
) error {
	steps, err := s.RewardSchedule(ctx)
	if err != nil {
		return err
	}
	var activeStep *staking.RewardStep
	for i, step := range steps {
		if time < step.Until {
			activeStep = &steps[i]
			break
		}
	}
	if activeStep == nil {
		// We're past the end of the schedule.
		return nil
	}

	commonPool, err := s.CommonPool(ctx)
	if err != nil {
		return fmt.Errorf("cometbft/staking: loading common pool: %w", err)
	}

	for _, addr := range addresses {
		var ent *staking.Account
		ent, err = s.Account(ctx, addr)
		if err != nil {
			return fmt.Errorf("cometbft/staking: failed to fetch account %s: %w", addr, err)
		}

		q := ent.Escrow.Active.Balance.Clone()
		// Multiply first.
		if err = q.Mul(factor); err != nil {
			return fmt.Errorf("cometbft/staking: failed multiplying by reward factor: %w", err)
		}
		if err = q.Mul(&activeStep.Scale); err != nil {
			return fmt.Errorf("cometbft/staking: failed multiplying by reward step scale: %w", err)
		}
		if err = q.Quo(staking.RewardAmountDenominator); err != nil {
			return fmt.Errorf("cometbft/staking: failed dividing by reward amount denominator: %w", err)
		}

		if q.IsZero() {
			continue
		}

		if q.Cmp(commonPool) == 1 {
			// Not enough balance left in common pool, skip.
			continue
		}

		rate := ent.Escrow.CommissionSchedule.CurrentRate(time)
		var com *quantity.Quantity
		com, q, err = s.computeCommission(ctx, rate, q)
		if err != nil {
			return err
		}

		if !q.IsZero() {
			if err = quantity.Move(&ent.Escrow.Active.Balance, commonPool, q); err != nil {
				return fmt.Errorf("cometbft/staking: failed transferring to active escrow balance from common pool: %w", err)
			}
			ctx.EmitEvent(abciAPI.NewEventBuilder(AppName).TypedAttribute(&staking.AddEscrowEvent{
				Owner:  staking.CommonPoolAddress,
				Escrow: addr,
				Amount: *q,
				// No new shares as this is the reward. As a result existing share price increases.
				NewShares: quantity.Quantity{},
			}))
		}

		if com != nil && !com.IsZero() {
			var delegation *staking.Delegation
			delegation, err = s.Delegation(ctx, addr, addr)
			if err != nil {
				return fmt.Errorf("cometbft/staking: failed to query delegation: %w", err)
			}

			var obtainedShares *quantity.Quantity
			obtainedShares, err = ent.Escrow.Active.Deposit(&delegation.Shares, commonPool, com)
			if err != nil {
				return fmt.Errorf("cometbft/staking: depositing commission: %w", err)
			}

			if err = s.SetDelegation(ctx, addr, addr, delegation); err != nil {
				return fmt.Errorf("cometbft/staking: failed to set delegation: %w", err)
			}

			// Above, we directly desposit from the common pool into the delegation,
			// which is a shorthand for transferring to the account and immediately
			// escrowing it. Explicitly emit both events.
			ctx.EmitEvent(abciAPI.NewEventBuilder(AppName).TypedAttribute(&staking.TransferEvent{
				From:   staking.CommonPoolAddress,
				To:     addr,
				Amount: *com,
			}))

			ctx.EmitEvent(abciAPI.NewEventBuilder(AppName).TypedAttribute(&staking.AddEscrowEvent{
				Owner:     addr,
				Escrow:    addr,
				Amount:    *com,
				NewShares: *obtainedShares,
			}))
		}

		if err = s.SetAccount(ctx, addr, ent); err != nil {
			return fmt.Errorf("cometbft/staking: failed to set account: %w", err)
		}
	}

	if err = s.SetCommonPool(ctx, commonPool); err != nil {
		return fmt.Errorf("cometbft/staking: failed to set common pool: %w", err)
	}

	return nil
}

// AddRewardSingleAttenuated computes, scales, and transfers a staking reward to an active escrow account.
func (s *MutableState) AddRewardSingleAttenuated(
	ctx *abciAPI.Context,
	time beacon.EpochTime,
	factor *quantity.Quantity,
	attenuationNumerator, attenuationDenominator int,
	address staking.Address,
) error {
	steps, err := s.RewardSchedule(ctx)
	if err != nil {
		return fmt.Errorf("failed to query reward schedule: %w", err)
	}
	var activeStep *staking.RewardStep
	for i, step := range steps {
		if time < step.Until {
			activeStep = &steps[i]
			break
		}
	}
	if activeStep == nil {
		// We're past the end of the schedule.
		return nil
	}

	var numQ, denQ quantity.Quantity
	if err = numQ.FromInt64(int64(attenuationNumerator)); err != nil {
		return fmt.Errorf("cometbft/staking: failed importing attenuation numerator %d: %w", attenuationNumerator, err)
	}
	if err = denQ.FromInt64(int64(attenuationDenominator)); err != nil {
		return fmt.Errorf("cometbft/staking: failed importing attenuation denominator %d: %w", attenuationDenominator, err)
	}

	commonPool, err := s.CommonPool(ctx)
	if err != nil {
		return fmt.Errorf("cometbft/staking: failed loading common pool: %w", err)
	}

	acct, err := s.Account(ctx, address)
	if err != nil {
		return fmt.Errorf("cometbft/staking: failed to query account %s: %w", address, err)
	}

	q := acct.Escrow.Active.Balance.Clone()
	// Multiply first.
	if err = q.Mul(factor); err != nil {
		return fmt.Errorf("cometbft/staking: failed multiplying by reward factor: %w", err)
	}
	if err = q.Mul(&activeStep.Scale); err != nil {
		return fmt.Errorf("cometbft/staking: failed multiplying by reward step scale: %w", err)
	}
	if err = q.Mul(&numQ); err != nil {
		return fmt.Errorf("cometbft/staking: failed multiplying by attenuation numerator: %w", err)
	}
	if err = q.Quo(staking.RewardAmountDenominator); err != nil {
		return fmt.Errorf("cometbft/staking: failed dividing by reward amount denominator: %w", err)
	}
	if err = q.Quo(&denQ); err != nil {
		return fmt.Errorf("cometbft/staking: failed dividing by attenuation denominator: %w", err)
	}

	if q.IsZero() {
		return nil
	}

	if q.Cmp(commonPool) == 1 {
		// Not enough balance left in common pool, skip.
		return nil
	}

	rate := acct.Escrow.CommissionSchedule.CurrentRate(time)
	com, q, err := s.computeCommission(ctx, rate, q)
	if err != nil {
		return err
	}

	if !q.IsZero() {
		if err = quantity.Move(&acct.Escrow.Active.Balance, commonPool, q); err != nil {
			return fmt.Errorf("cometbft/staking: failed transferring to active escrow balance from common pool: %w", err)
		}
		ctx.EmitEvent(abciAPI.NewEventBuilder(AppName).TypedAttribute(&staking.AddEscrowEvent{
			Owner:  staking.CommonPoolAddress,
			Escrow: address,
			Amount: *q,
			// No new shares as this is the reward. As a result existing share price increases.
			NewShares: quantity.Quantity{},
		}))
	}

	if com != nil && !com.IsZero() {
		var delegation *staking.Delegation
		delegation, err = s.Delegation(ctx, address, address)
		if err != nil {
			return fmt.Errorf("cometbft/staking: failed to query delegation: %w", err)
		}

		var obtainedShares *quantity.Quantity
		obtainedShares, err = acct.Escrow.Active.Deposit(&delegation.Shares, commonPool, com)
		if err != nil {
			return fmt.Errorf("cometbft/staking: failed depositing commission: %w", err)
		}

		if err = s.SetDelegation(ctx, address, address, delegation); err != nil {
			return fmt.Errorf("cometbft/staking: failed to set delegation: %w", err)
		}

		// Above, we directly desposit from the common pool into the delegation,
		// which is a shorthand for transferring to the account and immediately
		// escrowing it. Explicitly emit both events.
		ctx.EmitEvent(abciAPI.NewEventBuilder(AppName).TypedAttribute(&staking.TransferEvent{
			From:   staking.CommonPoolAddress,
			To:     address,
			Amount: *com,
		}))

		ctx.EmitEvent(abciAPI.NewEventBuilder(AppName).TypedAttribute(&staking.AddEscrowEvent{
			Owner:     address,
			Escrow:    address,
			Amount:    *com,
			NewShares: *obtainedShares,
		}))
	}

	if err = s.SetAccount(ctx, address, acct); err != nil {
		return fmt.Errorf("cometbft/staking: failed to set account: %w", err)
	}

	if err = s.SetCommonPool(ctx, commonPool); err != nil {
		return fmt.Errorf("cometbft/staking: failed to set common pool: %w", err)
	}

	return nil
}
