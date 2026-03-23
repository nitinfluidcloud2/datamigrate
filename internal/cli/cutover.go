package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nitinmore/datamigrate/internal/migration"
)

var cutoverCmd = &cobra.Command{
	Use:   "cutover",
	Short: "Final sync + boot VM on AHV",
	Long:  "Performs the final incremental sync, optionally shuts down the source VM, creates the VM on Nutanix AHV, and powers it on.",
	RunE:  runCutover,
}

var (
	cutoverPlanPath     string
	cutoverShutdownSrc  bool
)

func init() {
	cutoverCmd.Flags().StringVar(&cutoverPlanPath, "plan", "", "Migration plan file")
	cutoverCmd.Flags().BoolVar(&cutoverShutdownSrc, "shutdown-source", false, "Shutdown source VM before cutover")

	cutoverCmd.MarkFlagRequired("plan")

	rootCmd.AddCommand(cutoverCmd)
}

func runCutover(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	orch, _, err := setupOrchestrator(ctx, cutoverPlanPath)
	if err != nil {
		return err
	}

	fmt.Println("Starting cutover...")

	if err := orch.Cutover(ctx, migration.CutoverOptions{
		ShutdownSource: cutoverShutdownSrc,
	}); err != nil {
		return fmt.Errorf("cutover failed: %w", err)
	}

	fmt.Println("Cutover complete! VM is now running on Nutanix AHV.")
	return nil
}
