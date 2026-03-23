package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/nitinmore/datamigrate/internal/config"
	"github.com/nitinmore/datamigrate/internal/migration"
	"github.com/nitinmore/datamigrate/internal/nutanix"
	"github.com/nitinmore/datamigrate/internal/state"
	"github.com/nitinmore/datamigrate/internal/util"
	"github.com/nitinmore/datamigrate/internal/vmware"
)

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Manage VM migration",
}

var migrateStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start full sync (T0)",
	RunE:  runMigrateStart,
}

var migrateSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Run incremental sync (T1..TN)",
	RunE:  runMigrateSync,
}

var migrateStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show migration status",
	RunE:  runMigrateStatus,
}

var migrateFixStateCmd = &cobra.Command{
	Use:   "fixstate",
	Short: "Capture missing changeID from VMware (no data transfer)",
	Long:  "Creates a temporary snapshot, captures the CBT changeID, saves it to state, and removes the snapshot. Use this if T0 completed but changeID was not saved.",
	RunE:  runMigrateFixState,
}

var migratePlanPath string

func init() {
	migrateStartCmd.Flags().StringVar(&migratePlanPath, "plan", "", "Migration plan file")
	migrateSyncCmd.Flags().StringVar(&migratePlanPath, "plan", "", "Migration plan file")
	migrateStatusCmd.Flags().StringVar(&migratePlanPath, "plan", "", "Migration plan file")
	migrateFixStateCmd.Flags().StringVar(&migratePlanPath, "plan", "", "Migration plan file")

	migrateStartCmd.MarkFlagRequired("plan")
	migrateSyncCmd.MarkFlagRequired("plan")
	migrateStatusCmd.MarkFlagRequired("plan")
	migrateFixStateCmd.MarkFlagRequired("plan")

	migrateCmd.AddCommand(migrateStartCmd)
	migrateCmd.AddCommand(migrateSyncCmd)
	migrateCmd.AddCommand(migrateStatusCmd)
	migrateCmd.AddCommand(migrateFixStateCmd)
	rootCmd.AddCommand(migrateCmd)
}

func setupOrchestrator(ctx context.Context, planPath string) (*migration.Orchestrator, *config.MigrationPlan, error) {
	plan, err := config.LoadPlan(planPath)
	if err != nil {
		return nil, nil, fmt.Errorf("loading plan: %w", err)
	}

	// Open state store
	stateDir := plan.Staging.Directory
	if stateDir == "" {
		stateDir = "/tmp/datamigrate"
	}
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return nil, nil, fmt.Errorf("creating state dir: %w", err)
	}

	store, err := state.NewStore(filepath.Join(stateDir, "state.db"))
	if err != nil {
		return nil, nil, fmt.Errorf("opening state store: %w", err)
	}
	if err := store.InitJournal(); err != nil {
		store.Close()
		return nil, nil, fmt.Errorf("initializing journal: %w", err)
	}

	vmClient, err := vmware.NewClient(ctx, vmware.ClientConfig{
		VCenter:  plan.Source.VCenter,
		Username: plan.Source.Username,
		Password: plan.Source.Password,
		Insecure: plan.Source.Insecure,
	})
	if err != nil {
		store.Close()
		return nil, nil, fmt.Errorf("connecting to vCenter: %w", err)
	}

	nxClient := nutanix.NewClient(nutanix.ClientConfig{
		PrismCentral: plan.Target.PrismCentral,
		Username:     plan.Target.Username,
		Password:     plan.Target.Password,
		Insecure:     plan.Target.Insecure,
	})

	orch := migration.NewOrchestrator(plan, store, vmClient, nxClient)
	return orch, plan, nil
}

func runMigrateStart(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	orch, plan, err := setupOrchestrator(ctx, migratePlanPath)
	if err != nil {
		return err
	}

	// Check if there's an existing failed migration to retry
	existing, err := orch.GetState()
	var ms *state.MigrationState
	if err == nil && existing != nil && existing.Status == state.StatusFailed {
		fmt.Printf("Found previous failed migration for %s, retrying...\n", plan.Name)
		ms = existing
	} else {
		// Initialize migration state
		ms, err = orch.Initialize(ctx)
		if err != nil {
			return fmt.Errorf("initializing migration: %w", err)
		}
	}

	fmt.Printf("Migration initialized: %s\n", plan.Name)
	fmt.Printf("VM: %s, Disks: %d\n", ms.VMName, len(ms.Disks))
	fmt.Println("Starting full sync (T0)...")

	if err := orch.FullSync(ctx); err != nil {
		return fmt.Errorf("full sync failed: %w", err)
	}

	fmt.Println("Full sync complete. Run 'datamigrate migrate sync' for incremental syncs.")
	return nil
}

func runMigrateSync(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	orch, _, err := setupOrchestrator(ctx, migratePlanPath)
	if err != nil {
		return err
	}

	fmt.Println("Starting incremental sync...")

	if err := orch.IncrementalSync(ctx); err != nil {
		return fmt.Errorf("incremental sync failed: %w", err)
	}

	fmt.Println("Incremental sync complete.")
	return nil
}

func runMigrateStatus(cmd *cobra.Command, args []string) error {
	plan, err := config.LoadPlan(migratePlanPath)
	if err != nil {
		return fmt.Errorf("loading plan: %w", err)
	}

	stateDir := plan.Staging.Directory
	if stateDir == "" {
		stateDir = "/tmp/datamigrate"
	}

	store, err := state.NewStore(filepath.Join(stateDir, "state.db"))
	if err != nil {
		return fmt.Errorf("opening state store: %w", err)
	}
	defer store.Close()

	ms, err := store.GetMigration(plan.Name)
	if err != nil {
		return fmt.Errorf("migration not found: %w", err)
	}

	fmt.Printf("Migration: %s\n", ms.PlanName)
	fmt.Printf("VM: %s\n", ms.VMName)
	fmt.Printf("Status: %s\n", ms.Status)
	fmt.Printf("Transport: %s\n", ms.Transport)
	fmt.Printf("Sync Count: %d\n", ms.SyncCount)
	fmt.Printf("Created: %s\n", ms.CreatedAt.Format("2006-01-02 15:04:05"))
	fmt.Printf("Updated: %s\n", ms.UpdatedAt.Format("2006-01-02 15:04:05"))

	if ms.VolumeGroupID != "" {
		fmt.Printf("Volume Group: %s\n", ms.VolumeGroupID)
	}
	if ms.Error != "" {
		fmt.Printf("Error: %s\n", ms.Error)
	}

	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "DISK\tFILE\tCAPACITY\tCOPIED\tIMAGE UUID\tLAST SYNC")
	fmt.Fprintln(w, "----\t----\t--------\t------\t----------\t---------")

	for _, d := range ms.Disks {
		lastSync := "never"
		if !d.LastSyncedAt.IsZero() {
			lastSync = d.LastSyncedAt.Format("15:04:05")
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n",
			d.Key,
			d.FileName,
			util.HumanSize(d.CapacityB),
			util.HumanSize(d.BytesCopied),
			truncate(d.ImageUUID, 12),
			lastSync,
		)
	}
	w.Flush()

	return nil
}

func runMigrateFixState(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	plan, err := config.LoadPlan(migratePlanPath)
	if err != nil {
		return fmt.Errorf("loading plan: %w", err)
	}

	// Open state store
	stateDir := plan.Staging.Directory
	if stateDir == "" {
		stateDir = "/tmp/datamigrate"
	}
	store, err := state.NewStore(filepath.Join(stateDir, "state.db"))
	if err != nil {
		return fmt.Errorf("opening state store: %w", err)
	}
	defer store.Close()

	ms, err := store.GetMigration(plan.Name)
	if err != nil {
		return fmt.Errorf("migration not found: %w", err)
	}

	// Check if changeID is already present
	allPresent := true
	for _, d := range ms.Disks {
		if d.ChangeID == "" {
			allPresent = false
			break
		}
	}
	if allPresent {
		fmt.Println("All disks already have changeIDs. No fix needed.")
		return nil
	}

	// Connect to vCenter
	vmClient, err := vmware.NewClient(ctx, vmware.ClientConfig{
		VCenter:  plan.Source.VCenter,
		Username: plan.Source.Username,
		Password: plan.Source.Password,
		Insecure: plan.Source.Insecure,
	})
	if err != nil {
		return fmt.Errorf("connecting to vCenter: %w", err)
	}

	vm, _, err := vmClient.FindVM(ctx, "", plan.VMName)
	if err != nil {
		return fmt.Errorf("finding VM: %w", err)
	}

	// Ensure CBT is enabled
	fmt.Println("Ensuring CBT is enabled...")
	if err := vmClient.EnableCBT(ctx, vm); err != nil {
		return fmt.Errorf("enabling CBT: %w", err)
	}

	// Create temporary snapshot to capture changeID
	snapName := fmt.Sprintf("datamigrate-fixstate-%s", plan.Name)
	fmt.Printf("Creating temporary snapshot %q...\n", snapName)
	snapRef, err := vmClient.CreateSnapshot(ctx, vm, snapName, "Temporary snapshot to capture CBT changeID")
	if err != nil {
		return fmt.Errorf("creating snapshot: %w", err)
	}

	// Capture changeID for each disk
	for i := range ms.Disks {
		disk := &ms.Disks[i]
		if disk.ChangeID != "" {
			fmt.Printf("  Disk %d: already has changeID %s\n", disk.Key, disk.ChangeID)
			continue
		}

		changeID, err := vmClient.GetSnapshotDiskChangeID(ctx, snapRef, disk.Key)
		if err != nil {
			fmt.Printf("  Disk %d: ERROR getting changeID: %v\n", disk.Key, err)
			continue
		}

		disk.ChangeID = changeID
		fmt.Printf("  Disk %d: captured changeID %s\n", disk.Key, changeID)
	}

	// Save updated state
	if err := store.SaveMigration(ms); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}
	fmt.Println("State updated in BoltDB.")

	// Remove only the snapshot we just created
	fmt.Println("Removing temporary snapshot...")
	if err := vmClient.RemoveSnapshot(ctx, vm, snapName); err != nil {
		fmt.Printf("Warning: could not remove snapshot: %v\n", err)
		fmt.Println("Please remove it manually from vCenter.")
	}

	fmt.Println("\nDone! You can now run 'datamigrate migrate sync' for incremental sync.")
	return nil
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
