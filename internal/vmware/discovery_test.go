package vmware

import (
	"context"
	"testing"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
)

func TestDiscoverVMs(t *testing.T) {
	simulator.Test(func(ctx context.Context, c *vim25.Client) {
		client := &Client{
			vimClient:  c,
			vcenterURL: "https://simulator",
		}

		vms, err := client.DiscoverVMs(ctx, "")
		if err != nil {
			t.Fatalf("DiscoverVMs: %v", err)
		}

		if len(vms) == 0 {
			t.Fatal("expected at least one VM from simulator")
		}

		for _, vm := range vms {
			if vm.Name == "" {
				t.Error("VM name is empty")
			}
			if vm.Moref == "" {
				t.Error("VM moref is empty")
			}
			if vm.PowerState == "" {
				t.Error("VM power state is empty")
			}
			t.Logf("VM: %s (moref=%s, power=%s, cpus=%d, mem=%dMB, disks=%d, nics=%d)",
				vm.Name, vm.Moref, vm.PowerState, vm.NumCPUs, vm.MemoryMB,
				len(vm.Disks), len(vm.NICs))
		}
	})
}

func TestFindVM(t *testing.T) {
	simulator.Test(func(ctx context.Context, c *vim25.Client) {
		client := &Client{
			vimClient: c,
		}

		vms, err := client.DiscoverVMs(ctx, "")
		if err != nil {
			t.Fatalf("DiscoverVMs: %v", err)
		}
		if len(vms) == 0 {
			t.Skip("no VMs in simulator")
		}

		vmName := vms[0].Name
		vm, info, err := client.FindVM(ctx, "", vmName)
		if err != nil {
			t.Fatalf("FindVM(%q): %v", vmName, err)
		}
		if vm == nil {
			t.Fatal("vm object is nil")
		}
		if info.Name != vmName {
			t.Errorf("info.Name = %q, want %q", info.Name, vmName)
		}
	})
}
