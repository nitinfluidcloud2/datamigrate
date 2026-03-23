package vmware

import (
	"context"
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

const snapshotPrefix = "datamigrate-"

// CreateSnapshot creates a snapshot on the given VM.
func (c *Client) CreateSnapshot(ctx context.Context, vm *object.VirtualMachine, name, description string) (*types.ManagedObjectReference, error) {
	log.Info().Str("vm", vm.Name()).Str("snapshot", name).Msg("creating snapshot")

	task, err := vm.CreateSnapshot(ctx, name, description, false, false)
	if err != nil {
		return nil, fmt.Errorf("creating snapshot: %w", err)
	}

	info, err := task.WaitForResult(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("waiting for snapshot creation: %w", err)
	}

	snapRef := info.Result.(types.ManagedObjectReference)
	log.Info().Str("snapshot_ref", snapRef.Value).Msg("snapshot created")
	return &snapRef, nil
}

// RemoveSnapshot removes a single snapshot by name.
func (c *Client) RemoveSnapshot(ctx context.Context, vm *object.VirtualMachine, name string) error {
	log.Info().Str("vm", vm.Name()).Str("snapshot", name).Msg("removing snapshot")

	consolidate := true
	task, err := vm.RemoveSnapshot(ctx, name, false, &consolidate)
	if err != nil {
		return fmt.Errorf("removing snapshot %q: %w", name, err)
	}

	if err := task.Wait(ctx); err != nil {
		return fmt.Errorf("waiting for snapshot removal: %w", err)
	}

	log.Info().Str("snapshot", name).Msg("snapshot removed")
	return nil
}

// snapshotInfo holds a snapshot's name and MoRef for targeted removal.
type snapshotInfo struct {
	Name string
	Ref  types.ManagedObjectReference
}

// RemoveDatamigrateSnapshots removes only snapshots created by datamigrate
// (names starting with "datamigrate-"). Leaves all other snapshots untouched.
// Uses MoRef-based removal to handle duplicate snapshot names correctly.
func (c *Client) RemoveDatamigrateSnapshots(ctx context.Context, vm *object.VirtualMachine) error {
	log.Info().Str("vm", vm.Name()).Msg("removing datamigrate snapshots")

	var mvm mo.VirtualMachine
	pc := property.DefaultCollector(c.vimClient)
	if err := pc.RetrieveOne(ctx, vm.Reference(), []string{"snapshot"}, &mvm); err != nil {
		return fmt.Errorf("retrieving snapshot info: %w", err)
	}

	if mvm.Snapshot == nil {
		log.Info().Msg("no snapshots found")
		return nil
	}

	// Collect all datamigrate snapshots with their MoRefs
	var snaps []snapshotInfo
	collectDatamigrateSnapshots(mvm.Snapshot.RootSnapshotList, &snaps)

	if len(snaps) == 0 {
		log.Info().Msg("no datamigrate snapshots found")
		return nil
	}

	log.Info().Int("count", len(snaps)).Msg("found datamigrate snapshots to remove")

	// Remove each one by MoRef (leaf-first to handle parent-child)
	for i := len(snaps) - 1; i >= 0; i-- {
		snap := snaps[i]
		log.Info().Str("snapshot", snap.Name).Str("ref", snap.Ref.Value).Msg("removing snapshot by MoRef")

		consolidate := true
		req := types.RemoveSnapshot_Task{
			This:         snap.Ref,
			RemoveChildren: false,
			Consolidate:   &consolidate,
		}
		res, err := methods.RemoveSnapshot_Task(ctx, c.vimClient, &req)
		if err != nil {
			log.Warn().Err(err).Str("snapshot", snap.Name).Msg("failed to remove snapshot")
			continue
		}

		task := object.NewTask(c.vimClient, res.Returnval)
		if err := task.Wait(ctx); err != nil {
			log.Warn().Err(err).Str("snapshot", snap.Name).Msg("snapshot removal task failed")
		}
	}

	return nil
}

// collectDatamigrateSnapshots walks the snapshot tree and collects snapshots
// that start with the datamigrate prefix, along with their MoRefs.
func collectDatamigrateSnapshots(trees []types.VirtualMachineSnapshotTree, snaps *[]snapshotInfo) {
	for _, tree := range trees {
		if strings.HasPrefix(tree.Name, snapshotPrefix) {
			*snaps = append(*snaps, snapshotInfo{
				Name: tree.Name,
				Ref:  tree.Snapshot,
			})
		}
		if len(tree.ChildSnapshotList) > 0 {
			collectDatamigrateSnapshots(tree.ChildSnapshotList, snaps)
		}
	}
}

// RemoveSnapshotByMoRef removes a single snapshot by its ManagedObjectReference value.
// This is the most reliable method — works even with duplicate snapshot names.
// Returns nil if the snapshot no longer exists (safe to call multiple times).
func (c *Client) RemoveSnapshotByMoRef(ctx context.Context, morefValue string) error {
	log.Info().Str("moref", morefValue).Msg("removing snapshot by MoRef")

	ref := types.ManagedObjectReference{
		Type:  "VirtualMachineSnapshot",
		Value: morefValue,
	}

	// Check if snapshot still exists before trying to remove
	var snap mo.VirtualMachineSnapshot
	pc := property.DefaultCollector(c.vimClient)
	if err := pc.RetrieveOne(ctx, ref, []string{"config"}, &snap); err != nil {
		log.Info().Str("moref", morefValue).Msg("snapshot already gone, skipping")
		return nil
	}

	consolidate := true
	req := types.RemoveSnapshot_Task{
		This:           ref,
		RemoveChildren: false,
		Consolidate:    &consolidate,
	}
	res, err := methods.RemoveSnapshot_Task(ctx, c.vimClient, &req)
	if err != nil {
		return fmt.Errorf("removing snapshot %s: %w", morefValue, err)
	}

	task := object.NewTask(c.vimClient, res.Returnval)
	if err := task.Wait(ctx); err != nil {
		return fmt.Errorf("waiting for snapshot removal %s: %w", morefValue, err)
	}

	log.Info().Str("moref", morefValue).Msg("snapshot removed")
	return nil
}

// RemoveAllSnapshots removes all snapshots from a VM.
// DEPRECATED: Use RemoveDatamigrateSnapshots instead to avoid deleting user snapshots.
func (c *Client) RemoveAllSnapshots(ctx context.Context, vm *object.VirtualMachine) error {
	log.Info().Str("vm", vm.Name()).Msg("removing all snapshots")

	consolidate := true
	task, err := vm.RemoveAllSnapshot(ctx, &consolidate)
	if err != nil {
		return fmt.Errorf("removing all snapshots: %w", err)
	}

	if err := task.Wait(ctx); err != nil {
		return fmt.Errorf("waiting for snapshot removal: %w", err)
	}

	log.Info().Msg("all snapshots removed")
	return nil
}
