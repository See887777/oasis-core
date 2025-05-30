// Package upgrade implements the node upgrade backend.
//
// After submitting an upgrade descriptor, the old node may continue
// running or be restarted up to the point when the consensus layer reaches
// the upgrade epoch. The new node may not be started until the old node has
// reached the upgrade epoch.
package upgrade

import (
	"fmt"
	"sync"

	beacon "github.com/oasisprotocol/oasis-core/go/beacon/api"
	"github.com/oasisprotocol/oasis-core/go/common/cbor"
	"github.com/oasisprotocol/oasis-core/go/common/logging"
	"github.com/oasisprotocol/oasis-core/go/common/persistent"
	"github.com/oasisprotocol/oasis-core/go/upgrade/api"
	"github.com/oasisprotocol/oasis-core/go/upgrade/migrations"
)

var (
	_ api.Backend = (*upgradeManager)(nil)

	metadataStoreKey = []byte("descriptors")
)

type upgradeManager struct {
	sync.Mutex

	store      *persistent.ServiceStore
	pending    []*api.PendingUpgrade
	shouldStop bool

	dataDir string

	logger *logging.Logger
}

// Implements api.Backend.
func (u *upgradeManager) SubmitDescriptor(descriptor *api.Descriptor) error {
	if descriptor == nil {
		return api.ErrBadDescriptor
	}

	u.Lock()
	defer u.Unlock()

	for _, pu := range u.pending {
		if pu.Descriptor.Equals(descriptor) {
			return api.ErrAlreadyPending
		}
	}

	pending := &api.PendingUpgrade{
		Versioned:  cbor.NewVersioned(api.LatestPendingUpgradeVersion),
		Descriptor: descriptor,
	}
	u.pending = append(u.pending, pending)

	u.logger.Info("received upgrade descriptor, scheduling shutdown",
		"handler", pending.Descriptor.Handler,
		"epoch", pending.Descriptor.Epoch,
	)

	return u.flushDescriptorLocked()
}

// Implements api.Backend.
func (u *upgradeManager) PendingUpgrades() ([]*api.PendingUpgrade, error) {
	u.Lock()
	defer u.Unlock()

	return append([]*api.PendingUpgrade{}, u.pending...), nil
}

// Implements api.Backend.
func (u *upgradeManager) HasPendingUpgradeAt(height int64) (bool, error) {
	u.Lock()
	defer u.Unlock()

	if height == api.InvalidUpgradeHeight {
		return false, fmt.Errorf("invalid upgrade height specified")
	}

	for _, pu := range u.pending {
		if pu.IsCompleted() || pu.UpgradeHeight == api.InvalidUpgradeHeight || pu.UpgradeHeight != height {
			continue
		}
		return true, nil
	}
	return false, nil
}

// Implements api.Backend.
func (u *upgradeManager) CancelUpgrade(descriptor *api.Descriptor) error {
	if descriptor == nil {
		return api.ErrBadDescriptor
	}

	u.Lock()
	defer u.Unlock()

	if len(u.pending) == 0 {
		// Make sure nothing is saved.
		return u.flushDescriptorLocked()
	}

	var pending []*api.PendingUpgrade
	for _, pu := range u.pending {
		if !pu.Descriptor.Equals(descriptor) {
			pending = append(pending, pu)
			continue
		}
		if pu.UpgradeHeight != api.InvalidUpgradeHeight || pu.HasAnyStages() {
			return api.ErrUpgradeInProgress
		}
	}
	oldPending := u.pending
	u.pending = pending
	if err := u.flushDescriptorLocked(); err != nil {
		u.pending = oldPending
		return err
	}
	return nil
}

// Implements api.Backend.
func (u *upgradeManager) GetUpgrade(descriptor *api.Descriptor) (*api.PendingUpgrade, error) {
	if descriptor == nil {
		return nil, api.ErrBadDescriptor
	}

	u.Lock()
	defer u.Unlock()

	for _, pu := range u.pending {
		if pu.Descriptor.Equals(descriptor) {
			return pu, nil
		}
	}
	return nil, api.ErrUpgradeNotFound
}

func (u *upgradeManager) checkStatus() error {
	u.Lock()
	defer u.Unlock()
	var err error

	if err = u.store.GetCBOR(metadataStoreKey, &u.pending); err != nil {
		u.pending = nil
		if err == persistent.ErrNotFound {
			// No upgrade pending, nothing to do.
			u.logger.Debug("no pending descriptors, continuing startup")
			return nil
		}
		return fmt.Errorf("can't decode stored upgrade descriptors: %w", err)
	}

	for _, pu := range u.pending {
		if pu.IsCompleted() {
			continue
		}

		// Check if upgrade should proceed.
		if pu.UpgradeHeight == api.InvalidUpgradeHeight {
			continue
		}

		// The upgrade should proceed right now. Check that we have the right binary.
		if err = pu.Descriptor.EnsureCompatible(); err != nil {
			u.logger.Error("incompatible binary version for upgrade",
				"handler", pu.Descriptor.Handler,
				"err", err,
				logging.LogEvent, api.LogEventIncompatibleBinary,
			)
			return err
		}

		// Ensure the upgrade handler exists.
		if _, err = migrations.GetHandler(pu.Descriptor.Handler); err != nil {
			u.logger.Error("error getting migration handler for upgrade",
				"handler", pu.Descriptor.Handler,
				"err", err,
			)
			return err
		}
	}

	if err = u.flushDescriptorLocked(); err != nil {
		return err
	}

	u.logger.Info("loaded pending upgrade metadata",
		"pending", u.pending,
	)

	return nil
}

// NOTE: Assumes lock is held.
func (u *upgradeManager) flushDescriptorLocked() error {
	// Go over pending upgrades and check if any are completed.
	var pending []*api.PendingUpgrade
	for _, pu := range u.pending {
		if pu.IsCompleted() {
			u.logger.Info("upgrade completed, removing state",
				"handler", pu.Descriptor.Handler,
			)
			continue
		}
		pending = append(pending, pu)
	}
	u.pending = pending

	// Delete the state if there's no pending upgrades.
	if len(u.pending) == 0 {
		if err := u.store.Delete(metadataStoreKey); err != persistent.ErrNotFound {
			return err
		}
		return nil
	}

	return u.store.PutCBOR(metadataStoreKey, u.pending)
}

// Implements api.Backend.
func (u *upgradeManager) StartupUpgrade() error {
	u.Lock()
	defer u.Unlock()

	for _, pu := range u.pending {
		if pu.UpgradeHeight == api.InvalidUpgradeHeight {
			continue
		}
		if pu.HasStage(api.UpgradeStageStartup) {
			u.logger.Warn("startup upgrade already performed, skipping",
				"handler", pu.Descriptor.Handler,
			)
			continue
		}

		// Execute the statup stage.
		u.logger.Warn("performing startup upgrade",
			"handler", pu.Descriptor.Handler,
			logging.LogEvent, api.LogEventStartupUpgrade,
		)
		handler, err := migrations.GetHandler(pu.Descriptor.Handler)
		if err != nil {
			return err
		}
		if err := handler.StartupUpgrade(); err != nil {
			return err
		}
		pu.PushStage(api.UpgradeStageStartup)
	}

	return u.flushDescriptorLocked()
}

// Implements api.Backend.
func (u *upgradeManager) ConsensusUpgrade(privateCtx any, currentEpoch beacon.EpochTime, currentHeight int64) error {
	u.Lock()
	defer u.Unlock()

	// In case this method has been re-entered before the node has actually stopped, make sure that
	// we don't simply proceed with the upgrade before stopping.
	if u.shouldStop {
		return api.ErrStopForUpgrade
	}

	for _, pu := range u.pending {
		// If we haven't reached the upgrade epoch yet, we run normally;
		// startup made sure we're an appropriate binary for that.
		if pu.UpgradeHeight == api.InvalidUpgradeHeight {
			if currentEpoch < pu.Descriptor.Epoch {
				continue
			}
			pu.UpgradeHeight = currentHeight
			if err := u.flushDescriptorLocked(); err != nil {
				return err
			}
			// Check if we can proceed in place (e.g. without a restart).
			u.shouldStop = func() bool {
				if err := pu.Descriptor.EnsureCompatible(); err != nil {
					// Not compatible, we must stop.
					return true
				}
				// Check if the migration handler has a startup stage.
				handler, err := migrations.GetHandler(pu.Descriptor.Handler)
				if err != nil {
					// Handler not available, we must stop.
					return true
				}
				if handler.HasStartupUpgrade() {
					// Handler has a startup upgrade, we must stop.
					return true
				}
				return false
			}()
			if u.shouldStop {
				return api.ErrStopForUpgrade
			}
			// We can continue with the upgrade in place.
			u.logger.Info("skipping node restart as no startup upgrade stage needed")
			pu.PushStage(api.UpgradeStageStartup)
		}

		// If we're already past the upgrade height, then everything must be complete.
		if pu.UpgradeHeight < currentHeight {
			pu.PushStage(api.UpgradeStageConsensus)
			continue
		}

		if pu.UpgradeHeight > currentHeight {
			panic("consensus upgrade: UpgradeHeight is in the future but upgrade epoch seen already")
		}

		if !pu.HasStage(api.UpgradeStageConsensus) && privateCtx != nil {
			u.logger.Warn("performing consensus upgrade",
				"handler", pu.Descriptor.Handler,
				logging.LogEvent, api.LogEventConsensusUpgrade,
			)

			handler, err := migrations.GetHandler(pu.Descriptor.Handler)
			if err != nil {
				return err
			}
			if err := handler.ConsensusUpgrade(privateCtx); err != nil {
				return err
			}
		}
	}

	return u.flushDescriptorLocked()
}

// Implements api.Backend.
func (u *upgradeManager) Close() {
	u.Lock()
	defer u.Unlock()
	_ = u.flushDescriptorLocked()
	u.store.Close()
}

// New constructs and returns a new upgrade manager. It potentially also checks for and loads any
// pending upgrade descriptors; if this node is not the one intended to be run according
// to the loaded descriptor, New will return an error.
func New(store *persistent.CommonStore, dataDir string, checkStatus bool) (api.Backend, error) {
	svcStore := store.GetServiceStore(api.ModuleName)

	upgrader := &upgradeManager{
		store:   svcStore,
		dataDir: dataDir,
		logger:  logging.GetLogger(api.ModuleName),
	}

	if checkStatus {
		if err := upgrader.checkStatus(); err != nil {
			return nil, err
		}
	}

	return upgrader, nil
}
