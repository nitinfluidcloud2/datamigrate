package vmware

import (
	"context"
	"fmt"

	"github.com/rs/zerolog/log"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// VMInfo holds discovered VM metadata.
type VMInfo struct {
	Name            string
	Moref           string
	PowerState      string
	NumCPUs         int32
	MemoryMB        int32
	GuestID         string
	HardwareVersion string
	Disks           []DiskInfo
	NICs            []NICInfo
}

// DiskInfo holds disk metadata.
type DiskInfo struct {
	Key        int32
	FileName   string
	CapacityKB int64
	Datastore  string
}

// NICInfo holds NIC metadata.
type NICInfo struct {
	Name       string
	Network    string
	MacAddress string
}

// DiscoverVMs lists all VMs in the inventory, optionally filtered by datacenter.
func (c *Client) DiscoverVMs(ctx context.Context, datacenter string) ([]VMInfo, error) {
	finder := find.NewFinder(c.vimClient, true)

	// Set datacenter
	dc, err := finder.DatacenterOrDefault(ctx, datacenter)
	if err != nil {
		return nil, fmt.Errorf("finding datacenter: %w", err)
	}
	finder.SetDatacenter(dc)

	// Find all VMs
	vms, err := finder.VirtualMachineList(ctx, "*")
	if err != nil {
		return nil, fmt.Errorf("listing VMs: %w", err)
	}

	log.Info().Int("count", len(vms)).Msg("discovered VMs")

	var result []VMInfo
	for _, vm := range vms {
		info, err := c.getVMInfo(ctx, vm)
		if err != nil {
			log.Warn().Err(err).Str("vm", vm.Name()).Msg("failed to get VM info, skipping")
			continue
		}
		result = append(result, *info)
	}

	return result, nil
}

// FindVM finds a specific VM by name.
func (c *Client) FindVM(ctx context.Context, datacenter, name string) (*object.VirtualMachine, *VMInfo, error) {
	finder := find.NewFinder(c.vimClient, true)

	dc, err := finder.DatacenterOrDefault(ctx, datacenter)
	if err != nil {
		return nil, nil, fmt.Errorf("finding datacenter: %w", err)
	}
	finder.SetDatacenter(dc)

	vm, err := finder.VirtualMachine(ctx, name)
	if err != nil {
		return nil, nil, fmt.Errorf("finding VM %q: %w", name, err)
	}

	info, err := c.getVMInfo(ctx, vm)
	if err != nil {
		return nil, nil, err
	}

	return vm, info, nil
}

func (c *Client) getVMInfo(ctx context.Context, vm *object.VirtualMachine) (*VMInfo, error) {
	var mvm mo.VirtualMachine
	pc := property.DefaultCollector(c.vimClient)
	err := pc.RetrieveOne(ctx, vm.Reference(), []string{
		"config",
		"runtime.powerState",
		"guest.guestId",
	}, &mvm)
	if err != nil {
		return nil, fmt.Errorf("retrieving VM properties: %w", err)
	}

	info := &VMInfo{
		Name:            mvm.Config.Name,
		Moref:           vm.Reference().Value,
		PowerState:      string(mvm.Runtime.PowerState),
		NumCPUs:         mvm.Config.Hardware.NumCPU,
		MemoryMB:        mvm.Config.Hardware.MemoryMB,
		HardwareVersion: mvm.Config.Version,
	}
	if mvm.Guest != nil {
		info.GuestID = mvm.Guest.GuestId
	}

	// Extract disks and NICs from devices
	for _, dev := range mvm.Config.Hardware.Device {
		if disk, ok := dev.(*types.VirtualDisk); ok {
			var fileName, datastore string
			if backing, ok := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo); ok {
				fileName = backing.FileName
				if backing.Datastore != nil {
					datastore = backing.Datastore.Value
				}
			}
			info.Disks = append(info.Disks, DiskInfo{
				Key:        disk.Key,
				FileName:   fileName,
				CapacityKB: disk.CapacityInKB,
				Datastore:  datastore,
			})
		}
		if nic, ok := dev.(types.BaseVirtualEthernetCard); ok {
			card := nic.GetVirtualEthernetCard()
			var network string
			if card.Backing != nil {
				if nb, ok := card.Backing.(*types.VirtualEthernetCardNetworkBackingInfo); ok {
					network = nb.DeviceName
				}
			}
			info.NICs = append(info.NICs, NICInfo{
				Name:       card.DeviceInfo.GetDescription().Label,
				Network:    network,
				MacAddress: card.MacAddress,
			})
		}
	}

	return info, nil
}
