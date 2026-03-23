package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/nitinmore/datamigrate/internal/config"
	"github.com/nitinmore/datamigrate/internal/state"
	"github.com/nitinmore/datamigrate/internal/vmware"
)

var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Remove snapshots and temporary artifacts",
	RunE:  runCleanup,
}

var cleanupPlanPath string

func init() {
	cleanupCmd.Flags().StringVar(&cleanupPlanPath, "plan", "", "Migration plan file")
	cleanupCmd.MarkFlagRequired("plan")
	rootCmd.AddCommand(cleanupCmd)
}

func runCleanup(cmd *cobra.Command, args []string) error {
	plan, err := config.LoadPlan(cleanupPlanPath)
	if err != nil {
		return fmt.Errorf("loading plan: %w", err)
	}

	ctx := context.Background()

	stagingDir := plan.Staging.Directory
	if stagingDir == "" {
		stagingDir = "/tmp/datamigrate"
	}

	// Load state to get tracked snapshot MoRefs
	stateFile := filepath.Join(stagingDir, "state.db")
	store, storeErr := state.NewStore(stateFile)
	var ms *state.MigrationState
	if storeErr == nil {
		ms, _ = store.GetMigration(plan.Name)
	}

	// Clean up VMware snapshots — only remove what we created (tracked by MoRef)
	if ms != nil && len(ms.Snapshots) > 0 {
		fmt.Print("Removing VMware snapshots...")
		vmClient, err := vmware.NewClient(ctx, vmware.ClientConfig{
			VCenter:  plan.Source.VCenter,
			Username: plan.Source.Username,
			Password: plan.Source.Password,
			Insecure: plan.Source.Insecure,
		})
		if err != nil {
			fmt.Printf(" FAILED to connect: %v\n", err)
		} else {
			for _, snap := range ms.Snapshots {
				fmt.Printf("\n  Removing %s (MoRef: %s)... ", snap.Name, snap.MoRef)
				if err := vmClient.RemoveSnapshotByMoRef(ctx, snap.MoRef); err != nil {
					fmt.Printf("FAILED: %v", err)
				} else {
					fmt.Print("OK")
				}
			}
			fmt.Println()
			vmClient.Logout(ctx)
		}
	} else {
		fmt.Println("Removing VMware snapshots... SKIPPED (no tracked snapshots in state)")
	}

	// Clean up local staging files
	planDir := filepath.Join(stagingDir, plan.Name)
	fmt.Printf("Removing staging files in %s... ", planDir)
	if err := os.RemoveAll(planDir); err != nil {
		fmt.Printf("FAILED: %v\n", err)
	} else {
		fmt.Println("OK")
	}

	// Clean up cutover qcow2 file
	qcow2Path := filepath.Join(os.TempDir(), fmt.Sprintf("%s-disk.qcow2", plan.VMName))
	if _, err := os.Stat(qcow2Path); err == nil {
		fmt.Printf("Removing cutover qcow2 %s... ", qcow2Path)
		if err := os.Remove(qcow2Path); err != nil {
			fmt.Printf("FAILED: %v\n", err)
		} else {
			fmt.Println("OK")
		}
	}

	// Clean up state (reuse store opened earlier)
	if store != nil {
		fmt.Print("Removing migration state... ")
		if err := store.DeleteMigration(plan.Name); err != nil {
			fmt.Printf("FAILED: %v\n", err)
		} else {
			fmt.Println("OK")
		}
		_ = store.ClearJournal(plan.Name)
		store.Close()

		// Remove state.db file itself
		fmt.Printf("Removing state file %s... ", stateFile)
		if err := os.Remove(stateFile); err != nil {
			fmt.Printf("FAILED: %v\n", err)
		} else {
			fmt.Println("OK")
		}
	}

	fmt.Println("\nCleanup complete.")
	return nil
}
