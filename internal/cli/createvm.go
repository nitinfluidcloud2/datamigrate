package cli

import (
	"context"
	"fmt"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/nitinmore/datamigrate/internal/config"
	"github.com/nitinmore/datamigrate/internal/nutanix"
)

var createVMCmd = &cobra.Command{
	Use:   "createvm",
	Short: "Create a VM on AHV from a Volume Group",
	Long:  "Creates a VM on Nutanix AHV using the plan's VM spec (CPU, RAM, NIC) and attaches the specified Volume Group as its disk.",
	RunE:  runCreateVM,
}

var (
	createVMPlanPath    string
	createVMVGName      string
	createVMName        string
	createVMPowerOn     bool
	createVMBootType    string
)

func init() {
	createVMCmd.Flags().StringVar(&createVMPlanPath, "plan", "", "Migration plan file")
	createVMCmd.Flags().StringVar(&createVMVGName, "volume-group", "", "Volume Group name to attach")
	createVMCmd.Flags().StringVar(&createVMName, "vm-name", "", "VM name (defaults to plan's vm_name + '-ahv')")
	createVMCmd.Flags().BoolVar(&createVMPowerOn, "power-on", false, "Power on the VM after creation")
	createVMCmd.Flags().StringVar(&createVMBootType, "boot-type", "UEFI", "Boot type: UEFI or LEGACY")

	createVMCmd.MarkFlagRequired("plan")
	createVMCmd.MarkFlagRequired("volume-group")

	rootCmd.AddCommand(createVMCmd)
}

func runCreateVM(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	plan, err := config.LoadPlan(createVMPlanPath)
	if err != nil {
		return fmt.Errorf("loading plan: %w", err)
	}

	nxClient := nutanix.NewClient(nutanix.ClientConfig{
		PrismCentral: plan.Target.PrismCentral,
		Username:     plan.Target.Username,
		Password:     plan.Target.Password,
		Insecure:     plan.Target.Insecure,
	})

	// Step 1: Find the Volume Group by name
	fmt.Printf("Looking up Volume Group %q...\n", createVMVGName)
	vg, err := nxClient.FindVolumeGroupByName(ctx, createVMVGName)
	if err != nil {
		return fmt.Errorf("finding volume group: %w", err)
	}
	fmt.Printf("Found Volume Group: %s (UUID: %s)\n", vg.Name, vg.UUID)

	// Step 2: Detach any iSCSI clients from the VG
	fmt.Println("Detaching iSCSI clients from Volume Group...")
	if err := nxClient.DetachVGFromExternal(ctx, vg.UUID); err != nil {
		log.Warn().Err(err).Msg("detach iSCSI client failed (may not have any attached)")
	}

	// Step 3: Determine VM name
	vmName := createVMName
	if vmName == "" {
		vmName = plan.VMName + "-ahv"
	}

	// Step 4: Build VM spec from plan
	numCPUs := plan.TargetVMSpec.NumCPUs
	if numCPUs == 0 {
		numCPUs = 2
	}
	memoryMB := plan.TargetVMSpec.MemoryMB
	if memoryMB == 0 {
		memoryMB = 4096
	}

	clusterUUID := plan.TargetVMSpec.ClusterUUID
	if clusterUUID == "" {
		clusterUUID = plan.Target.ClusterUUID
	}

	// Build NIC list from network mappings
	var nics []nutanix.VMNIC
	for _, nm := range plan.NetworkMap {
		if nm.Target != "" {
			nics = append(nics, nutanix.VMNIC{
				SubnetRef: &nutanix.ResourceRef{
					Kind: "subnet",
					UUID: nm.Target,
				},
			})
		}
	}

	spec := nutanix.VMCreateSpec{
		Spec: nutanix.VMSpec{
			Name:        vmName,
			Description: fmt.Sprintf("Migrated from VMware via datamigrate (VG: %s)", createVMVGName),
			Resources: nutanix.VMResources{
				NumSockets:      numCPUs,
				NumVCPUsPerSock: 1,
				MemoryMB:        memoryMB,
				PowerState:      "OFF",
				MachineType:     "PC",
				BootConfig:      buildBootConfig(createVMBootType),
				NICList:         nics,
			},
		},
		Metadata: nutanix.Metadata{Kind: "vm"},
	}

	if clusterUUID != "" {
		spec.Spec.ClusterRef = &nutanix.ResourceRef{
			Kind: "cluster",
			UUID: clusterUUID,
		}
	}

	fmt.Printf("Creating VM %q (%d vCPU, %d MB RAM)...\n", vmName, numCPUs, memoryMB)
	vmUUID, err := nxClient.CreateVM(ctx, spec)
	if err != nil {
		return fmt.Errorf("creating VM: %w", err)
	}
	fmt.Printf("VM created: %s (UUID: %s)\n", vmName, vmUUID)

	// Step 5: Attach Volume Group to the VM
	fmt.Printf("Attaching Volume Group %q to VM...\n", createVMVGName)
	if err := nxClient.AttachVGToVM(ctx, vg.UUID, vmUUID); err != nil {
		return fmt.Errorf("attaching volume group to VM: %w", err)
	}
	fmt.Println("Volume Group attached successfully.")

	// Step 6: Power on if requested
	if createVMPowerOn {
		fmt.Println("Powering on VM...")
		if err := nxClient.PowerOnVM(ctx, vmUUID); err != nil {
			return fmt.Errorf("powering on VM: %w", err)
		}
		fmt.Println("VM is now powered on.")
	}

	fmt.Printf("\nDone! VM %q is ready.\n", vmName)
	fmt.Printf("  UUID: %s\n", vmUUID)
	fmt.Printf("  Volume Group: %s\n", createVMVGName)
	if !createVMPowerOn {
		fmt.Println("  Use --power-on flag or power on from Prism Central.")
	}
	return nil
}

func buildBootConfig(bootType string) *nutanix.BootConfig {
	bc := &nutanix.BootConfig{BootType: bootType}
	if bootType == "LEGACY" {
		// Legacy BIOS requires boot_device_order_list
		bc.BootDeviceOrderList = []string{"CDROM", "DISK", "NETWORK"}
	}
	return bc
}
