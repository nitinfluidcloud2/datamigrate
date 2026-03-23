package cli

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/nitinmore/datamigrate/internal/config"
	"github.com/nitinmore/datamigrate/internal/util"
	"github.com/nitinmore/datamigrate/internal/vmware"
)

var discoverCmd = &cobra.Command{
	Use:   "discover",
	Short: "List VMs on vCenter",
	Long:  "Discovers and lists all virtual machines available on the specified vCenter server.",
	RunE:  runDiscover,
}

var (
	discoverCredsDir   string
	discoverDatacenter string
	discoverVM         string
)

func init() {
	discoverCmd.Flags().StringVar(&discoverCredsDir, "creds-dir", "configs", "Directory containing vmware.creds and nutanix.creds")
	discoverCmd.Flags().StringVar(&discoverDatacenter, "datacenter", "", "Datacenter name (optional)")
	discoverCmd.Flags().StringVar(&discoverVM, "vm", "", "Show detailed info for a specific VM")

	rootCmd.AddCommand(discoverCmd)
}

func runDiscover(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(discoverCredsDir)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

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

	vms, err := client.DiscoverVMs(ctx, discoverDatacenter)
	if err != nil {
		return fmt.Errorf("discovering VMs: %w", err)
	}

	if len(vms) == 0 {
		fmt.Println("No VMs found.")
		return nil
	}

	// If --vm is specified, show detailed info for that VM
	if discoverVM != "" {
		for _, vm := range vms {
			if vm.Name == discoverVM {
				fmt.Printf("VM: %s\n", vm.Name)
				fmt.Printf("  MOREF:       %s\n", vm.Moref)
				fmt.Printf("  Power:       %s\n", vm.PowerState)
				fmt.Printf("  Guest OS:    %s\n", vm.GuestID)
				fmt.Printf("  CPUs:        %d\n", vm.NumCPUs)
				fmt.Printf("  Memory:      %s\n", util.HumanSize(int64(vm.MemoryMB)*1024*1024))
				fmt.Printf("  Disks:\n")
				for i, d := range vm.Disks {
					fmt.Printf("    [%d] %s  (%s, datastore: %s)\n", i, d.FileName, util.HumanSize(d.CapacityKB*1024), d.Datastore)
				}
				fmt.Printf("  NICs:\n")
				for i, n := range vm.NICs {
					fmt.Printf("    [%d] %s  (network: %s, mac: %s)\n", i, n.Name, n.Network, n.MacAddress)
				}
				return nil
			}
		}
		return fmt.Errorf("VM %q not found", discoverVM)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tMOREF\tPOWER\tGUEST OS\tCPUs\tMEMORY\tDISKS\tNICs\tNETWORKS")
	fmt.Fprintln(w, "----\t-----\t-----\t--------\t----\t------\t-----\t----\t--------")

	for _, vm := range vms {
		totalDiskSize := int64(0)
		for _, d := range vm.Disks {
			totalDiskSize += d.CapacityKB * 1024
		}
		networks := ""
		for i, n := range vm.NICs {
			if i > 0 {
				networks += ", "
			}
			networks += n.Network
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%d (%s)\t%d\t%s\n",
			vm.Name,
			vm.Moref,
			vm.PowerState,
			vm.GuestID,
			vm.NumCPUs,
			util.HumanSize(int64(vm.MemoryMB)*1024*1024),
			len(vm.Disks),
			util.HumanSize(totalDiskSize),
			len(vm.NICs),
			networks,
		)
	}
	w.Flush()

	return nil
}
