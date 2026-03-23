package migration

import (
	"context"
	"fmt"

	"github.com/rs/zerolog/log"

	"github.com/nitinmore/datamigrate/internal/nutanix"
	"github.com/nitinmore/datamigrate/internal/state"
	"github.com/vmware/govmomi/object"
)

// CutoverOptions holds options for the cutover operation.
type CutoverOptions struct {
	ShutdownSource bool
}

// Cutover performs the final sync, shuts down the source VM, and creates the target VM on AHV.
func (o *Orchestrator) Cutover(ctx context.Context, opts CutoverOptions) error {
	ms, err := o.GetState()
	if err != nil {
		return fmt.Errorf("getting migration state: %w", err)
	}

	if ms.Status != state.StatusCutoverReady && ms.Status != state.StatusSyncing {
		return fmt.Errorf("cutover requires CUTOVER_READY or SYNCING state, currently %s", ms.Status)
	}

	// Final incremental sync
	log.Info().Msg("performing final incremental sync before cutover")
	if err := o.IncrementalSync(ctx); err != nil {
		o.SetError(ms, err)
		return fmt.Errorf("final incremental sync: %w", err)
	}

	// Refresh state after sync
	ms, err = o.GetState()
	if err != nil {
		return err
	}

	if err := o.TransitionTo(ms, state.StatusCuttingOver); err != nil {
		return err
	}

	// Power off source VM
	if opts.ShutdownSource {
		vm, _, err := o.vmClient.FindVM(ctx, "", o.plan.VMName)
		if err != nil {
			o.SetError(ms, err)
			return fmt.Errorf("finding source VM: %w", err)
		}

		if err := powerOffVM(ctx, vm); err != nil {
			o.SetError(ms, err)
			return fmt.Errorf("powering off source VM: %w", err)
		}

		// One more delta sync after shutdown to capture final writes
		log.Info().Msg("performing post-shutdown sync")
		if err := o.doFinalDeltaSync(ctx, ms); err != nil {
			log.Warn().Err(err).Msg("post-shutdown sync failed, continuing with last sync state")
		}
	}

	// Create VM on Nutanix AHV
	vmUUID, err := o.createTargetVM(ctx, ms)
	if err != nil {
		o.SetError(ms, err)
		return fmt.Errorf("creating target VM: %w", err)
	}

	// Power on the target VM
	if err := o.nxClient.PowerOnVM(ctx, vmUUID); err != nil {
		o.SetError(ms, err)
		return fmt.Errorf("powering on target VM: %w", err)
	}

	// Transition to completed
	if err := o.TransitionTo(ms, state.StatusCompleted); err != nil {
		return err
	}

	log.Info().
		Str("vm_uuid", vmUUID).
		Str("transport", string(ms.Transport)).
		Msg("cutover complete — VM is running on Nutanix AHV")

	return nil
}

func powerOffVM(ctx context.Context, vm *object.VirtualMachine) error {
	log.Info().Str("vm", vm.Name()).Msg("powering off source VM")

	task, err := vm.PowerOff(ctx)
	if err != nil {
		return fmt.Errorf("initiating power off: %w", err)
	}

	if err := task.Wait(ctx); err != nil {
		log.Warn().Err(err).Msg("power off task error (VM may already be off)")
	}

	return nil
}

func (o *Orchestrator) doFinalDeltaSync(ctx context.Context, ms *state.MigrationState) error {
	origStatus := ms.Status
	ms.Status = state.StatusSyncing
	if err := o.store.SaveMigration(ms); err != nil {
		return err
	}

	err := o.IncrementalSync(ctx)

	ms.Status = origStatus
	_ = o.store.SaveMigration(ms)

	return err
}

func (o *Orchestrator) createTargetVM(ctx context.Context, ms *state.MigrationState) (string, error) {
	log.Info().Str("vm", o.plan.VMName).Msg("creating target VM on Nutanix AHV")

	// Build disk list — depends on transport mode
	var disks []nutanix.VMDisk

	// Both transport modes now use image references
	for i, disk := range ms.Disks {
		if disk.ImageUUID == "" {
			return "", fmt.Errorf("disk %d has no image UUID", disk.Key)
		}
		disks = append(disks, nutanix.VMDisk{
			DataSourceRef: &nutanix.ResourceRef{
				Kind: "image",
				UUID: disk.ImageUUID,
			},
			DeviceProps: &nutanix.DeviceProps{
				DeviceType: "DISK",
				DiskAddr: &nutanix.DiskAddr{
					AdapterType: "SCSI",
					DeviceIndex: i,
				},
			},
		})
	}

	// Build NIC list from network mappings
	var nics []nutanix.VMNIC
	for _, nm := range o.plan.NetworkMap {
		nics = append(nics, nutanix.VMNIC{
			SubnetRef: &nutanix.ResourceRef{
				Kind: "subnet",
				UUID: nm.Target,
			},
		})
	}

	numCPUs := o.plan.TargetVMSpec.NumCPUs
	if numCPUs == 0 {
		numCPUs = 2
	}
	memoryMB := o.plan.TargetVMSpec.MemoryMB
	if memoryMB == 0 {
		memoryMB = 4096
	}

	spec := nutanix.VMCreateSpec{
		Spec: nutanix.VMSpec{
			Name:        o.plan.VMName,
			Description: fmt.Sprintf("Migrated from VMware by datamigrate (plan: %s)", o.plan.Name),
			Resources: nutanix.VMResources{
				NumSockets:      numCPUs,
				NumVCPUsPerSock: 1,
				MemoryMB:        memoryMB,
				PowerState:      "OFF",
				DiskList:        disks,
				NICList:         nics,
			},
		},
		Metadata: nutanix.Metadata{Kind: "vm"},
	}

	clusterUUID := o.plan.TargetVMSpec.ClusterUUID
	if clusterUUID == "" {
		clusterUUID = o.plan.Target.ClusterUUID
	}
	if clusterUUID != "" {
		spec.Spec.ClusterRef = &nutanix.ResourceRef{
			Kind: "cluster",
			UUID: clusterUUID,
		}
	}

	return o.nxClient.CreateVM(ctx, spec)
}
