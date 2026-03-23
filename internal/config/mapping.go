package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// MigrationPlan describes a VM migration plan.
type MigrationPlan struct {
	Name            string           `yaml:"name"`
	VMName          string           `yaml:"vm_name"`
	VMMoref         string           `yaml:"vm_moref,omitempty"`
	Transport       string           `yaml:"transport,omitempty"` // "iscsi" (default, efficient) or "image" (legacy, full re-upload)
	ISCSIChunkBytes int              `yaml:"iscsi_chunk_bytes,omitempty"` // iSCSI write chunk size in bytes (default: 1MB, max per SCSI command)
	Source          SourceConfig     `yaml:"source"`
	Target          TargetConfig     `yaml:"target"`
	Staging         StagingConfig    `yaml:"staging"`
	NetworkMap      []NetworkMapping `yaml:"network_map"`
	StorageMap      []StorageMapping `yaml:"storage_map"`
	TargetVMSpec    TargetVMSpec     `yaml:"target_vm_spec"`
}

// NetworkMapping maps a source network to a target subnet.
type NetworkMapping struct {
	Source string `yaml:"source"`
	Target string `yaml:"target"`
}

// StorageMapping maps a source datastore to a target container.
type StorageMapping struct {
	Source string `yaml:"source"`
	Target string `yaml:"target"`
}

// TargetVMSpec holds the desired VM configuration on the target.
type TargetVMSpec struct {
	NumCPUs   int32  `yaml:"num_cpus,omitempty"`
	MemoryMB  int64  `yaml:"memory_mb,omitempty"`
	ClusterUUID string `yaml:"cluster_uuid,omitempty"`
}

// SavePlan writes a migration plan to a YAML file.
func SavePlan(plan *MigrationPlan, path string) error {
	data, err := yaml.Marshal(plan)
	if err != nil {
		return fmt.Errorf("marshaling plan: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing plan file: %w", err)
	}
	return nil
}

// LoadPlan reads a migration plan from a YAML file.
// It also checks for vmware.creds and nutanix.creds files in the same directory
// as the plan file to override embedded credentials.
func LoadPlan(path string) (*MigrationPlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading plan file: %w", err)
	}
	var plan MigrationPlan
	if err := yaml.Unmarshal(data, &plan); err != nil {
		return nil, fmt.Errorf("unmarshaling plan: %w", err)
	}

	// Override credentials from .creds files in plan directory
	planDir := filepath.Dir(path)
	if vmCreds := loadCredsFile(planDir, "vmware.creds"); vmCreds != nil {
		if v := vmCreds["host"]; v != "" {
			plan.Source.VCenter = v
		}
		if v := vmCreds["username"]; v != "" {
			plan.Source.Username = v
		}
		if v := vmCreds["password"]; v != "" {
			plan.Source.Password = v
		}
		if v, ok := vmCreds["insecure"]; ok {
			plan.Source.Insecure = v == "true" || v == "yes" || v == "1"
		}
	}
	if nxCreds := loadCredsFile(planDir, "nutanix.creds"); nxCreds != nil {
		if v := nxCreds["host"]; v != "" {
			plan.Target.PrismCentral = v
		}
		if v := nxCreds["username"]; v != "" {
			plan.Target.Username = v
		}
		if v := nxCreds["password"]; v != "" {
			plan.Target.Password = v
		}
		if v := nxCreds["cluster_uuid"]; v != "" {
			plan.Target.ClusterUUID = v
		}
		if v := nxCreds["storage_container_uuid"]; v != "" {
			plan.Target.StorageContainerUUID = v
		}
		if v, ok := nxCreds["insecure"]; ok {
			plan.Target.Insecure = v == "true" || v == "yes" || v == "1"
		}
	}

	// Environment variables take highest priority
	if envPwd := os.Getenv("DATAMIGRATE_SOURCE_PASSWORD"); envPwd != "" {
		plan.Source.Password = envPwd
	}
	if envPwd := os.Getenv("DATAMIGRATE_TARGET_PASSWORD"); envPwd != "" {
		plan.Target.Password = envPwd
	}

	return &plan, nil
}
