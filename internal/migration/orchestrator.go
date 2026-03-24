package migration

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/vmware/govmomi/object"

	"github.com/nitinmore/datamigrate/internal/config"
	"github.com/nitinmore/datamigrate/internal/nutanix"
	"github.com/nitinmore/datamigrate/internal/state"
	"github.com/nitinmore/datamigrate/internal/vmware"
)

// Orchestrator coordinates the migration state machine.
type Orchestrator struct {
	plan     *config.MigrationPlan
	store    *state.Store
	vmClient *vmware.Client
	nxClient *nutanix.Client
}

// NewOrchestrator creates a new migration orchestrator.
func NewOrchestrator(plan *config.MigrationPlan, store *state.Store, vmClient *vmware.Client, nxClient *nutanix.Client) *Orchestrator {
	return &Orchestrator{
		plan:     plan,
		store:    store,
		vmClient: vmClient,
		nxClient: nxClient,
	}
}

// Initialize creates the initial migration state.
func (o *Orchestrator) Initialize(ctx context.Context) (*state.MigrationState, error) {
	log.Info().Str("plan", o.plan.Name).Str("vm", o.plan.VMName).Msg("initializing migration")

	// Find the VM on vCenter
	_, vmInfo, err := o.vmClient.FindVM(ctx, "", o.plan.VMName)
	if err != nil {
		return nil, fmt.Errorf("finding VM: %w", err)
	}

	// Build disk states
	var disks []state.DiskState
	for _, d := range vmInfo.Disks {
		disks = append(disks, state.DiskState{
			Key:       d.Key,
			FileName:  d.FileName,
			CapacityB: d.CapacityKB * 1024,
		})
	}

	// Determine transport mode from plan
	var transport state.TransportMode
	switch o.plan.Transport {
	case "iscsi":
		transport = state.TransportISCSI
	case "stream":
		transport = state.TransportStream
	case "image":
		transport = state.TransportImage
	case "repository":
		transport = state.TransportRepository
	default:
		transport = state.TransportStream // default: works from any OS
	}

	ms := &state.MigrationState{
		PlanName:  o.plan.Name,
		VMName:    o.plan.VMName,
		Status:    state.StatusCreated,
		Transport: transport,
		Disks:     disks,
		CreatedAt: time.Now(),
	}

	log.Info().
		Str("transport", string(transport)).
		Msg("transport mode selected")

	if err := o.store.SaveMigration(ms); err != nil {
		return nil, fmt.Errorf("saving initial state: %w", err)
	}

	log.Info().
		Str("plan", o.plan.Name).
		Int("disks", len(disks)).
		Msg("migration initialized")

	return ms, nil
}

// GetState retrieves the current migration state.
func (o *Orchestrator) GetState() (*state.MigrationState, error) {
	return o.store.GetMigration(o.plan.Name)
}

// TransitionTo updates the migration status with validation.
func (o *Orchestrator) TransitionTo(ms *state.MigrationState, newStatus state.MigrationStatus) error {
	validTransitions := map[state.MigrationStatus][]state.MigrationStatus{
		state.StatusCreated:      {state.StatusFullSync, state.StatusFailed},
		state.StatusFullSync:     {state.StatusSyncing, state.StatusFailed},
		state.StatusFailed:       {state.StatusFullSync, state.StatusSyncing, state.StatusFailed}, // allow retry after failure
		state.StatusSyncing:      {state.StatusSyncing, state.StatusCutoverReady, state.StatusFailed},
		state.StatusCutoverReady: {state.StatusCuttingOver, state.StatusSyncing, state.StatusFailed},
		state.StatusCuttingOver:  {state.StatusCompleted, state.StatusFailed},
	}

	allowed, ok := validTransitions[ms.Status]
	if !ok {
		return fmt.Errorf("no transitions from %s", ms.Status)
	}

	valid := false
	for _, s := range allowed {
		if s == newStatus {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("invalid transition: %s -> %s", ms.Status, newStatus)
	}

	log.Info().
		Str("from", string(ms.Status)).
		Str("to", string(newStatus)).
		Msg("migration state transition")

	ms.Status = newStatus
	return o.store.SaveMigration(ms)
}

// SetError records an error and transitions to FAILED.
func (o *Orchestrator) SetError(ms *state.MigrationState, err error) error {
	ms.Status = state.StatusFailed
	ms.Error = err.Error()
	return o.store.SaveMigration(ms)
}

// removeTrackedSnapshots removes snapshots we created (stored in state by MoRef)
// and clears them from state. Falls back to name-based removal if MoRefs are missing.
func (o *Orchestrator) removeTrackedSnapshots(ctx context.Context, vm *object.VirtualMachine, ms *state.MigrationState) {
	if len(ms.Snapshots) == 0 {
		// No tracked snapshots — fall back to name-based removal
		log.Warn().Msg("no tracked snapshots in state, falling back to name-based removal")
		if err := o.vmClient.RemoveDatamigrateSnapshots(ctx, vm); err != nil {
			log.Warn().Err(err).Msg("failed to remove snapshots")
		}
		return
	}

	for _, snap := range ms.Snapshots {
		log.Info().Str("snapshot", snap.Name).Str("moref", snap.MoRef).Msg("removing tracked snapshot")
		if err := o.vmClient.RemoveSnapshotByMoRef(ctx, snap.MoRef); err != nil {
			log.Warn().Err(err).Str("snapshot", snap.Name).Msg("failed to remove snapshot")
		}
	}

	// Clear snapshots from state
	ms.Snapshots = nil
	if err := o.store.SaveMigration(ms); err != nil {
		log.Warn().Err(err).Msg("failed to clear snapshot refs from state")
	}
}
