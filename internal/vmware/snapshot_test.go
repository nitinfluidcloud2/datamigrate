package vmware

import (
	"context"
	"testing"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
)

func TestCreateAndRemoveSnapshot(t *testing.T) {
	simulator.Test(func(ctx context.Context, c *vim25.Client) {
		client := &Client{
			vimClient: c,
		}

		vms, err := client.DiscoverVMs(ctx, "")
		if err != nil || len(vms) == 0 {
			t.Skip("no VMs in simulator")
		}

		vm, _, err := client.FindVM(ctx, "", vms[0].Name)
		if err != nil {
			t.Fatalf("FindVM: %v", err)
		}

		snapRef, err := client.CreateSnapshot(ctx, vm, "test-snap", "test snapshot")
		if err != nil {
			t.Fatalf("CreateSnapshot: %v", err)
		}
		if snapRef == nil {
			t.Fatal("snapRef is nil")
		}
		t.Logf("Created snapshot: %s", snapRef.Value)

		err = client.RemoveAllSnapshots(ctx, vm)
		if err != nil {
			t.Fatalf("RemoveAllSnapshots: %v", err)
		}
	})
}
