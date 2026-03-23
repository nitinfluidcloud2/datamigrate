package nutanix

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/rs/zerolog/log"
)

// VMCreateSpec is the request body for creating a VM on AHV.
type VMCreateSpec struct {
	Spec     VMSpec   `json:"spec"`
	Metadata Metadata `json:"metadata"`
}

// Metadata holds Nutanix resource metadata.
type Metadata struct {
	Kind string `json:"kind"`
}

// VMSpec holds the VM specification.
type VMSpec struct {
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	Resources   VMResources  `json:"resources"`
	ClusterRef  *ResourceRef `json:"cluster_reference,omitempty"`
}

// VMResources holds VM resource configuration.
type VMResources struct {
	NumSockets      int32          `json:"num_sockets"`
	NumVCPUsPerSock int32          `json:"num_vcpus_per_socket"`
	MemoryMB        int64          `json:"memory_size_mib"`
	PowerState      string         `json:"power_state"`
	MachineType     string         `json:"machine_type,omitempty"`
	BootConfig      *BootConfig    `json:"boot_config,omitempty"`
	DiskList        []VMDisk       `json:"disk_list,omitempty"`
	NICList         []VMNIC        `json:"nic_list,omitempty"`
}

// BootConfig holds VM boot configuration.
type BootConfig struct {
	BootType           string   `json:"boot_type,omitempty"`
	BootDeviceOrderList []string `json:"boot_device_order_list,omitempty"`
}

// VMDisk represents a disk attachment.
type VMDisk struct {
	DataSourceRef *ResourceRef `json:"data_source_reference,omitempty"`
	DeviceProps   *DeviceProps `json:"device_properties,omitempty"`
}

// DeviceProps holds disk device properties.
type DeviceProps struct {
	DeviceType string    `json:"device_type"`
	DiskAddr   *DiskAddr `json:"disk_address,omitempty"`
}

// DiskAddr holds disk address info.
type DiskAddr struct {
	AdapterType string `json:"adapter_type"`
	DeviceIndex int    `json:"device_index"`
}

// VMNIC represents a NIC attachment.
type VMNIC struct {
	SubnetRef *ResourceRef `json:"subnet_reference,omitempty"`
}

// ResourceRef is a reference to a Nutanix resource.
type ResourceRef struct {
	Kind string `json:"kind"`
	UUID string `json:"uuid"`
}

// VMResponse is the response from VM creation.
type VMResponse struct {
	Metadata struct {
		UUID string `json:"uuid"`
	} `json:"metadata"`
	Status struct {
		ExecutionContext struct {
			TaskUUID string `json:"task_uuid"`
		} `json:"execution_context"`
	} `json:"status"`
}

// CreateVM creates a VM on Nutanix AHV.
func (c *Client) CreateVM(ctx context.Context, spec VMCreateSpec) (string, error) {
	log.Info().Str("name", spec.Spec.Name).Msg("creating VM on Nutanix AHV")

	body, status, err := c.doRequest(ctx, http.MethodPost, "/vms", spec)
	if err != nil {
		return "", fmt.Errorf("creating VM: %w", err)
	}
	if status >= 300 {
		return "", fmt.Errorf("create VM failed with status %d: %s", status, string(body))
	}

	var resp VMResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parsing VM response: %w", err)
	}

	uuid := resp.Metadata.UUID
	log.Info().Str("uuid", uuid).Msg("VM created")

	if taskUUID := resp.Status.ExecutionContext.TaskUUID; taskUUID != "" {
		if err := c.WaitForTask(ctx, taskUUID); err != nil {
			return "", fmt.Errorf("waiting for VM creation: %w", err)
		}
	}

	return uuid, nil
}

// PowerOnVM powers on a VM.
func (c *Client) PowerOnVM(ctx context.Context, vmUUID string) error {
	log.Info().Str("uuid", vmUUID).Msg("powering on VM")

	// Get current VM spec
	body, status, err := c.doRequest(ctx, http.MethodGet, "/vms/"+vmUUID, nil)
	if err != nil {
		return fmt.Errorf("getting VM: %w", err)
	}
	if status >= 300 {
		return fmt.Errorf("get VM failed with status %d", status)
	}

	var vmData map[string]interface{}
	if err := json.Unmarshal(body, &vmData); err != nil {
		return fmt.Errorf("parsing VM data: %w", err)
	}

	// Update power state
	if spec, ok := vmData["spec"].(map[string]interface{}); ok {
		if resources, ok := spec["resources"].(map[string]interface{}); ok {
			resources["power_state"] = "ON"
		}
	}
	// Remove status from update
	delete(vmData, "status")

	body, status, err = c.doRequest(ctx, http.MethodPut, "/vms/"+vmUUID, vmData)
	if err != nil {
		return fmt.Errorf("updating VM power state: %w", err)
	}
	if status >= 300 {
		return fmt.Errorf("power on failed with status %d: %s", status, string(body))
	}

	var resp VMResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parsing power on response: %w", err)
	}

	if taskUUID := resp.Status.ExecutionContext.TaskUUID; taskUUID != "" {
		if err := c.WaitForTask(ctx, taskUUID); err != nil {
			return fmt.Errorf("waiting for power on: %w", err)
		}
	}

	return nil
}
