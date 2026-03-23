package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nitinmore/datamigrate/internal/config"
	"github.com/nitinmore/datamigrate/internal/nutanix"
	"github.com/nitinmore/datamigrate/internal/vmware"
)

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate source and target connectivity",
	RunE:  runValidate,
}

var validateCredsDir string

func init() {
	validateCmd.Flags().StringVar(&validateCredsDir, "creds-dir", "configs", "Directory containing vmware.creds and nutanix.creds")
	rootCmd.AddCommand(validateCmd)
}

func runValidate(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(validateCredsDir)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	ctx := context.Background()
	hasError := false

	// Validate VMware connection
	fmt.Print("Validating VMware vCenter connection... ")
	vmClient, err := vmware.NewClient(ctx, vmware.ClientConfig{
		VCenter:  cfg.Source.VCenter,
		Username: cfg.Source.Username,
		Password: cfg.Source.Password,
		Insecure: cfg.Source.Insecure,
	})
	if err != nil {
		fmt.Printf("FAILED: %v\n", err)
		hasError = true
	} else {
		fmt.Println("OK")
		vmClient.Logout(ctx)
	}

	// Validate Nutanix connection
	fmt.Print("Validating Nutanix Prism Central connection... ")
	nxClient := nutanix.NewClient(nutanix.ClientConfig{
		PrismCentral: cfg.Target.PrismCentral,
		Username:     cfg.Target.Username,
		Password:     cfg.Target.Password,
		Insecure:     cfg.Target.Insecure,
	})
	if err := nxClient.TestConnection(ctx); err != nil {
		fmt.Printf("FAILED: %v\n", err)
		hasError = true
	} else {
		fmt.Println("OK")
	}

	if hasError {
		return fmt.Errorf("validation failed")
	}

	fmt.Println("\nAll validations passed.")
	return nil
}
