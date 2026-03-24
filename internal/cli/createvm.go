package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/nitinmore/datamigrate/internal/config"
	"github.com/nitinmore/datamigrate/internal/nutanix"
	"github.com/nitinmore/datamigrate/internal/state"
)

var createVMCmd = &cobra.Command{
	Use:   "createvm",
	Short: "Create a VM on AHV from a migrated disk",
	Long:  "Creates a VM on Nutanix AHV using the plan's VM spec (CPU, RAM, NIC). For repository/stream transport, uses the uploaded image. For iSCSI transport, attaches the Volume Group.",
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
	createVMCmd.Flags().StringVar(&createVMVGName, "volume-group", "", "Volume Group name to attach (iSCSI transport)")
	createVMCmd.Flags().StringVar(&createVMName, "vm-name", "", "VM name (defaults to plan's vm_name + '-ahv')")
	createVMCmd.Flags().BoolVar(&createVMPowerOn, "power-on", false, "Power on the VM after creation")
	createVMCmd.Flags().StringVar(&createVMBootType, "boot-type", "UEFI", "Boot type: UEFI or LEGACY")

	createVMCmd.MarkFlagRequired("plan")
	// volume-group is no longer required — auto-detected from transport

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

	// Determine VM name
	vmName := createVMName
	if vmName == "" {
		vmName = plan.VMName + "-ahv"
	}

	// Build VM spec from plan
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

	// Determine disk attachment method based on transport
	var disks []nutanix.VMDisk
	description := "Migrated from VMware via datamigrate"

	if createVMVGName != "" {
		// iSCSI transport: attach Volume Group
		fmt.Printf("Looking up Volume Group %q...\n", createVMVGName)
		vg, err := nxClient.FindVolumeGroupByName(ctx, createVMVGName)
		if err != nil {
			return fmt.Errorf("finding volume group: %w", err)
		}
		fmt.Printf("Found Volume Group: %s (UUID: %s)\n", vg.Name, vg.UUID)

		fmt.Println("Detaching iSCSI clients from Volume Group...")
		if err := nxClient.DetachVGFromExternal(ctx, vg.UUID); err != nil {
			log.Warn().Err(err).Msg("detach iSCSI client failed (may not have any attached)")
		}
		description = fmt.Sprintf("Migrated from VMware via datamigrate (VG: %s)", createVMVGName)
		// VG attachment happens after VM creation (separate API call)
	} else {
		// Repository/stream transport: use image UUID from state
		stagingDir := plan.Staging.Directory
		if stagingDir == "" {
			stagingDir = "/tmp/datamigrate"
		}
		stateFile := filepath.Join(stagingDir, "state.db")
		store, err := state.NewStore(stateFile)
		if err != nil {
			return fmt.Errorf("opening state: %w", err)
		}
		defer store.Close()

		ms, err := store.GetMigration(plan.Name)
		if err != nil {
			return fmt.Errorf("getting migration state: %w", err)
		}

		for _, disk := range ms.Disks {
			if disk.ImageUUID == "" {
				return fmt.Errorf("disk %d has no image UUID — run T0 sync first", disk.Key)
			}
			fmt.Printf("Using image %s for disk-%d\n", disk.ImageUUID, disk.Key)
			disks = append(disks, nutanix.VMDisk{
				DataSourceRef: &nutanix.ResourceRef{
					Kind: "image",
					UUID: disk.ImageUUID,
				},
				DeviceProps: &nutanix.DeviceProps{
					DeviceType: "DISK",
					DiskAddr: &nutanix.DiskAddr{
						AdapterType: "SCSI",
						DeviceIndex: disk.VGDiskIndex,
					},
				},
			})
		}
		description = "Migrated from VMware via datamigrate (repository)"
	}

	spec := nutanix.VMCreateSpec{
		Spec: nutanix.VMSpec{
			Name:        vmName,
			Description: description,
			Resources: nutanix.VMResources{
				NumSockets:      numCPUs,
				NumVCPUsPerSock: 1,
				MemoryMB:        memoryMB,
				PowerState:      "OFF",
				MachineType:     "PC",
				BootConfig:      buildBootConfig(createVMBootType),
				DiskList:        disks,
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

	fmt.Printf("Creating VM %q (%d vCPU, %d MB RAM, boot: %s)...\n", vmName, numCPUs, memoryMB, createVMBootType)
	vmUUID, err := nxClient.CreateVM(ctx, spec)
	if err != nil {
		return fmt.Errorf("creating VM: %w", err)
	}
	fmt.Printf("VM created: %s (UUID: %s)\n", vmName, vmUUID)

	// For VG transport: attach Volume Group after VM creation
	if createVMVGName != "" {
		vg, _ := nxClient.FindVolumeGroupByName(ctx, createVMVGName)
		fmt.Printf("Attaching Volume Group %q to VM...\n", createVMVGName)
		if err := nxClient.AttachVGToVM(ctx, vg.UUID, vmUUID); err != nil {
			return fmt.Errorf("attaching volume group to VM: %w", err)
		}
		fmt.Println("Volume Group attached successfully.")
	}

	// Power on if requested
	if createVMPowerOn {
		fmt.Println("Powering on VM...")
		if err := nxClient.PowerOnVM(ctx, vmUUID); err != nil {
			return fmt.Errorf("powering on VM: %w", err)
		}
		fmt.Println("VM is now powered on.")
	}

	fmt.Printf("\nDone! VM %q is ready.\n", vmName)
	fmt.Printf("  UUID: %s\n", vmUUID)
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
