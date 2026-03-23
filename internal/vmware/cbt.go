package vmware

import (
	"context"
	"fmt"

	"github.com/rs/zerolog/log"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// CBTStatus holds diagnostic information about CBT on a VM.
type CBTStatus struct {
	Enabled bool
	Disks   []CBTDiskStatus
}

// CBTDiskStatus holds CBT status for a single disk.
type CBTDiskStatus struct {
	Key        int32
	ChangeID   string
	Controller string
}

// GetCBTStatus returns detailed CBT diagnostic info for a VM.
func (c *Client) GetCBTStatus(ctx context.Context, vm *object.VirtualMachine) (*CBTStatus, error) {
	var mvm mo.VirtualMachine
	pc := property.DefaultCollector(c.vimClient)
	err := pc.RetrieveOne(ctx, vm.Reference(), []string{"config.changeTrackingEnabled", "config.hardware.device"}, &mvm)
	if err != nil {
		return nil, fmt.Errorf("retrieving VM config: %w", err)
	}

	status := &CBTStatus{}
	if mvm.Config.ChangeTrackingEnabled != nil {
		status.Enabled = *mvm.Config.ChangeTrackingEnabled
	}

	for _, dev := range mvm.Config.Hardware.Device {
		if vd, ok := dev.(*types.VirtualDisk); ok {
			ds := CBTDiskStatus{Key: vd.Key}

			// Get changeId from backing
			if backing, ok := vd.Backing.(*types.VirtualDiskFlatVer2BackingInfo); ok {
				ds.ChangeID = backing.ChangeId
			}

			// Get controller type
			controllerKey := vd.ControllerKey
			for _, d := range mvm.Config.Hardware.Device {
				if d.GetVirtualDevice().Key == controllerKey {
					ds.Controller = fmt.Sprintf("%T", d)
					break
				}
			}

			status.Disks = append(status.Disks, ds)
		}
	}

	return status, nil
}

// EnableCBT enables Change Block Tracking on a VM.
func (c *Client) EnableCBT(ctx context.Context, vm *object.VirtualMachine) error {
	log.Info().Str("vm", vm.Name()).Msg("enabling CBT")

	spec := types.VirtualMachineConfigSpec{
		ChangeTrackingEnabled: types.NewBool(true),
	}

	task, err := vm.Reconfigure(ctx, spec)
	if err != nil {
		return fmt.Errorf("reconfiguring VM for CBT: %w", err)
	}

	if err := task.Wait(ctx); err != nil {
		return fmt.Errorf("waiting for CBT enable: %w", err)
	}

	log.Info().Msg("CBT enabled")
	return nil
}

// ResetCBT performs a full CBT reset: disable → stun/unstun → enable → stun/unstun.
// This is the standard procedure to fix CBT issues (e.g., after storage vMotion,
// or when QueryChangedDiskAreas fails with startOffset errors).
func (c *Client) ResetCBT(ctx context.Context, vm *object.VirtualMachine) error {
	log.Info().Str("vm", vm.Name()).Msg("resetting CBT (full disable/enable cycle)")

	// Step 1: Disable CBT
	disableSpec := types.VirtualMachineConfigSpec{
		ChangeTrackingEnabled: types.NewBool(false),
	}
	task, err := vm.Reconfigure(ctx, disableSpec)
	if err != nil {
		return fmt.Errorf("disabling CBT: %w", err)
	}
	if err := task.Wait(ctx); err != nil {
		return fmt.Errorf("waiting for CBT disable: %w", err)
	}
	log.Info().Msg("CBT disabled")

	// Step 2: Stun/unstun (snapshot create+delete) to clear CBT state
	snapRef, err := c.CreateSnapshot(ctx, vm, "datamigrate-cbt-reset-1", "CBT reset phase 1")
	if err != nil {
		return fmt.Errorf("CBT reset snapshot 1: %w", err)
	}
	if err := c.RemoveSnapshotByMoRef(ctx, snapRef.Value); err != nil {
		log.Warn().Err(err).Msg("failed to remove CBT reset snapshot 1")
	}

	// Step 3: Re-enable CBT
	enableSpec := types.VirtualMachineConfigSpec{
		ChangeTrackingEnabled: types.NewBool(true),
	}
	task, err = vm.Reconfigure(ctx, enableSpec)
	if err != nil {
		return fmt.Errorf("re-enabling CBT: %w", err)
	}
	if err := task.Wait(ctx); err != nil {
		return fmt.Errorf("waiting for CBT re-enable: %w", err)
	}
	log.Info().Msg("CBT re-enabled")

	// Step 4: Stun/unstun again to activate CBT tracking
	snapRef, err = c.CreateSnapshot(ctx, vm, "datamigrate-cbt-reset-2", "CBT reset phase 2")
	if err != nil {
		return fmt.Errorf("CBT reset snapshot 2: %w", err)
	}
	if err := c.RemoveSnapshotByMoRef(ctx, snapRef.Value); err != nil {
		log.Warn().Err(err).Msg("failed to remove CBT reset snapshot 2")
	}

	log.Info().Msg("CBT reset complete")
	return nil
}

// ChangedArea represents a range of changed blocks.
type ChangedArea struct {
	Offset int64
	Length  int64
}

// QueryChangedBlocks queries changed disk areas since a given changeId.
// Use changeId="*" for a full sync (returns all allocated areas).
// Returns the changed areas, the new changeId from the snapshot backing, and error.
func (c *Client) QueryChangedBlocks(ctx context.Context, vm *object.VirtualMachine, snapRef *types.ManagedObjectReference, disk DiskInfo, changeID string) ([]ChangedArea, string, error) {
	log.Info().
		Str("vm", vm.Name()).
		Int32("disk_key", disk.Key).
		Str("change_id", changeID).
		Msg("querying changed blocks")

	// Get the new changeId from the snapshot's disk backing (NOT the live VM config)
	newChangeID, err := c.GetSnapshotDiskChangeID(ctx, snapRef, disk.Key)
	if err != nil {
		log.Warn().Err(err).Msg("could not get snapshot disk changeId, using empty")
	}

	var allAreas []ChangedArea
	var startOffset int64

	for {
		req := types.QueryChangedDiskAreas{
			This:        vm.Reference(),
			Snapshot:    snapRef,
			DeviceKey:   disk.Key,
			StartOffset: startOffset,
			ChangeId:    changeID,
		}

		res, err := methods.QueryChangedDiskAreas(ctx, c.vimClient, &req)
		if err != nil {
			return nil, "", fmt.Errorf("querying changed disk areas: %w", err)
		}

		for _, area := range res.Returnval.ChangedArea {
			allAreas = append(allAreas, ChangedArea{
				Offset: area.Start,
				Length: area.Length,
			})
		}

		if len(res.Returnval.ChangedArea) == 0 {
			break
		}

		last := res.Returnval.ChangedArea[len(res.Returnval.ChangedArea)-1]
		nextOffset := last.Start + last.Length
		if nextOffset <= startOffset {
			break
		}
		startOffset = nextOffset
	}

	log.Info().
		Int("areas", len(allAreas)).
		Str("new_change_id", newChangeID).
		Msg("changed blocks queried")

	return allAreas, newChangeID, nil
}

// GetSnapshotDiskChangeID retrieves the changeId directly from a snapshot's device config.
// This is different from GetSnapshotChangeID which reads from the current VM config.
func (c *Client) GetSnapshotDiskChangeID(ctx context.Context, snapRef *types.ManagedObjectReference, diskKey int32) (string, error) {
	var snap mo.VirtualMachineSnapshot
	pc := property.DefaultCollector(c.vimClient)
	err := pc.RetrieveOne(ctx, *snapRef, []string{"config.hardware.device"}, &snap)
	if err != nil {
		return "", fmt.Errorf("retrieving snapshot config: %w", err)
	}

	for _, dev := range snap.Config.Hardware.Device {
		if vd, ok := dev.(*types.VirtualDisk); ok && vd.Key == diskKey {
			if backing, ok := vd.Backing.(*types.VirtualDiskFlatVer2BackingInfo); ok {
				log.Info().
					Int32("disk_key", diskKey).
					Str("change_id", backing.ChangeId).
					Msg("snapshot disk backing changeId")
				return backing.ChangeId, nil
			}
		}
	}

	return "", fmt.Errorf("disk %d not found in snapshot config", diskKey)
}

// GetSnapshotChangeID retrieves the changeId from the VM's disk backing.
// The changeId in the current VM config reflects the latest CBT tracking point.
func (c *Client) GetSnapshotChangeID(ctx context.Context, vm *object.VirtualMachine, snapRef *types.ManagedObjectReference, diskKey int32) (string, error) {
	var mvm mo.VirtualMachine
	pc := property.DefaultCollector(c.vimClient)
	err := pc.RetrieveOne(ctx, vm.Reference(), []string{"config.hardware.device", "config.changeTrackingEnabled"}, &mvm)
	if err != nil {
		return "", fmt.Errorf("retrieving VM config: %w", err)
	}

	// Log CBT status
	if mvm.Config.ChangeTrackingEnabled != nil {
		log.Info().Bool("cbt_enabled", *mvm.Config.ChangeTrackingEnabled).Msg("VM CBT status")
	} else {
		log.Warn().Msg("VM CBT status unknown (changeTrackingEnabled is nil)")
	}

	// Look through devices for the disk's backing changeId
	for _, dev := range mvm.Config.Hardware.Device {
		if vd, ok := dev.(*types.VirtualDisk); ok && vd.Key == diskKey {
			if backing, ok := vd.Backing.(*types.VirtualDiskFlatVer2BackingInfo); ok {
				changeId := backing.ChangeId
				if changeId == "" {
					log.Warn().Int32("disk_key", diskKey).Msg("disk backing has empty changeId — CBT may not be active")
				} else {
					log.Info().Int32("disk_key", diskKey).Str("change_id", changeId).Msg("captured changeId from disk backing")
				}
				return changeId, nil
			}
		}
	}

	return "", fmt.Errorf("disk %d not found or backing has no changeId", diskKey)
}
