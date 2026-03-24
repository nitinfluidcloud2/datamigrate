package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/nitinmore/datamigrate/internal/config"
	"github.com/nitinmore/datamigrate/internal/vmware"
)

var planCmd = &cobra.Command{
	Use:   "plan",
	Short: "Manage migration plans",
}

var planCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a migration plan",
	RunE:  runPlanCreate,
}

var planShowCmd = &cobra.Command{
	Use:   "show [plan-file]",
	Short: "Show a migration plan",
	Args:  cobra.ExactArgs(1),
	RunE:  runPlanShow,
}

var (
	planCredsDir    string
	planVMName      string
	planNetworkMaps []string
	planStorageMaps []string
	planOutputFile  string
	planTransport   string
)

func init() {
	planCreateCmd.Flags().StringVar(&planCredsDir, "creds-dir", "configs", "Directory containing vmware.creds and nutanix.creds")
	planCreateCmd.Flags().StringVar(&planVMName, "vm", "", "VM name to migrate")
	planCreateCmd.Flags().StringSliceVar(&planNetworkMaps, "network-map", nil, "Network mappings: subnet-uuid or source-network:subnet-uuid")
	planCreateCmd.Flags().StringSliceVar(&planStorageMaps, "storage-map", nil, "Storage mappings (source-datastore:target-container-uuid) — optional, for future use")
	planCreateCmd.Flags().StringVar(&planOutputFile, "output", "", "Output plan file (default: <vm-name>-plan.yaml)")
	planCreateCmd.Flags().StringVar(&planTransport, "transport", "stream", "Transport mode: 'iscsi' (direct VG writes, Linux only), 'stream' (gzip API upload, any OS), 'image' (local qcow2 staging), 'repository' (NFC + local storage + qcow2 upload)")

	planCreateCmd.MarkFlagRequired("vm")

	planCmd.AddCommand(planCreateCmd)
	planCmd.AddCommand(planShowCmd)
	rootCmd.AddCommand(planCmd)
}

func runPlanCreate(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(planCredsDir)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Parse network mappings
	// Supports two formats:
	//   --network-map "subnet-uuid"                  (single NIC, just the target subnet)
	//   --network-map "VM Network:subnet-uuid"       (multi-NIC, source:target for each NIC)
	var networkMaps []config.NetworkMapping
	for _, nm := range planNetworkMaps {
		if strings.Contains(nm, ":") {
			parts := strings.SplitN(nm, ":", 2)
			networkMaps = append(networkMaps, config.NetworkMapping{
				Source: parts[0],
				Target: parts[1],
			})
		} else {
			// Just a subnet UUID — no source network name needed
			networkMaps = append(networkMaps, config.NetworkMapping{
				Target: nm,
			})
		}
	}

	// Parse storage mappings
	var storageMaps []config.StorageMapping
	for _, sm := range planStorageMaps {
		parts := strings.SplitN(sm, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid storage mapping %q (expected source:target)", sm)
		}
		storageMaps = append(storageMaps, config.StorageMapping{
			Source: parts[0],
			Target: parts[1],
		})
	}

	// Connect to vCenter to discover VM details
	ctx := context.Background()
	client, err := vmware.NewClient(ctx, vmware.ClientConfig{
		VCenter:  cfg.Source.VCenter,
		Username: cfg.Source.Username,
		Password: cfg.Source.Password,
		Insecure: cfg.Source.Insecure,
	})
	if err != nil {
		return fmt.Errorf("connecting to vCenter: %w", err)
	}
	defer client.Logout(ctx)

	_, vmInfo, err := client.FindVM(ctx, "", planVMName)
	if err != nil {
		return fmt.Errorf("finding VM: %w", err)
	}

	// Set per-VM staging directory for parallel execution isolation
	staging := cfg.Staging
	if staging.Directory == "" {
		staging.Directory = "/tmp/datamigrate"
	}
	staging.Directory = fmt.Sprintf("%s/%s", staging.Directory, planVMName)

	plan := &config.MigrationPlan{
		Name:      fmt.Sprintf("%s-migration", planVMName),
		VMName:    planVMName,
		VMMoref:   vmInfo.Moref,
		Transport: planTransport,
		Staging:   staging,
		NetworkMap: networkMaps,
		StorageMap: storageMaps,
		TargetVMSpec: config.TargetVMSpec{
			NumCPUs:     vmInfo.NumCPUs,
			MemoryMB:    int64(vmInfo.MemoryMB),
			ClusterUUID: cfg.Target.ClusterUUID,
		},
	}

	outputFile := planOutputFile
	if outputFile == "" {
		outputFile = fmt.Sprintf("%s/%s-plan.yaml", planCredsDir, planVMName)
	}

	if err := config.SavePlan(plan, outputFile); err != nil {
		return fmt.Errorf("saving plan: %w", err)
	}

	fmt.Printf("Migration plan created: %s\n", outputFile)
	fmt.Printf("VM: %s (moref: %s)\n", vmInfo.Name, vmInfo.Moref)
	fmt.Printf("CPUs: %d, Memory: %d MB\n", vmInfo.NumCPUs, vmInfo.MemoryMB)
	fmt.Printf("Disks: %d, NICs: %d\n", len(vmInfo.Disks), len(vmInfo.NICs))
	for _, d := range vmInfo.Disks {
		sizeGB := float64(d.CapacityKB) / (1024 * 1024)
		fmt.Printf("  disk-%d: %.1f GB (%s) [%s]\n", d.Key, sizeGB, d.FileName, d.Datastore)
	}

	return nil
}

func runPlanShow(cmd *cobra.Command, args []string) error {
	plan, err := config.LoadPlan(args[0])
	if err != nil {
		return fmt.Errorf("loading plan: %w", err)
	}

	// Redact passwords
	plan.Source.Password = "***"
	plan.Target.Password = "***"

	data, err := yaml.Marshal(plan)
	if err != nil {
		return fmt.Errorf("marshaling plan: %w", err)
	}

	fmt.Println(string(data))
	return nil
}
