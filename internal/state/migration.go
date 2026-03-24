package state

import "time"

// MigrationStatus represents the current state of a migration.
type MigrationStatus string

const (
	StatusCreated      MigrationStatus = "CREATED"
	StatusFullSync     MigrationStatus = "FULL_SYNC"
	StatusSyncing      MigrationStatus = "SYNCING"
	StatusCutoverReady MigrationStatus = "CUTOVER_READY"
	StatusCuttingOver  MigrationStatus = "CUTTING_OVER"
	StatusCompleted    MigrationStatus = "COMPLETED"
	StatusFailed       MigrationStatus = "FAILED"
)

// TransportMode controls how data is written to the target.
type TransportMode string

const (
	// TransportISCSI writes blocks directly to Nutanix Volume Group via iSCSI.
	// Most network-efficient: only changed blocks cross the wire.
	// Requires Linux with iscsiadm.
	TransportISCSI TransportMode = "iscsi"

	// TransportStream streams disk data directly to Nutanix image upload API
	// with gzip compression. No local disk needed. Works from any OS (Mac/Linux).
	// Good for remote migrations where iSCSI is not available.
	TransportStream TransportMode = "stream"

	// TransportImage stages to local qcow2 file, then uploads to Nutanix via API.
	// Requires local disk space equal to the VM disk size.
	TransportImage TransportMode = "image"

	// TransportRepository reads via NFC, stores locally as raw file, uploads as qcow2.
	// Correct for thin-provisioned VMDKs. Pure Go, no VDDK dependency.
	TransportRepository TransportMode = "repository"
)

// SnapshotRef tracks a VMware snapshot we created.
type SnapshotRef struct {
	Name  string `json:"name"`
	MoRef string `json:"moref"` // e.g. "snapshot-1234"
}

// MigrationState holds the overall migration state.
type MigrationState struct {
	PlanName      string          `json:"plan_name"`
	VMName        string          `json:"vm_name"`
	Status        MigrationStatus `json:"status"`
	Transport     TransportMode   `json:"transport"`
	Disks         []DiskState     `json:"disks"`
	SyncCount     int             `json:"sync_count"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	Error         string          `json:"error,omitempty"`
	VolumeGroupID string          `json:"volume_group_id,omitempty"`
	Snapshots     []SnapshotRef   `json:"snapshots,omitempty"`
}

// DiskState tracks the replication state of a single disk.
type DiskState struct {
	Key          int32     `json:"key"`
	FileName     string    `json:"file_name"`
	CapacityB    int64     `json:"capacity_bytes"`
	ChangeID     string    `json:"change_id"`
	LocalPath    string    `json:"local_path"`
	ImageUUID    string    `json:"image_uuid,omitempty"`
	VGDiskIndex  int       `json:"vg_disk_index"`
	BytesCopied  int64     `json:"bytes_copied"`
	LastSyncedAt time.Time `json:"last_synced_at"`
}
