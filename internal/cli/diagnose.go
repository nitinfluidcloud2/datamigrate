package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nitinmore/datamigrate/internal/config"
	"github.com/nitinmore/datamigrate/internal/vmware"
)

var diagnoseCmd = &cobra.Command{
	Use:   "diagnose",
	Short: "Diagnose CBT and connectivity issues",
}

var diagnoseCBTCmd = &cobra.Command{
	Use:   "cbt",
	Short: "Functional test: is CBT actually working on this VM?",
	Long: `Performs an end-to-end CBT functional test:
  1. Reset CBT (disable → stun/unstun → enable → stun/unstun)
  2. Create snapshot S1, capture changeId1
  3. Create snapshot S2, capture changeId2
  4. QueryChangedDiskAreas with changeId="*" on S1
  5. QueryChangedDiskAreas with changeId1 on S2
  6. Report verdict`,
	RunE: runDiagnoseCBT,
}

var (
	diagnoseCredsDir string
	diagnoseVMName   string
)

func init() {
	diagnoseCBTCmd.Flags().StringVar(&diagnoseCredsDir, "creds-dir", "configs", "Directory containing vmware.creds")
	diagnoseCBTCmd.Flags().StringVar(&diagnoseVMName, "vm", "", "VM name to diagnose")
	diagnoseCBTCmd.MarkFlagRequired("vm")

	diagnoseCmd.AddCommand(diagnoseCBTCmd)
	rootCmd.AddCommand(diagnoseCmd)
}

func runDiagnoseCBT(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(diagnoseCredsDir)
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

	vm, vmInfo, err := client.FindVM(ctx, "", diagnoseVMName)
	if err != nil {
		return fmt.Errorf("finding VM: %w", err)
	}

	fmt.Printf("VM: %s (moref: %s, hw: %s)\n", vmInfo.Name, vmInfo.Moref, vmInfo.HardwareVersion)
	fmt.Printf("Disks: %d\n", len(vmInfo.Disks))
	for _, d := range vmInfo.Disks {
		fmt.Printf("  disk %d: %s\n", d.Key, d.FileName)
	}

	hasSnapshotChangeId := false
	incrementalWorks := false

	// Step 1: Check initial CBT status
	fmt.Println("\n=== Step 1: Initial CBT Status ===")
	cbtStatus, err := client.GetCBTStatus(ctx, vm)
	if err != nil {
		fmt.Printf("  ERROR: %v\n", err)
	} else {
		fmt.Printf("  changeTrackingEnabled: %v\n", cbtStatus.Enabled)
		for _, d := range cbtStatus.Disks {
			fmt.Printf("  disk %d: changeId=%q controller=%s\n", d.Key, d.ChangeID, d.Controller)
		}
	}

	// Step 2: Full CBT Reset
	fmt.Println("\n=== Step 2: Full CBT Reset ===")
	if err := client.ResetCBT(ctx, vm); err != nil {
		fmt.Printf("  ERROR: %v\n", err)
		fmt.Println("  Falling back to simple enable...")
		if err := client.EnableCBT(ctx, vm); err != nil {
			fmt.Printf("  Enable also failed: %v\n", err)
		}
	} else {
		fmt.Println("  CBT reset complete")
	}

	// Step 3: Create snapshot S1
	fmt.Println("\n=== Step 3: Create Snapshot S1 ===")
	s1Ref, err := client.CreateSnapshot(ctx, vm, "datamigrate-cbt-test-s1", "CBT test snapshot 1")
	if err != nil {
		fmt.Printf("  ERROR: %v\n", err)
		printVerdict(false, false)
		return nil
	}
	fmt.Printf("  S1 created: %s\n", s1Ref.Value)

	// Step 4: Capture changeId from S1
	fmt.Println("\n=== Step 4: Capture changeId from S1 ===")
	disk := vmInfo.Disks[0]
	diskInfo := vmware.DiskInfo{Key: disk.Key, FileName: disk.FileName, CapacityKB: disk.CapacityKB}

	changeId1VM, err := client.GetSnapshotChangeID(ctx, vm, s1Ref, disk.Key)
	if err != nil {
		fmt.Printf("  VM config:      ERROR: %v\n", err)
	} else {
		fmt.Printf("  VM config:      changeId=%q\n", changeId1VM)
	}

	changeId1Snap, err := client.GetSnapshotDiskChangeID(ctx, s1Ref, disk.Key)
	if err != nil {
		fmt.Printf("  Snapshot config: ERROR: %v\n", err)
	} else {
		fmt.Printf("  Snapshot config: changeId=%q\n", changeId1Snap)
	}

	changeId1 := changeId1Snap
	if changeId1 == "" {
		changeId1 = changeId1VM
	}
	if changeId1 != "" {
		hasSnapshotChangeId = true
	} else {
		fmt.Println("  ⚠ Both changeIds are empty — CBT is not generating tracking IDs")
	}

	// Step 5: Create snapshot S2
	fmt.Println("\n=== Step 6: Create Snapshot S2 ===")
	s2Ref, err := client.CreateSnapshot(ctx, vm, "datamigrate-cbt-test-s2", "CBT test snapshot 2")
	if err != nil {
		fmt.Printf("  ERROR: %v\n", err)
		client.RemoveSnapshotByMoRef(ctx, s1Ref.Value)
		printVerdict(hasSnapshotChangeId, incrementalWorks)
		return nil
	}
	fmt.Printf("  S2 created: %s\n", s2Ref.Value)

	// Step 6: QueryChangedDiskAreas with changeId1 on S2 (the real incremental test)
	fmt.Println("\n=== Step 6: QueryChangedDiskAreas (changeId1 → S2) ===")
	if changeId1 != "" {
		areasDelta, newChangeId, err := client.QueryChangedBlocks(ctx, vm, s2Ref, diskInfo, changeId1)
		if err != nil {
			fmt.Printf("  FAILED: %v\n", err)
		} else {
			var deltaBytes int64
			for _, a := range areasDelta {
				deltaBytes += a.Length
			}
			fmt.Printf("  %d extents, %d MB changed between S1→S2\n", len(areasDelta), deltaBytes/(1024*1024))
			fmt.Printf("  new changeId: %q\n", newChangeId)
			if newChangeId != "" {
				incrementalWorks = true
				fmt.Println("  ✅ Incremental query succeeded")
			}
		}
	} else {
		fmt.Println("  SKIPPED (no changeId1 to query with)")
	}

	// Step 7: Post-test CBT status
	fmt.Println("\n=== Step 7: Post-test CBT Status ===")
	cbtStatus, err = client.GetCBTStatus(ctx, vm)
	if err != nil {
		fmt.Printf("  ERROR: %v\n", err)
	} else {
		fmt.Printf("  changeTrackingEnabled: %v\n", cbtStatus.Enabled)
		for _, d := range cbtStatus.Disks {
			fmt.Printf("  disk %d: changeId=%q\n", d.Key, d.ChangeID)
		}
	}

	// Cleanup
	fmt.Println("\n=== Cleanup ===")
	if err := client.RemoveSnapshotByMoRef(ctx, s2Ref.Value); err != nil {
		fmt.Printf("  S2 removal: %v\n", err)
	} else {
		fmt.Println("  S2 removed")
	}
	if err := client.RemoveSnapshotByMoRef(ctx, s1Ref.Value); err != nil {
		fmt.Printf("  S1 removal: %v\n", err)
	} else {
		fmt.Println("  S1 removed")
	}

	printVerdict(hasSnapshotChangeId, incrementalWorks)
	return nil
}

func printVerdict(hasChangeId, incrementalWorks bool) {
	fmt.Printf("\n=============================\n")
	fmt.Printf("Results:\n")
	fmt.Printf("  Snapshot changeId:     %s\n", boolIcon(hasChangeId))
	fmt.Printf("  Incremental query:     %s\n", boolIcon(incrementalWorks))
	fmt.Println()

	if incrementalWorks {
		fmt.Println("Verdict: CBT WORKING ✅ — incremental sync will work")
	} else if hasChangeId {
		fmt.Println("Verdict: CBT PARTIAL ⚠️ — changeId exists but queries fail")
	} else {
		fmt.Println("Verdict: CBT BROKEN ❌ — no changeId, no incremental sync")
	}
	fmt.Printf("=============================\n")
}

func boolIcon(v bool) string {
	if v {
		return "✅"
	}
	return "❌"
}
