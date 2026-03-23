# Datamigrate: Implementation Plan

## Approach: Veeam Model (Snapshot + CBT Block-Level Replication)

We use the **Veeam approach** — not Zerto — because:
- **Pure Go, no hypervisor agents** — no VRA/VIB installation on ESXi hosts
- **Self-contained CLI** — no infrastructure changes required
- **govmomi provides full CBT support** out of the box
- Simpler to implement; VDDK can be added later for SAN/HotAdd performance

### How It Works
```
CBT tells WHAT changed → ESXi provides the bytes → We replay blocks → One final qcow2 is written
```

---

## Project Structure

```
datamigrate/
├── cmd/datamigrate/main.go              # Entry point
├── internal/
│   ├── cli/                             # Cobra commands
│   │   ├── root.go                      # Root command, global flags
│   │   ├── discover.go                  # List VMs on vCenter
│   │   ├── plan.go                      # Create/show migration plan
│   │   ├── migrate.go                   # Start full sync, run incremental, show status
│   │   ├── cutover.go                   # Final sync + boot on AHV
│   │   ├── validate.go                  # Validate source + target connectivity
│   │   └── cleanup.go                   # Remove snapshots, temp artifacts
│   ├── config/
│   │   ├── config.go                    # Config structs (source/target), YAML + env vars
│   │   └── mapping.go                   # MigrationPlan, NetworkMapping, StorageMapping
│   ├── state/
│   │   ├── store.go                     # BoltDB state persistence
│   │   ├── migration.go                 # MigrationState, VMState, DiskState structs
│   │   └── journal.go                   # Block journal for resumability
│   ├── vmware/
│   │   ├── client.go                    # govmomi client wrapper (with relogin fix)
│   │   ├── discovery.go                 # VM/disk/NIC enumeration
│   │   ├── snapshot.go                  # Create/remove snapshots
│   │   ├── cbt.go                       # Enable CBT, QueryChangedDiskAreas
│   │   ├── reader.go                    # NFC-based disk reader (streamOptimized VMDK)
│   │   └── raw_reader.go               # Raw datastore reader (flat VMDK for iSCSI)
│   ├── iscsi/
│   │   └── initiator.go                 # Pure Go iSCSI initiator (no kernel modules)
│   ├── transport/
│   │   ├── transport.go                 # TransportMode enum + factory
│   │   ├── nbd.go                       # Pure Go NBD via govmomi NFC lease
│   │   └── vddk.go                      # CGo VDDK bindings (Phase 8)
│   ├── blockio/
│   │   ├── types.go                     # BlockExtent, BlockData
│   │   ├── reader.go                    # BlockReader interface
│   │   ├── writer.go                    # BlockWriter interface
│   │   ├── iscsi.go                     # ISCSIWriter using pure Go initiator
│   │   ├── stream_writer.go             # HTTP upload stream writer (buffered)
│   │   ├── qcow2.go                     # qcow2 disk writer (raw + convert)
│   │   └── pipeline.go                  # Concurrent read→compress→write pipeline
│   ├── nutanix/
│   │   ├── client.go                    # Prism Central HTTP client + task polling
│   │   ├── image.go                     # Image create + upload (qcow2)
│   │   ├── volume_group.go              # Volume Group CRUD + iSCSI portal + client attach
│   │   ├── vm.go                        # VM creation on AHV
│   │   └── network.go                   # List subnets/containers
│   ├── migration/
│   │   ├── orchestrator.go              # State machine coordinator
│   │   ├── full_sync.go                 # T0: full block copy
│   │   ├── incremental_sync.go          # T1..TN: CBT delta sync
│   │   ├── cutover.go                   # Final delta + VM creation + boot
│   │   └── progress.go                  # Progress tracking + ETA
│   └── util/
│       ├── retry.go                     # Exponential backoff retry
│       ├── logging.go                   # zerolog setup
│       └── size.go                      # Human-readable sizes
├── configs/example.yaml
├── Dockerfile                            # Multi-stage build (alpine + qemu-img)
├── go.mod
├── Makefile
└── plan.md                              # This file
```

---

## CLI Commands

```
datamigrate discover    --vcenter <url> --username <user>         # List VMs
datamigrate validate    --config <path>                           # Test connectivity
datamigrate plan create --vm <name> --transport iscsi   # Create plan (reads creds from configs/ dir)
datamigrate plan show   <plan-file>
datamigrate migrate start  --plan <plan.yaml>                    # Full sync (T0)
datamigrate migrate sync   --plan <plan.yaml>                    # Incremental (T1..TN)
datamigrate migrate status --plan <plan.yaml>                    # Show progress
datamigrate cutover        --plan <plan.yaml> --shutdown-source  # Final cutover
datamigrate createvm       --plan <plan.yaml> --volume-group <vg-name> [--vm-name <name>] [--boot-type UEFI|LEGACY] [--power-on]  # Create VM from VG
datamigrate cleanup        --plan <plan.yaml>                    # Remove snapshots/artifacts
```

**Typical workflow**: `discover → plan create → validate → migrate start → migrate sync (repeat) → cutover → cleanup`
**Manual workflow (iSCSI)**: `discover → plan create → validate → migrate start → migrate sync (repeat) → createvm → cleanup`

---

## Data Flow

### Transport Modes

The tool supports three transport modes, selectable via `--transport` flag:

#### iSCSI Transport (Default — Network Efficient)

Writes blocks **directly** to a Nutanix Volume Group over iSCSI. Only changed blocks cross the network. No local staging, no qcow2 conversion, no re-upload.

```
T0:  Snapshot → CBT("*") → ReadAllBlocks → iSCSI WriteAt(offset) to Nutanix VG → RemoveSnapshot
T1:  Snapshot → CBT(lastId) → ReadΔBlocks → iSCSI WriteAt(offset) to same VG → RemoveSnapshot
TN:  Same — only changed blocks transferred each time

Cutover: Final Δ → PowerOff source → Detach iSCSI → Attach VG to new VM → PowerOn
```

**Network transfer per sync**:
```
T0:  100 GB (all used blocks)
T1:    5 GB (daily delta)
T2:  500 MB (hourly delta)
TN:   50 MB (final delta)
────────────────────────────
Total: ~106 GB for 10 syncs
```

#### Stream Transport (Mac Testing — NFC + qemu-img)

Downloads streamOptimized VMDK via NFC lease, converts to qcow2 locally with `qemu-img`, then uploads to Nutanix Images. Good for development/testing on Mac where iSCSI is not available.

```
T0:  Snapshot → NFC Lease → Download VMDK → qemu-img convert → Upload qcow2 → RemoveSnapshot
T1:  Same — re-downloads full VMDK each time (no delta optimization yet)

Cutover: Upload final qcow2 → PowerOff source → CreateVM from image → PowerOn
```

**Note**: NFC gives streamOptimized VMDK format (sparse, compressed). qemu-img converts to qcow2 for Nutanix.

#### Image Transport (Legacy — Simpler but Wasteful)

Writes blocks to local raw file, converts to qcow2, uploads **entire file** each sync.

```
T0:  Snapshot → CBT("*") → ReadAll → local raw → qcow2 → Upload 30 GB → RemoveSnapshot
T1:  Snapshot → CBT(lastId) → ReadΔ → patch raw → qcow2 → Upload 30 GB → RemoveSnapshot
TN:  Same — full 30 GB upload every time regardless of delta size

Cutover: Final sync → Upload 30 GB → PowerOff source → CreateVM from image → PowerOn
```

**Network transfer per sync**:
```
T0:  30 GB (full qcow2)
T1:  30 GB (full qcow2 again)
T2:  30 GB (full qcow2 again)
TN:  30 GB (full qcow2 again)
────────────────────────────
Total: 300 GB for 10 syncs  ← 3x more than iSCSI
```

### Cutover
```
Final incremental → PowerOff source → CreateVM on AHV (mapped CPU/RAM/NICs/disks) → PowerOn target → Cleanup
```

---

## Key Dependencies

| Package | Purpose | Version |
|---|---|---|
| `github.com/spf13/cobra` | CLI framework | v1.10.2 |
| `github.com/spf13/viper` | Config (YAML + env vars) | v1.21.0 |
| `github.com/vmware/govmomi` | VMware vSphere API | v0.53.0 |
| `go.etcd.io/bbolt` | Embedded state DB | v1.4.3 |
| `github.com/rs/zerolog` | Structured logging | v1.34.0 |
| `github.com/schollz/progressbar/v3` | Progress bars | v3.19.0 |
| `gopkg.in/yaml.v3` | YAML marshal/unmarshal | v3.0.1 |

**Note**: govmomi v0.53.0 requires `go >= 1.24.13`.

---

## Key Interfaces

```go
type BlockReader interface {
    ReadBlocks(ctx context.Context, extents []BlockExtent) (<-chan BlockData, <-chan error)
    ReadExtent(ctx context.Context, extent BlockExtent) (BlockData, error)
    Close() error
}

type BlockWriter interface {
    WriteBlock(ctx context.Context, block BlockData) error
    Finalize() error
}
```

---

## Implementation Phases

### Phase 1: Project Skeleton + VMware Discovery ✅
- [x] Go module init, Cobra CLI scaffold, config loading
- [x] `internal/vmware/client.go` + `discovery.go` using govmomi
- [x] `datamigrate discover` command working
- [x] Tests with `govmomi/simulator`

```bash
# Discover VMs on vCenter
datamigrate discover --vcenter 10.0.0.1 --username admin@vsphere.local
```

### Phase 2: Snapshot + CBT ✅
- [x] `internal/vmware/snapshot.go` — create/remove snapshots
- [x] `internal/vmware/cbt.go` — enable CBT, query changed areas via `methods.QueryChangedDiskAreas`
- [x] `internal/state/store.go` — BoltDB persistence
- [x] Unit tests with govmomi simulator

### Phase 3: Block Reading via NBD ✅
- [x] `internal/blockio/types.go`, `reader.go` interfaces
- [x] `internal/transport/nbd.go` — NFC lease-based block reader (pure Go, no VDDK)
- [x] `internal/blockio/pipeline.go` — concurrent transfer pipeline (configurable concurrency)
- [x] Tests with mock reader/writer

### Phase 4: qcow2 Writer + Local Staging ✅
- [x] `internal/blockio/qcow2.go` — write blocks to raw sparse file → `qemu-img convert` to qcow2
- [x] Compressed qcow2 output (`-c` flag)

### Phase 5: Nutanix Client + Image Upload ✅
- [x] `internal/nutanix/client.go` — HTTP client, basic auth, task polling
- [x] `internal/nutanix/image.go` — create image + upload qcow2
- [x] `internal/nutanix/vm.go` — create VM with disk/NIC refs, power on/off
- [x] `internal/nutanix/network.go` — list subnets and containers
- [x] `datamigrate validate` command
- [x] Tests with httptest mock server

```bash
# Validate connectivity to both vCenter and Prism Central
datamigrate validate --config configs/
```

### Phase 6: Migration Orchestrator ✅
- [x] `internal/migration/orchestrator.go` — state machine (CREATED→FULL_SYNC→SYNCING→CUTOVER_READY→COMPLETED)
- [x] `internal/migration/full_sync.go` — T0 end-to-end
- [x] `internal/migration/progress.go` — tracking + ETA + rate
- [x] `internal/state/journal.go` — block journal for resumability
- [x] `datamigrate plan` and `datamigrate migrate start/status` commands

```bash
# Create migration plan
datamigrate plan create --config configs/ --vm ubuntu-vm --network-map "VM Network:subnet-uuid"

# Start full sync (T0)
datamigrate migrate start --plan configs/ubuntu-vm-plan.yaml

# Check progress
datamigrate migrate status --plan configs/ubuntu-vm-plan.yaml
```

### Phase 7: Incremental Sync + Cutover ✅
- [x] `internal/migration/incremental_sync.go` — T1..TN with CBT fallback to full sync
- [x] `internal/migration/cutover.go` — final sync + VM creation on AHV + power on
- [x] `datamigrate migrate sync`, `datamigrate cutover`, `datamigrate cleanup` commands

```bash
# Incremental sync (T1..TN) — only changed blocks
datamigrate migrate sync --plan configs/ubuntu-vm-plan.yaml

# Final cutover — delta sync + shutdown source + create VM + power on
datamigrate cutover --plan configs/ubuntu-vm-plan.yaml --shutdown-source

# Cleanup snapshots and temp artifacts
datamigrate cleanup --plan configs/ubuntu-vm-plan.yaml
```

### Phase 8: Real Disk Reading + Transport Modes ✅
- [x] `internal/vmware/reader.go` — Real NFC lease-based disk reader (ExportSnapshot → HTTP download)
- [x] `internal/vmware/raw_reader.go` — Raw datastore flat VMDK reader (for iSCSI transport)
- [x] `internal/iscsi/initiator.go` — Pure Go iSCSI initiator (no kernel modules, works on Mac/Linux)
- [x] `internal/blockio/iscsi.go` — ISCSIWriter using pure Go initiator
- [x] `internal/blockio/stream_writer.go` — Buffered HTTP upload stream writer
- [x] `internal/nutanix/volume_group.go` — Volume Group CRUD, iSCSI portal discovery, client whitelisting
- [x] Three transport modes: `iscsi` (production), `stream` (Mac testing), `image` (legacy)
- [x] OOM fix: chunked 64MB blocks instead of single disk-size allocation
- [x] Transfer speed: 1.3 MB/s → 8.5 MB/s (buffered streaming)
- [x] Successful RHEL6 migration via stream transport (NFC → VMDK → qcow2 → bootable on AHV)
- [x] Dockerfile for containerized deployment
- [x] Image names with datetime suffix for uniqueness

```bash
# Build for Linux and deploy to migration host
make build-linux
scp bin/datamigrate-linux-amd64 ubuntuadmin@15.204.34.202:~/datamigrate

# Full sync via iSCSI transport (on migration host)
./datamigrate migrate start --plan configs/ubuntu-vm-plan.yaml

# Full sync via stream transport (on Mac for testing)
datamigrate migrate start --plan configs/ubuntu-vm-plan.yaml
```

### Phase 8b: iSCSI End-to-End Testing ✅

**Completed (2026-03-21):**
- [x] Pure Go iSCSI initiator — RFC 3720/7143 (Login, WRITE, Data-Out, R2T, Logout)
- [x] Volume Group creation + disk attachment + iSCSI client whitelisting
- [x] Portal discovery with data services IP + Prism Central fallback
- [x] Two-phase iSCSI login (CSG=00→NSG=01, then CSG=01→NSG=11) — Nutanix rejects single-phase
- [x] iSCSI target redirect handling — Data Services IP (`172.16.3.254:3260`) redirects to CVM Stargate (`172.16.1.x:3205`)
- [x] CmdSN fix — login PDUs are Immediate and don't advance CmdSN; must use target's `ExpCmdSN` (was causing silent command drops)
- [x] READ CAPACITY(10) — confirms block_size=512, capacity=300GB, 629M sectors
- [x] Sense data parsing — ASC/ASCQ decode for debugging SCSI errors
- [x] `InitialR2T` / `ImmediateData` negotiation and honoring
- [x] MaxBurstLength write splitting (64MB chunks → 16MB per WRITE command)
- [x] Migration host VM (`migration-host`, 8GB RAM) on Nutanix with dual Basic NICs

**RESOLVED (2026-03-22):**
- WRITE(10) with 1 MB chunks (2048 blocks) — **WORKING!** T0 sync in progress.
- Root cause: Nutanix Stargate rejects transfer lengths > ~8192 blocks per SCSI command
- 1-block test WRITE succeeded → confirmed WRITE works, just transfer size limit
- Memory stable at ~300 MB (vs 1.3 GB with 16 MB chunks)

**iSCSI protocol bugs fixed (2026-03-21/22):**
1. **Single-phase login rejected** → two-phase (security then operational)
2. **Target redirect** → Data Services IP is discovery-only, CVM Stargate port 3205 is data path
3. **CmdSN off-by-one** → target silently dropped WRITE commands (root cause of hangs/0% CPU)
4. **ExpStatSN** → must be `StatSN + 1`, not raw `StatSN`
5. **MaxRecvDataSegmentLength** → accept target's value (1MB), don't cap to our default
6. **Task attribute** → UNTAGGED (0x00) → SIMPLE (0x01), required by Nutanix Stargate
7. **Transfer length** → 32768 blocks (16MB) rejected → 2048 blocks (1MB) works

**T0 full sync completed (2026-03-22):**
- [x] T0 full sync: 300 GB in ~81 minutes (~62 MB/s) via iSCSI WRITE(10) with 1 MB chunks
- [x] Started ~18:38 UTC, completed ~20:00 UTC
- [x] Memory stable at ~300 MB (8 GB VM), CPU ~13%
- [x] vCenter session auto-reconnected after timeout during long transfer
- [x] Migration state: FULL_SYNC → SYNCING

```bash
# T0 full sync command (run on migration-host)
$ ./datamigrate migrate start --plan configs/ubuntu-vm-plan.yaml

# Example output:
# INF creating volume group name=datamigrate-ubuntu-vm
# INF volume group created uuid=cc55add2-e201-46a5-4876-ede01de4543c
# INF iSCSI client attached
# INF starting full sync for disk 0 (300 GB)
# INF iSCSI portal selected portal_ip=172.16.3.254 port=3260
# INF iSCSI login successful target=iqn.2010-06.com.nutanix:datamigrate-ubuntu-vm
# ... progress bar ...
# INF full sync complete bytes=322122547200 duration=81m rate=62MB/s
# INF migration state: FULL_SYNC → SYNCING
```

**Step 9: VM Creation from Volume Group — COMPLETED (2026-03-22):**
- [x] Added `datamigrate createvm` CLI command
- [x] `ListVolumeGroups()` + `FindVolumeGroupByName()` — v4 API to find VG by name
- [x] `ListISCSIClients()` — v4.2 API (`/external-iscsi-attachments`) to list attached iSCSI clients
- [x] Fixed `DetachVGFromExternal()` — must specify client UUID per detach (v4 API requires it)
- [x] VM creation with UEFI/Legacy boot type support (`--boot-type` flag, defaults to UEFI)
- [x] `AttachVGToVM()` — attaches VG as passthrough disk (shows as `scsi.0` in Prism Central)
- [x] Ubuntu 24.04.3 LTS boots successfully on AHV from VG disk — **migration validated!**

**`createvm` command and example output:**
```bash
$ ./datamigrate createvm --plan configs/ubuntu-vm-plan.yaml --volume-group datamigrate-ubuntu-vm --vm-name ubuntu-vm-ahv --power-on

Looking up Volume Group "datamigrate-ubuntu-vm"...
INF listing volume groups
INF volume groups listed count=19
Found Volume Group: datamigrate-ubuntu-vm (UUID: cc55add2-e201-46a5-4876-ede01de4543c)
Detaching iSCSI clients from Volume Group...
INF detaching external iSCSI access from volume group vg=cc55add2-e201-46a5-4876-ede01de4543c
INF no iSCSI clients attached to volume group
Creating VM "ubuntu-vm-ahv" (2 vCPU, 4096 MB RAM)...
INF creating VM on Nutanix AHV name=ubuntu-vm-ahv
INF VM created uuid=75ee0194-5804-4e37-a761-78ba1ade0156
INF task completed task=46c2d5a6-043a-45ba-bfba-df660ae4ae5b
VM created: ubuntu-vm-ahv (UUID: 75ee0194-5804-4e37-a761-78ba1ade0156)
Attaching Volume Group "datamigrate-ubuntu-vm" to VM...
INF attaching volume group to VM vg=cc55add2-e201-46a5-4876-ede01de4543c vm=75ee0194-5804-4e37-a761-78ba1ade0156
INF v4 task completed
Volume Group attached successfully.
Powering on VM...
INF powering on VM uuid=75ee0194-5804-4e37-a761-78ba1ade0156
INF task completed
VM is now powered on.

Done! VM "ubuntu-vm-ahv" is ready.
  UUID: 75ee0194-5804-4e37-a761-78ba1ade0156
  Volume Group: datamigrate-ubuntu-vm
```

**Flags:**
- `--plan` (required): Migration plan file (reads Nutanix creds from `nutanix.creds` in same directory)
- `--volume-group` (required): Volume Group name on Nutanix (found by name via v4 API)
- `--vm-name` (optional): VM name on AHV (defaults to `<vm_name>-ahv`)
- `--boot-type` (optional): `UEFI` (default) or `LEGACY` — must match source VM boot type
- `--power-on` (optional): Power on the VM after creation

**Learnings from VM creation (2026-03-22):**
1. **No "Clone from Volume Group" in Prism Central UI** — only options are: Allocate on Storage Container, Clone from Image, Clone from another VM Disk. Must use API.
2. **VG attach = passthrough disk** — disk shows as 0 GiB in UI but data is there. VM boots from it.
3. **iSCSI client detach requires client UUID** — v4 API `detach-iscsi-client` action requires `extId` of the specific client. List clients first via `/external-iscsi-attachments` (v4.2 API).
4. **Boot type (UEFI vs Legacy) cannot be changed after VM creation** — must delete and recreate if wrong.
5. **Source VM boot type must match target** — EFI source VM needs UEFI boot on AHV. Legacy BIOS will hang at "Booting from hard disk..."
6. **VG disk is not a cloned disk** — it's shared/passthrough access to the Volume Group. For production, may want to clone VG disk into a proper VM disk.

**Completed (2026-03-22):**

**Step 10a: rhel6-test Stream Transport — Full Lifecycle** ✅
- T0 full sync via stream transport (NFC → VMDK → qcow2 → upload as Image)
- Created VM from image in Prism UI — boots with Legacy BIOS ✅
- T1 incremental sync (full re-download due to CBT bug, but upload + VM creation worked) ✅
- Verified delta changes (50MB test file) present on destination ✅
- **Bugs found & fixed**: image not re-uploaded on sync, negative progress display, session re-login noise

**Step 10b: rhel6-test iSCSI Transport — Boot Failure** ❌
- T0 full sync via iSCSI completed but VM failed to boot (kernel panic)
- Root cause: VG passthrough uses virtio-scsi controller, RHEL6 initramfs lacks virtio drivers
- Fix needed: pre-migration virtio driver injection (see Phase 9b)

**Next steps (resume here):**

**Step 10c: Test rhel6-test with CBT Reset Fix**
```bash
# Fresh T0 with CBT reset — tests Bug 1 fix
./datamigrate cleanup --plan configs/rhel6-test-plan.yaml
rm -f /tmp/datamigrate/rhel6-test/state.db
./datamigrate migrate start --plan configs/rhel6-test-plan.yaml
# Watch for: "CBT reset complete" and valid changeId captured
# Then make changes on source and test incremental:
./datamigrate migrate sync --plan configs/rhel6-test-plan.yaml
# This time sync should be INCREMENTAL (only deltas), not full re-download
```

**Step 10d: Test ubuntu-vm Incremental Sync (iSCSI)** — NOT YET TESTED
```bash
# ubuntu-vm T0 was done (300GB, iSCSI, completed 2026-03-21)
# Need to test incremental sync:
# 1. Make changes on source ubuntu-vm (see docs/incremental-sync-test.md)
# 2. Run fixstate to capture changeID (T0 didn't save it):
./datamigrate migrate fixstate --plan configs/ubuntu-vm-plan.yaml
# 3. Make MORE changes after fixstate
# 4. Run incremental sync:
./datamigrate migrate sync --plan configs/ubuntu-vm-plan.yaml
# This should transfer only deltas via iSCSI — the real test of incremental
```

**Step 11: Convert VG Passthrough Disk to Regular VM Disk**

VG passthrough disks work but are not ideal for production (shows 0 GiB, tied to VG).
Commercial tools (Move, Zerto, Veeam) avoid this by running on-cluster with direct vDisk access.
See `docs/comparemigrateapproach.md` for full comparison.

Best approach — **v2 API vDisk Clone** (zero extra data movement):
```bash
# 1. Get VG's internal vmdisk_uuid via v2 API (hit Prism Element / CVM IP)
curl -sk -u admin:PASS \
  'https://CVM_IP:9440/api/nutanix/v2.0/volume_groups/VG_UUID' \
  | jq '.disk_list[].vmdisk_uuid'

# 2. Create VM with disk cloned from that vDisk (fast CoW clone, ~seconds)
curl -sk -u admin:PASS -X POST \
  -H "Content-Type: application/json" \
  -d '{
    "name": "ubuntu-vm-ahv",
    "memory_mb": 4096, "num_vcpus": 2,
    "vm_disks": [{
      "vm_disk_clone": {
        "disk_address": { "vmdisk_uuid": "VMDISK_UUID" },
        "minimum_size": 322122547200
      }
    }],
    "vm_nics": [{"network_uuid": "SUBNET_UUID"}],
    "boot": {"uefi_boot": true}
  }' \
  'https://CVM_IP:9440/api/nutanix/v2.0/vms'

# 3. Delete VG — VM disk is now independent
```

Fallback approaches if v2 clone doesn't work with VG vDisks:
- **Image upload**: Read VG disk via iSCSI → upload as Image → create VM from image (Veeam's approach)
- **dd inside VM**: Boot from VG, add empty disk, `dd if=/dev/sda of=/dev/sdb`, swap disks

**Status**: Needs testing — validate v2 API `vm_disk_clone` with VG `vmdisk_uuid`

**Step 12: Test Cutover**
```bash
./datamigrate cutover --plan configs/ubuntu-vm-plan.yaml
# Final delta sync → shutdown source VM → detach iSCSI → create AHV VM from VG → power on
# This is the production migration workflow
```

**Step 13: Add NIC to createvm**
- Currently `network_map` is empty in ubuntu-vm-plan.yaml → VM created without NIC
- Add `--subnet` flag or populate `network_map` in plan YAML

### Phase 9: Incremental Sync via iSCSI + Cutover (TODO)
- [ ] `migrate sync` with CBT deltas via iSCSI WriteAt (only changed blocks)
- [ ] `cutover` command: final delta → shutdown source → detach iSCSI → create AHV VM from VG → power on
- [ ] Parallel multi-disk migration

### Phase 9b: Virtio Driver Injection for iSCSI Transport (TODO)

**Problem**: VMs migrated via iSCSI transport use VG passthrough disks, which AHV presents via virtio-scsi controller. Guest VMs (especially older Linux like RHEL6) don't have virtio drivers in their initramfs — they boot with VMware's LSI Logic/PVSCSI drivers. Result: kernel panic at boot (`wrong fs type, bad superblock on /dev/mapper/...`).

**Confirmed**: RHEL6 boots fine via stream transport (regular disk, compatible controller) but fails via iSCSI (VG passthrough, virtio-scsi). This is the same issue Nutanix Move solves with post-migration virtio driver injection via helper ISO.

**Options**:
- [ ] **Pre-migration**: `datamigrate prepare --vm <name>` — SSH into source VM and rebuild initramfs with virtio drivers (`dracut --add-drivers "virtio virtio_pci virtio_blk virtio_scsi virtio_net"`)
- [ ] **Post-migration**: Mount a VirtIO helper ISO on the AHV VM, boot from it, inject drivers
- [ ] **Hybrid**: After VG-to-regular-disk conversion (Step 11), the disk is no longer passthrough — may use compatible controller
- [ ] **Detection**: During `discover`, check guest OS and warn if virtio drivers are needed

**Affected**: Older Linux (RHEL 5/6/7 without virtio in initramfs), Windows (needs VirtIO drivers installed pre-migration). Ubuntu 18.04+ and RHEL 8+ typically have virtio drivers built-in.

### Phase 9c: Known Bugs — Fixed

**Bug 1: changeId read from wrong source — LIVE VM instead of SNAPSHOT** ✅ FIXED
- Root cause: `GetSnapshotChangeID` read `backing.ChangeId` from live VM config (`vm.Reference()`) — always empty
- The VDDK Programming Guide states changeId MUST be read from `snapshot.config.hardware.device[n].backing.changeId`
- Fix: Switched all callers to `GetSnapshotDiskChangeID` which reads from the snapshot's device config
- **Confirmed via `diagnose cbt`**: snapshot changeId = `"52 da a2 fc..."` (valid!), VM config changeId = `""` (empty)
- CBT IS working on OVH — we were just reading from the wrong place

**Bug 2: `changeId="*"` (all-allocated-blocks) query fails on OVH ESXi** ⚠️ KNOWN LIMITATION
- `QueryChangedDiskAreas` with `changeId="*"` returns `startOffset` error on this ESXi
- **Does NOT affect incremental sync** — incremental uses real changeId, not `"*"`
- Only affects the "show all allocated blocks" query (used for iSCSI T0 thin-disk optimization)
- Workaround: iSCSI T0 falls back to sequential flat VMDK read (works for thick disks, needs care for thin)
- May be ESXi version specific (vmx-11 / ESXi 6.0)

**Bug 3: vCenter session re-login fails after long transfers** ✅ FIXED
- Root cause: `govClient.Login()` re-auth fails on OVH managed vSphere after session expiry
- Fix: `Relogin()` now always creates a fresh connection directly (skips the failing session re-login attempt). No more noisy warning.

**Bug 4: `plan create` output doesn't show transport mode** 🐛 TODO
- `plan create` should print the transport mode (iscsi/stream/image) in its output so user can verify
- Default transport is `stream`, easy to accidentally start T0 with wrong transport

**Bug 5: `plan create` default transport should be `iscsi`** 🐛 TODO
- Default is `stream` but production use case is `iscsi`
- Should default to `iscsi` on Linux, `stream` on macOS (or just default to `iscsi`)

**Bug 6: `plan create` should auto-detect boot type (UEFI/LEGACY) from VMware** 🐛 TODO
- Currently user must manually specify `--boot-type` when creating VM
- VMware VM config has firmware info (bios vs efi) — should capture in discovery and save to plan
- `createvm` should default to the detected boot type from the plan

### Phase 10: Cutover — VG to Native AHV VM Disk (TODO)

**Problem**: VG passthrough does not boot — UEFI/GRUB can't find bootloader on virtio-scsi VG disk.
PE v2 `vm_disk_clone_spec` requires CVM access. PC v3 `source_uri` with `nfs://` rejects all IPs (loopback, CVM).
Third-party tools like datamigrate should only talk to Prism Central, never directly to CVMs.

**Solution**: Read VG over iSCSI → convert to qcow2 → upload as image → create VM from image.

**Full Migration Workflow (iSCSI Transport)**:
```
Phase 1: T0 Full Sync
  VMware snapshot → CBT → flat VMDK → iSCSI write → Nutanix Volume Group
  (50GB disk ≈ 10-15 min)

Phase 2: T1..TN Incremental Syncs (repeat as needed)
  VMware snapshot → CBT delta query → HTTP Range read changed blocks → iSCSI write at offsets → VG updated
  (25MB delta ≈ 13 sec)

Phase 3: Final Sync + Cutover
  a) Last incremental sync (minimal delta)
  b) Read VG disk over iSCSI → raw file on migration-host (internal network, fast)
  c) Convert raw → qcow2 (qemu-img, compresses unallocated blocks → small file)
  d) Upload qcow2 as Nutanix Image via PC v3 API
  e) Create VM from image via PC v3 API
  f) Boot VM, validate, done
```

**Why this works**:
- VG is a staging area that stays in sync with VMware via incremental CBT
- Conversion to native disk only happens once at cutover
- qcow2 is compressed — 50GB disk with 5GB used ≈ 5GB qcow2
- All network traffic in Phase 3 is internal (VG→migration-host→PC), very fast
- Only uses Prism Central APIs — no CVM access required

**Downtime window** = last incremental sync + VG read + qcow2 convert + image upload + VM creation

**Implementation: `datamigrate cutover` command**:
1. Run final incremental sync
2. Connect to VG via iSCSI, read full disk to raw file
3. `qemu-img convert -f raw -O qcow2` raw → qcow2
4. Create image via PC v3 API, upload qcow2
5. Create VM from image with correct CPU/RAM/NIC/boot-type
6. Optionally power on

**Approaches ruled out**:
- VG passthrough: UEFI/GRUB boot fails, virtio-scsi driver issues
- PE v2 `vm_disk_clone_spec`: Requires CVM/PE direct access
- PC v3 `source_uri` with `nfs://`: PC rejects all NFS host IPs
- `acli image.create`: Requires CVM SSH access

### Phase 11: VDDK Transport + Polish (TODO)
- [ ] `internal/transport/vddk.go` — CGo VDDK bindings for SAN/HotAdd performance
- [ ] Enhanced retry logic with per-extent retry
- [ ] End-to-end integration tests against real vCenter + Nutanix

---

## State Machine

```
CREATED ──► FULL_SYNC ──► SYNCING ◄──► CUTOVER_READY ──► CUTTING_OVER ──► COMPLETED
   │            │            │              │                  │
   └────────────┴────────────┴──────────────┴──────────────────┘
                              FAILED
```

---

## Error Handling

| Scenario | Strategy |
|---|---|
| Network timeout on VMware/Nutanix API | Retry 3x with exponential backoff (1s base, 30s max) |
| Block read I/O error | Retry individual extent 3x |
| CBT changeId invalidated | Fall back to full sync for that disk |
| Orphaned snapshot (process crash) | Detect on startup, `cleanup` removes them |
| Nutanix upload failure | Resume from last uploaded chunk |
| Snapshot creation failure (VM locked) | Fail immediately, report to user |

---

## Design Decisions

1. **Local staging** (write raw locally, convert to qcow2, then upload) — simpler and more reliable than streaming directly to Nutanix
2. **NBD first, VDDK later** — pure Go with zero external deps; VDDK is a Phase 8 performance optimization
3. **qcow2 via raw+convert** for MVP — avoids implementing qcow2 format internals; uses `qemu-img convert -c`
4. **BoltDB for state** — embedded, no external DB dependency; CLI is self-contained
5. **Nutanix v3 API for images/VMs, v4 API for Volume Groups** — v3 has wider compatibility; v4 needed for VG iSCSI management
6. **govmomi.NewClient()** — direct connection, not session/cache (simpler for CLI usage)
7. **methods.QueryChangedDiskAreas()** — proper govmomi API call, not RoundTrip
8. **ChangeId from disk backing** — DiskChangeInfo doesn't carry changeId; must read from VirtualDiskFlatVer2BackingInfo
9. **Pure Go iSCSI** — no kernel modules (iscsi_tcp), no iscsiadm dependency; works on Mac, Linux, Docker, any OS
10. **Raw datastore reader for iSCSI** — flat VMDK = raw sector bytes; NFC gives streamOptimized VMDK which needs conversion
11. **Three transport modes** — iSCSI (production), stream (Mac dev), image (legacy) — different reader/writer combos for different environments
12. **Migration host on Nutanix** — deploy tool on VM inside cluster to avoid firewall/anti-DDoS issues with iSCSI port 3260
13. **VG passthrough for VM boot** — attach VG to VM as passthrough disk (not clone); VM boots directly from VG data. Avoids extra clone/copy step.
14. **UEFI default for createvm** — most modern Linux VMs use EFI boot; Legacy BIOS hangs if source was EFI. Boot type cannot be changed after VM creation on AHV.
15. **v4.2 API for iSCSI client listing** — `/external-iscsi-attachments` endpoint only exists in v4.2; v4.1 `/iscsi-clients` returns 404

---

## Nutanix API Notes

Critical learnings for anyone modifying the Nutanix integration:

- **No "Clone from Volume Group" in Prism Central UI** — must use API to attach VG to VM
- **VG attach is passthrough** — disk shows as 0 GiB in Prism UI but data is there and bootable
- **Detach iSCSI client requires client UUID** — empty body returns `VOL-40101` error; must list clients first via `GET /external-iscsi-attachments`, then detach each by `extId`
- **iSCSI client listing endpoint**: `GET /api/volumes/v4.2/config/volume-groups/{vgUUID}/external-iscsi-attachments` (NOT `/iscsi-clients`)
- **Boot type is immutable** — `UEFI` vs `LEGACY` must be set at VM creation; cannot be changed after
- **EFI source VM must use UEFI boot on AHV** — Legacy BIOS will hang at "Booting from hard disk..."
- **v4 API versions**: use v4.1 for VG CRUD/attach/detach, v4.2 for listing iSCSI attachments

---

## govmomi API Notes

These are critical learnings for anyone modifying the VMware integration:

- `simulator.Test` takes `func(context.Context, *vim25.Client)`, NOT `*simulator.Client`
- `DiskChangeInfo` struct has NO `ChangeId` field — changeId comes from `VirtualDiskFlatVer2BackingInfo.ChangeId`
- NFC `FileItem` uses `.URL` (type `*url.URL`), not `.Url`
- Use `methods.QueryChangedDiskAreas()` for CBT queries, not client `RoundTrip`
- Use `govmomi.NewClient()` directly, not `session/cache`

---

## VMware → Nutanix AHV Object Mapping

| VMware Object | Nutanix AHV Equivalent | Migration Behavior |
|---|---|---|
| VM | VM on AHV | Core migration object |
| vCPU / Memory | vCPU / Memory | Retained as-is from plan |
| VMDK | qcow2 disk image | Converted via block replay |
| vNIC | AHV NIC | Mapped to target subnet |
| Port Group | AHV Subnet | Manual mapping required |
| Datastore | Storage Container | Manual mapping required |
| Snapshots | Not migrated | Only live state is migrated |
| VM Tools | NGT (post-migration) | VMware Tools removed |
| Affinity Rules | Manual recreation | Not auto-migrated |

---

## Testing

All tests pass with `go test ./...`:

| Package | Test Type | Coverage |
|---|---|---|
| `internal/util` | Unit | size formatting, retry with backoff |
| `internal/config` | Unit | YAML load/save, plan serialization |
| `internal/state` | Unit | BoltDB CRUD, journal entries |
| `internal/blockio` | Unit | Pipeline with mock reader/writer |
| `internal/vmware` | Integration | govmomi simulator (discover, snapshot) |
| `internal/nutanix` | Integration | httptest mock (connection, subnets, images) |

---

## Build & Run

```bash
# Build
make build

# Run tests
make test

# Install globally
make install

# Quick test
./bin/datamigrate --help
./bin/datamigrate discover --vcenter vcenter.example.com --username admin --password secret
```

---

## What's Left for Production

1. **iSCSI end-to-end test** — pure Go initiator built, needs testing from a Linux VM on Nutanix (OVH anti-DDoS blocks Mac→port 3260)
2. **Deploy migration host on Nutanix** — Linux VM with internet access, direct access to data services IP (172.16.3.254:3260)
3. **Incremental sync via iSCSI** — `migrate sync` using CBT deltas + iSCSI WriteAt (code paths exist but untested)
4. **Cutover command for iSCSI** — detach iSCSI, attach VG disks to new AHV VM, power on
5. **VDDK CGo bindings** — for SAN/HotAdd transport (10x faster than NFC)
6. **Parallel multi-disk** — process multiple disks concurrently
7. **End-to-end integration tests** — against real vCenter + Nutanix environments
8. **Pre-migration checks** — validate disk compatibility, guest OS support
9. **Post-migration** — VMware Tools uninstall, NGT install guidance
10. **NSX-T** — no automated migration exists; manual network re-architecture required

---

## Recent Key Changes (as of 2026-03-17)

### New Components
| Component | File | Description |
|---|---|---|
| Pure Go iSCSI Initiator | `internal/iscsi/initiator.go` | RFC 3720 implementation: Login, WRITE(16), Data-Out, R2T, NOP-In, Logout. No kernel modules needed. Works on Mac/Linux. |
| Raw Datastore Reader | `internal/vmware/raw_reader.go` | Downloads flat VMDK directly via HTTPS. Raw sector bytes — perfect for iSCSI WriteAt. Strips snapshot suffixes. |
| ISCSIWriter | `internal/blockio/iscsi.go` | Rewritten to use pure Go initiator instead of iscsiadm + /dev/sdX. |
| Stream Writer | `internal/blockio/stream_writer.go` | 128MB buffered writer for HTTP upload. Progress logging every 30s. |
| Volume Group Manager | `internal/nutanix/volume_group.go` | Nutanix v4 API: create VG, add disks, attach iSCSI client (whitelist IQN), portal discovery. |
| Dockerfile | `Dockerfile` | Multi-stage alpine build with qemu-img. |

### Three Transport Modes
| Mode | Reader | Writer | Use Case |
|---|---|---|---|
| `iscsi` | Raw datastore (flat VMDK) | Pure Go iSCSI → Nutanix VG | Production: direct block writes, delta-efficient |
| `stream` | NFC lease (streamOptimized VMDK) | qemu-img → qcow2 → HTTP upload | Mac testing: no iSCSI needed |
| `image` | NFC lease (streamOptimized VMDK) | Qcow2Writer → HTTP upload | Legacy: block pipeline approach |

### Bugs Fixed
- **OOM Kill**: Single `make([]byte, 322GB)` → chunked 64MB blocks
- **Progress 4613%**: Cumulative values added as deltas → track lastTransferred, compute delta
- **NFC picked CD-ROM**: First-item fallback → match by size closest to disk capacity
- **Snapshot flat file 404**: `disk-000003-flat.vmdk` → strip suffix to `disk-flat.vmdk`
- **iSCSI login EOF**: VG name used as IQN → prepend `iqn.2010-06.com.nutanix:` prefix
- **vCenter relogin**: `soap.ParseURL` mangled special chars → use `url.UserPassword` directly
- **UEFI boot fail**: RHEL6 needs Legacy BIOS → set boot mode in Nutanix VM config

### Current Blocker (Updated 2026-03-21)

**The migration host needs access to ALL three endpoints:**
1. **vCenter/ESXi** (port 443) — to read VM disks via CBT
2. **Prism Central API** (port 9440) — to create Volume Groups, manage VMs
3. **iSCSI Data Services IP** (port 3260) — to write blocks to Volume Groups

**No single VM can currently reach all three:**

| VM | vCenter (443) | Prism API (9440) | iSCSI (3260) | Internet |
|---|---|---|---|---|
| ubuntu-vm (VMware, `15.204.37.85`) | ✅ | ✅ (public IP) | ❌ anti-DDoS blocks | ✅ |
| RHEL7 (Nutanix, `infra_pb6bh`, `172.16.0.50`) | ❌ no route | ✅ (`172.16.1.99`) | ✅ (`172.16.3.254`) | ❌ no internet |

### Networking Investigation Results (2026-03-21)

#### OVH Anti-DDoS (affects VMware → Nutanix)
- OVH anti-DDoS blocks **repeated** TCP connections to non-standard ports (3260, 2049, 22) on Nutanix public IP `147.135.96.30`
- First connection succeeds, second/third get refused — kills both iSCSI and NFS
- Affects all traffic from ubuntu-vm to Nutanix, even though both are inside OVH network
- Port 9440 (HTTPS/Prism) is **not** affected — always stable

#### Nutanix iSCSI Same-L2 Requirement
- **Nutanix mandates iSCSI initiators be on the same L2 broadcast domain as the Data Services IP**
- L3/routed access is NOT supported (Nutanix best practices: "Avoid routing between client initiators and CVM targets")
- Overlay subnets (e.g., `OVH-External-Subnet` 172.16.0.0/24) do NOT work — different L2 domain
- Only **`infra_pb6bh`** (VLAN 1, vs0) works — this is the CVM/host infrastructure network

#### Working iSCSI Setup (Confirmed)
- RHEL7 VM on Nutanix with NIC on `infra_pb6bh` (VLAN 1, vs0)
- Static IP: `172.16.0.50/22` on `eth4`
- `172.16.3.254:3260` is reachable ✅
- `172.16.1.99:9440` (Prism VIP) is reachable ✅
- No internet access, no route to VMware network

#### Nutanix Subnet Map
| Subnet | Type | VLAN | IP Prefix | iSCSI? | Internet? |
|---|---|---|---|---|---|
| `infra_pb6bh` | VLAN (Basic) | 1 (vs0) | 172.16.0.0/22 | ✅ same L2 | ❌ |
| `OVH-IPFO-Uplink-Subnet` | VLAN | 0 (vs0) | 51.81.192.212/30 | ❌ | ✅ (ext. connectivity) |
| `OVH-External-Subnet` | Overlay | - | 172.16.0.0/24 | ❌ overlay | ❌ |
| `vlan_network_11` | Overlay | - | 10.10.11.0/24 | ❌ VMware side | ❌ |

#### Nutanix Move Uses NFS, Not iSCSI
- Nutanix Move uses **NFS** (ports 2049, 111) — mounts Nutanix container as NFS datastore on ESXi
- NFS works across L3/routed networks — no same-L2 requirement
- Move does NOT use port 3260 or the data services IP
- However, NFS port 2049 is ALSO blocked by OVH anti-DDoS from ubuntu-vm

#### Internet on Nutanix VMs
- CVMs have internet (can download images via URL in Prism Central)
- VMs on `infra_pb6bh` do NOT have internet — no default gateway to IPFO
- IPFO block `51.81.192.212/30` has only 1 usable IP (likely used by cluster)
- OVH support investigated but could not resolve — said it may be a Nutanix setting
- **This is the key blocker** — a VM with both iSCSI and internet access would solve everything

### Solving the Internet Problem on Nutanix VMs

**Option A: Dual-NIC VM (infra + IPFO uplink)**
- NIC 1: `infra_pb6bh` → iSCSI access (`172.16.3.254:3260`)
- NIC 2: `OVH-IPFO-Uplink-Subnet` → internet access
- Problem: Attaching IPFO uplink NIC failed with UUID error, and `/30` has no spare IPs

**Option B: Route internet through a CVM**
- CVMs on `172.16.0.0/22` have internet routing
- Set a CVM IP as default gateway for the VM
- Problem: CVMs likely don't have IP forwarding enabled, and we can't SSH to CVMs

**Option C: Fix Nutanix networking (OVH + Nutanix support)**
- Get OVH/Nutanix to explain how VMs should get internet
- May need a larger IPFO block, or NAT configuration on the cluster
- This is the proper long-term fix

**Option D: SSH tunnel through ubuntu-vm (workaround)**
- If we can get the VM to reach ubuntu-vm (e.g., via a shared network), tunnel vCenter through SSH
- ubuntu-vm already has vCenter access

**Option E: Abandon iSCSI, use enhanced stream transport**
- Use ubuntu-vm with stream transport (already works, port 9440 only)
- Accept full re-upload each sync (30GB per sync for 300GB disk)
- Can enhance later with local delta patching to reduce VMware read size

### Root Cause Confirmed (2026-03-21)

**`51.81.192.213` is reserved by Nutanix for internal CVM use.** Attempting to create a Basic VLAN subnet with this IP gives:
```
Error: 51.81.192.213 is reserved for internal use
```

API confirms: `num_assigned_ips: 3` (3 CVMs), `num_free_ips: 0`, `external_connectivity_state: DISABLED`.

The VPC SNAT at `.213` was never going to work — the IP is consumed by CVMs and the external connectivity is disabled.

**Cannot mix Basic + Advanced NICs on the same VM.** Attempting to add an Overlay NIC (`OVH-External-Subnet`) to a VM with a Basic NIC (`infra_pb6bh`) gives:
```
Error: existing NICs use network type 'VLAN Basic network', conflicts with 'VLAN Advanced network'
```

### Solution: Order New IPFO Block from OVH

**Step 1: Order IPs (OVH Manager)** ✅ DONE
- Ordered `/29` block: **`15.204.34.200/29`**
- IPs available:

| IP | Status |
|---|---|
| `15.204.34.200` | Network address (unusable) |
| `15.204.34.201` | Usable — migration VM |
| `15.204.34.202` | Usable — spare |
| `15.204.34.203` | Usable — spare |
| `15.204.34.204` | Usable — spare |
| `15.204.34.205` | Usable — spare |
| `15.204.34.206` | OVH Gateway (verify — usually second-to-last or last usable) |
| `15.204.34.207` | Broadcast (unusable) |

> **Note:** OVH gateway for /29 blocks is typically `.206` or `.207`. Verify in OVH Manager → IP → click on the block → check gateway.

**Step 2: Add to vRack**
- OVH Manager → Network → vRack → pn-2014805
- `15.204.34.200/29` appears under "Your eligible services" (left panel)
- Move it to "Your vRack" (right panel) alongside existing `51.81.192.212/30`

**Step 3: Create Basic VLAN Subnet in Prism Central**
- Networking → Subnets → Create Subnet
- Name: `new-ipfo-basic`
- Type: VLAN
- Virtual Switch: vs0
- VLAN ID: 0
- Do NOT enable "External Connectivity for VPCs" (keep it Basic)
- IP Management: Nutanix IPAM
- Network: `15.204.34.200/29`
- Gateway: `15.204.34.206` (verify in OVH Manager)
- IP Pool: `15.204.34.201` to `15.204.34.205`

**Step 4: Create Dual-NIC Migration VM (Both Basic)**
- NIC 1: `infra_pb6bh` (Basic, VLAN 1) → `172.16.0.50/22` → iSCSI access to `172.16.3.254:3260`
- NIC 2: `new-ipfo-basic` (Basic, VLAN 0) → `15.204.34.201` → internet + vCenter access
- Both NICs are Basic type → no conflict

**Step 5: Configure and Test**
```bash
# NIC 1 (infra — iSCSI)
ip addr add 172.16.0.50/22 dev eth0
ip link set eth0 up

# NIC 2 (new IPFO — internet)
ip addr add 15.204.34.201/29 dev eth1
ip link set eth1 up
ip route add default via 15.204.34.206 dev eth1
echo "nameserver 213.186.33.99" > /etc/resolv.conf

# Test internet
ping -c 2 8.8.8.8

# Test iSCSI
timeout 3 bash -c 'echo > /dev/tcp/172.16.3.254/3260' && echo "connected"
```

**Step 6: Deploy and Run datamigrate**
```bash
# Build on Mac
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o datamigrate-linux ./cmd/datamigrate

# SCP to migration VM (now has public IP)
scp datamigrate-linux root@15.204.34.201:~/
chmod +x datamigrate-linux

# Run iSCSI end-to-end test
./datamigrate-linux migrate start --plan ubuntu-vm-plan.yaml --transport iscsi
```

### Completed Steps (2026-03-21)
- ✅ Step 1: Ordered /29 IPFO block `15.204.34.200/29`
- ✅ Step 2: Added to vRack `pn-2014805`
- ✅ Step 3: Created Basic VLAN subnet `new-ipfo-basic` (VLAN 0, vs0, gateway `15.204.34.201`, pool `.202`-`.205`)
  - Note: `.206` reserved by Nutanix internally, `.201` used as gateway
  - Real OVH gateway is `51.81.192.214` — must add host route manually inside VM
- ✅ Step 4: Tested dual-NIC on RHEL7 VM — **BOTH iSCSI and internet working!**
  - eth6: `172.16.0.50/22` on `infra_pb6bh` → iSCSI `172.16.3.254:3260` ✅
  - eth7: `15.204.34.202/29` on `new-ipfo-basic` → `ping 8.8.8.8` ✅

### Step 7: Create Ubuntu Migration Host VM ✅

RHEL7 VM (kernel 2.6.32) is too old. Created a fresh Ubuntu 22.04 VM.

**7a. Upload Ubuntu Server ISO to Prism Central** ✅
- Prism Central → Images → Add Image → From URL
- URL: `https://releases.ubuntu.com/22.04/ubuntu-22.04.5-live-server-amd64.iso`
- Image Type: ISO
- Name: `ubuntu-22.04-server-iso`

**7b. Create VM** ✅
- Name: `migration-host`
- vCPUs: 2, RAM: 4GB
- Disk: 500 GB (SCSI)
- CD-ROM: Attach `ubuntu-22.04-server-iso`
- **Add BOTH NICs during VM creation** (cannot add mixed types later):
  - NIC 1: `infra_pb6bh` (Basic, VLAN 1) → iSCSI → `ens3`
  - NIC 2: `new-ipfo-basic` (Basic, VLAN 0) → internet → `ens4`
- Boot order: CD-ROM first

**7c. Install Ubuntu** ✅
- Ubuntu Server (full, not minimized)
- Hostname: `migration-host`
- User: `ubuntuadmin`
- OpenSSH server installed
- Mirrors worked via NIC 2 (ens4 got DHCP `15.204.34.203/29`)

**7d. Configure Networking (after install)** ✅

**Actual VM connectivity (confirmed 2026-03-21):**
- SSH: `ssh ubuntuadmin@15.204.34.202`
- Internet: `ping 8.8.8.8` ✅ (via ens4, IPFO gateway 51.81.192.214)
- iSCSI: `nc -w 3 172.16.3.254 3260` ✅ (via ens3, same L2 as Data Services IP)
- Prism API: `curl -sk https://172.16.1.99:9440` ✅ (via ens3)

Create netplan config `/etc/netplan/01-netcfg.yaml`:
```yaml
network:
  version: 2
  ethernets:
    ens3:
      addresses:
        - 172.16.0.50/22
      # No gateway — iSCSI only (same L2)
    ens4:
      addresses:
        - 15.204.34.202/29
      routes:
        - to: 51.81.192.214/32
          scope: link
        - to: 0.0.0.0/0
          via: 51.81.192.214
      nameservers:
        addresses:
          - 213.186.33.99
          - 8.8.8.8
```

Apply:
```bash
sudo netplan apply
```

Verify:
```bash
# Test internet
ping -c 2 8.8.8.8

# Test iSCSI
nc -zv 172.16.3.254 3260

# Test Prism API
curl -sk https://172.16.1.99:9440/api/nutanix/v3/clusters/list \
  -X POST -H "Content-Type: application/json" \
  -d '{"kind":"cluster"}' -u "admin2:~4Kn6F1-w~fDFYFe" | head -c 200
```

**7e. Security Policy** ✅
- Created category `ProdServers:Migration`, assigned to `migration-host`
- Created Flow security policy: allow all traffic from address group containing your IP
- This restricts SSH/access to only your known IPs

**7f. Deploy datamigrate** ✅
```bash
# Build on Mac
cd /Users/nitinmore/go/datamigrate
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o datamigrate-linux ./cmd/datamigrate

# SCP — NOTE: ssh-rsa workaround needed
# Option 1: scp with host key flag
scp -o HostKeyAlgorithms=+ssh-rsa -O datamigrate-linux ubuntuadmin@15.204.34.202:~/

# Option 2: pipe via ssh (works if scp fails)
cat datamigrate-linux | ssh ubuntuadmin@15.204.34.202 'cat > ~/datamigrate && chmod +x ~/datamigrate'

# Also transfer plan config
cat configs/ubuntu-vm-plan.yaml | ssh ubuntuadmin@15.204.34.202 'cat > ~/ubuntu-vm-plan.yaml'

# On migration-host
./datamigrate --help
```

**7g. iSCSI Login Fixes (discovered during testing)**
- Nutanix iSCSI target rejects single-phase login (CSG=00→NSG=11) with opReject (0x3f)
- Requires two-phase: security (CSG=00→NSG=01) then operational (CSG=01→NSG=11)
- Data Services IP (`172.16.3.254:3260`) is a discovery portal — redirects to actual CVM (e.g., `172.16.1.2:3205`)
- `ExpStatSN` must be `StatSN + 1` (not just `StatSN`) between login phases
- Whitelisted `15.204.34.200/29` in OVH VMware firewall to allow vCenter access from migration-host

**7h. Test iSCSI End-to-End** ✅ (T0 sync running)
```bash
# Validate connectivity
./datamigrate-linux validate --config ubuntu-vm-plan.yaml

# Start full sync (T0) with iSCSI transport
./datamigrate-linux migrate start --plan ubuntu-vm-plan.yaml --transport iscsi

# Check status
./datamigrate-linux migrate status --plan ubuntu-vm-plan.yaml
```

### Networking Model: Basic VLAN vs VPC Overlay

**Current setup uses Basic VLAN — direct public IP per VM (like static NAT 1:1), NOT shared SNAT like NSX-T.**

| | **NSX-T (VMware)** | **Nutanix Basic VLAN (current)** | **Nutanix VPC Overlay** |
|---|---|---|---|
| Type | Overlay + SNAT/DNAT | Direct public IP per VM | Overlay + SNAT |
| 1 public IP serves | Many VMs (NAT) | 1 VM | Many VMs (NAT) |
| More VMs? | Add NAT rules | Need more public IPs | Add VMs to VPC |
| iSCSI same-L2? | N/A | ✅ Works (Basic NIC) | ❌ Fails (different L2) |
| Firewall | NSX-T distributed FW | Flow Microseg or UFW | VPC security |

**Why Basic VLAN (not VPC):** Nutanix does NOT allow mixing Basic + Advanced NICs on the same VM. Migration host needs iSCSI (Basic NIC on `infra_pb6bh`, same L2 as Data Services IP `172.16.3.254`) AND internet. Both must be Basic type.

**IP allocation (15.204.34.200/29):**
| IP | Status |
|---|---|
| `.200` | Network address |
| `.201` | Gateway (dummy, in Prism subnet config) |
| `.202` | `migration-host` ✅ |
| `.203` | Available |
| `.204` | Available |
| `.205` | Available |
| `.206` | Reserved by OVH/Nutanix |
| `.207` | Broadcast |

**3 IPs remaining** for additional VMs. For NAT-style shared IP, would need VPC overlay — but that breaks iSCSI requirement.

### Next Steps (After Migration Host is Ready)
1. **Test iSCSI end-to-end** — full T0 sync of ubuntu-vm (300GB disk)
2. **Test incremental sync** — `migrate sync` with CBT deltas via iSCSI WriteAt
3. **Test cutover** — final delta → shutdown source → detach iSCSI → create AHV VM → power on
4. **Implement Phase 9** — incremental sync + cutover for iSCSI transport (code changes if needed)

### Key Infrastructure Details
| Component | Value |
|---|---|
| ubuntu-vm (VMware) | `15.204.37.85`, user: `ubuntuadmin`, network: `10.10.11.20` |
| RHEL7 (Nutanix, datamigrate-rhel7) | `172.16.0.50/22` on `eth4` (infra_pb6bh) |
| Nutanix Prism Central | `cluster-1919.nutanix.ovh.us` / `147.135.96.30:9440` |
| Nutanix Prism Central (internal) | `172.16.1.99:9440` / `172.16.1.100:9440` |
| Nutanix Data Services IP (iSCSI) | `172.16.3.254:3260` |
| Nutanix IPFO Gateway | `51.81.192.214` |
| Nutanix IPFO Block | `51.81.192.212/30` |
| Nutanix Infra Subnet | `infra_pb6bh`, VLAN 1, vs0, `172.16.0.0/22` |
| vCenter | `pcc-147-135-35-91` |
| OVH IPMI | Available on nodes ns1027233, ns1029300, ns1029318 (KVM console works, but AHV root password unknown) |
| Nutanix API creds | user: `admin2`, host: `cluster-1919.nutanix.ovh.us` |

---

## Technical FAQ

### Q1. How do I use this utility? What is the step-by-step workflow?

The workflow is a sequence of CLI commands. You do NOT need to touch vCenter UI or Nutanix Prism UI for the migration itself.

```bash
# Step 1: Set up credentials (one-time)
# Create configs/vmware.creds and configs/nutanix.creds with your vCenter + Prism Central details
cp configs/example.yaml configs/vmware.creds   # edit with vCenter host/user/pass
cp configs/example.yaml configs/nutanix.creds   # edit with Prism Central host/user/pass

# Step 2: Discover VMs on your vCenter
datamigrate discover --vcenter vcenter.example.com --username admin@vsphere.local --password secret

# Step 3: Validate connectivity to both source and target
datamigrate validate --config configs/

# Step 4: Create a migration plan for a specific VM
# Reads creds from configs/ dir (default). Use --transport iscsi for best performance.
datamigrate plan create --vm "web-server-01" --transport iscsi

# Step 5: Review the generated plan
datamigrate plan show configs/web-server-01-plan.yaml

# Step 6: Start full sync (T0) — this is the big initial copy
datamigrate migrate start --plan configs/web-server-01-plan.yaml

# Step 7: Run incremental syncs (T1, T2, ...) — only changed blocks
datamigrate migrate sync --plan configs/web-server-01-plan.yaml
# Repeat this as many times as needed (e.g., daily, hourly)

# Step 8: Check progress at any time
datamigrate migrate status --plan configs/web-server-01-plan.yaml

# Step 9: Final cutover — shuts down source, does last sync, boots on Nutanix
datamigrate cutover --plan configs/web-server-01-plan.yaml --shutdown-source

# Step 10: Clean up snapshots and staging files
datamigrate cleanup --plan configs/web-server-01-plan.yaml
```

The source VM **keeps running** throughout Steps 6-7. Downtime only happens at Step 9 (cutover), and it's limited to the time for the final delta sync + VM boot — typically **minutes, not hours**.

---

### Q2. Do I need to install/copy this tool onto a VM inside vCenter? Or can it run from any machine?

**No, you do NOT need to install anything inside vCenter or on any ESXi host.**

`datamigrate` runs from **any machine** that has network access to:
1. **vCenter API** (port 443) — for VM discovery, snapshots, CBT queries, block reading
2. **Nutanix Prism Central API** (port 9440) — for image upload, VM creation

```
                    ┌─────────────────┐
                    │  Your Laptop /  │
                    │  Jump Server /  │
                    │  Any Linux VM   │
                    │                 │
                    │  datamigrate    │
                    │  (runs here)    │
                    └───┬─────────┬───┘
                        │         │
           vSphere API  │         │  Prism Central API
           (port 443)   │         │  (port 9440)
                        ▼         ▼
                ┌──────────┐  ┌──────────┐
                │  vCenter │  │ Nutanix  │
                │  Server  │  │  Prism   │
                └──────────┘  └──────────┘
```

**Recommended placement**: Run on a machine that is **network-close to both vCenter and Nutanix** — ideally a jump server or utility VM in the same datacenter. This minimizes block transfer latency since the disk data flows through the machine running `datamigrate`.

The tool communicates with vCenter using the **govmomi** library (standard vSphere API), and with Nutanix using the **Prism Central v3 REST API**. No agents, no VIBs, no kernel modules, no guest OS changes.

---

### Q3. Where is the first T0 full copy written? Where does the data land?

The T0 full copy is written to a **local staging directory** on the machine running `datamigrate`. This is configurable in two places:

**In the config file (`myconfig.yaml`)**:
```yaml
staging:
  directory: "/data/datamigrate-staging"   # <-- T0 copy goes here
```

**Default**: `/tmp/datamigrate` if not specified.

**What gets created on disk during T0**:
```
/data/datamigrate-staging/
├── web-server-01-migration/        # Per-plan directory
│   ├── disk-2000.raw               # Full raw disk image (sparse file, actual size = used blocks)
│   └── disk-2000.qcow2             # Converted qcow2 (compressed, smaller than raw)
└── state.db                        # BoltDB state file (migration status, changeIds)
```

**Important sizing note**: The `.raw` file is a **sparse file** — it is allocated to the full disk capacity (e.g., 200 GB) but only consumes actual space for written blocks. The `.qcow2` file is **compressed** and typically 30-60% of the used space. So a 200 GB disk with 50 GB used might produce a ~20-30 GB qcow2 file.

**After T0 completes**, the qcow2 file is **uploaded to Nutanix** as a disk image via the Prism Central API. The local copy remains for incremental patching.

---

### Q4. Do I need to give a datastore path to store these files?

**No datastore path is needed.** You provide a **local filesystem path** on the machine running `datamigrate`, not a VMware datastore or Nutanix container path.

The tool handles two separate storage concepts:

| What | Where | How you configure it |
|---|---|---|
| **Local staging** (raw/qcow2 files) | Local disk on the machine running `datamigrate` | `staging.directory` in config YAML |
| **Nutanix destination** (uploaded images) | Nutanix storage container | `--storage-map` flag maps VMware datastores to Nutanix containers |

```yaml
# In myconfig.yaml — this is a LOCAL path, not a datastore
staging:
  directory: "/mnt/fast-ssd/datamigrate"

# The storage-map flag tells Nutanix WHICH container to use
# datamigrate plan create ... --storage-map "datastore1:container-uuid-xyz"
```

**Disk space requirement**: You need enough local disk space for the largest VM disk being migrated. For a VM with a 500 GB disk (100 GB used), plan for ~100-150 GB of staging space.

---

### Q5. Are these files VMDK or some other format?

**They are NOT VMDK files.** The tool produces two formats during staging:

| File | Format | Purpose |
|---|---|---|
| `disk-NNNN.raw` | **Raw disk image** | Intermediate format — direct block-for-block copy of the virtual disk. Sparse file (doesn't consume full capacity). |
| `disk-NNNN.qcow2` | **qcow2 (QEMU Copy On Write v2)** | Final format — compressed, efficient. This is what gets uploaded to Nutanix. |

**Why not VMDK?**
- VMDK is a VMware-specific container format with metadata headers, descriptor files, and extent files
- Nutanix AHV natively supports **qcow2** and **raw** — it does NOT need VMDK
- We **read blocks** from the VMDK via the vSphere API, but we **write blocks** into a raw image, then convert to qcow2
- This is the same approach Veeam uses — blocks are replayed into a new format, never file-copied

**The conversion chain**:
```
VMware VMDK (on datastore)
    → vSphere API reads blocks
    → Raw disk image (local staging)
    → qemu-img convert → qcow2 (local staging)
    → Upload to Nutanix as disk image
    → Nutanix creates VM with this disk
```

---

### Q6. Can this T0 copy be sent to Nutanix and verified — can I boot the VM there to check?

**Yes, absolutely.** This is a key design feature. After T0 completes:

1. The qcow2 image is **already uploaded to Nutanix** as a disk image
2. You can **manually create a test VM** on Nutanix Prism using that image to verify it boots
3. The source VM **keeps running** — nothing is disrupted

**How to verify after T0**:
```bash
# After migrate start completes, check status
datamigrate migrate status --plan web-server-01-plan.yaml
# Look for IMAGE UUID in the output — this is the Nutanix image

# Option A: Use Prism Central UI
# Go to Images → find the uploaded image → Create VM from it → Boot → Verify

# Option B: Use the CLI cutover (but DON'T use --shutdown-source for testing)
# This creates a VM on Nutanix and boots it, without touching the source
datamigrate cutover --plan web-server-01-plan.yaml
# (without --shutdown-source, the source VM stays running)
```

**What you're verifying**:
- Guest OS boots on AHV hypervisor
- Filesystem is intact
- Applications start correctly
- Network connectivity works (with mapped subnets)

**Important**: The T0 copy is a **crash-consistent** point-in-time image. It's as if the VM lost power at the moment of the snapshot. Databases and apps with write-ahead logs (PostgreSQL, MySQL, etc.) will perform recovery on boot, just like after a power failure. This is normal and expected.

---

### Q7. After T0 is copied, how are the deltas (T1, T2, ...) stored on disk?

Deltas are **NOT stored as separate files**. They are **patched directly into the existing raw file**.

**Here's what happens during an incremental sync (e.g., T1)**:

```
1. Create new snapshot on VMware
2. Query CBT: "What blocks changed since T0?"
   → CBT returns: [(offset=8192, len=4096), (offset=1048576, len=65536), ...]
3. Read ONLY those changed blocks from VMware (not the full disk)
4. Write those blocks into the EXISTING disk-2000.raw at the correct offsets
5. Re-convert the patched raw file → new disk-2000.qcow2
6. Remove the VMware snapshot
7. Save the new changeId for next sync
```

**On disk, you see**:
```
/data/datamigrate-staging/web-server-01-migration/
├── disk-2000.raw       # SAME file, patched in-place with changed blocks
├── disk-2000.qcow2     # Re-converted after patching
└── state.db            # Updated with new changeId, sync count, timestamps
```

**The state.db tracks**:
- Which changeId to use for the next CBT query
- How many bytes were transferred in each sync
- Sync count (T0=1, T1=2, T2=3, ...)
- Timestamp of last sync

**Key insight**: There are no delta files like Veeam's `.vib` files. The raw image is a **living document** that gets patched with each sync. The qcow2 is re-generated each time. This is simpler than maintaining a chain of deltas.

---

### Q8. Can these deltas be used to update the copy on Nutanix?

**Yes — this is exactly what `datamigrate migrate sync` does.** Each incremental sync:

1. Patches the local raw file with only the changed blocks
2. Re-converts to qcow2
3. **Re-uploads the qcow2 to Nutanix**, replacing the previous image

```bash
# Run as many incremental syncs as you want
datamigrate migrate sync --plan web-server-01-plan.yaml   # T1
datamigrate migrate sync --plan web-server-01-plan.yaml   # T2
datamigrate migrate sync --plan web-server-01-plan.yaml   # T3
# Each one gets smaller because fewer blocks change between syncs
```

**Why this is efficient (with iSCSI transport — the default)**:
- T0 transfers 100 GB to Nutanix (all used blocks, written directly via iSCSI)
- T1 transfers 5 GB (only changed blocks written to same Volume Group)
- T2 transfers 500 MB (only changed blocks)
- Final sync transfers 50 MB (only changed blocks)
- **10 syncs total: ~106 GB transferred**

**With image transport (legacy)**: every sync re-uploads the full 30 GB qcow2, so 10 syncs = 300 GB. The iSCSI approach is ~3x more efficient.

**After each sync, the Nutanix disk is up-to-date** with the source VM as of the snapshot moment. You can verify by booting a test VM from it at any time.

---

### Q9. Can I reconstruct T0 + T1 delta together and boot the VM?

**Yes — and with iSCSI transport, there's nothing to "reconstruct."**

With **iSCSI transport** (default), blocks are written directly to the Nutanix Volume Group at exact offsets. T0 writes all blocks, T1 overwrites only changed blocks at their exact positions. The Volume Group disk is always a complete, bootable disk — there are no separate files to merge.

With **image transport**, the local raw file is patched in-place and the merged result is re-uploaded:

```
After T0:     VG disk = all blocks as of snapshot T0
After T1:     VG disk = T0 blocks + T1 changed blocks overwritten in-place
After T2:     VG disk = T0 + T1 + T2 all merged in-place
...
After TN:     VG disk = complete disk as of snapshot TN
```

**The qcow2 on Nutanix is always a complete, bootable disk image.** You can create a VM and boot from it after any sync — T0, T1, or T47. You never need to "apply deltas in order" or "merge chains." That complexity is handled internally.

This is the same principle Veeam uses: *"Vendors hide deltas and replay blocks internally. The final output is always one clean disk."*

---

### Q10. Same question for T1 through TN — does each delta stack?

**Yes, deltas stack automatically.** Each `migrate sync` command:

1. Asks VMware CBT: "What changed since my last sync?"
2. Reads only those changed blocks
3. Patches them into the existing raw image
4. The raw image now represents the disk state at the new snapshot point

```
T0:  Read ALL blocks      → raw file is 100% of disk at time T0
T1:  Read 2% changed      → raw file is now 100% of disk at time T1
T2:  Read 0.5% changed    → raw file is now 100% of disk at time T2
T3:  Read 0.1% changed    → raw file is now 100% of disk at time T3
```

**Each sync gets smaller** because less data changes between shorter intervals. This is why the typical workflow is:
- T0: Full copy (hours, done during off-hours)
- T1-T5: Daily syncs (minutes each)
- T6-T10: Hourly syncs before cutover (seconds each)
- Final: Cutover sync after source shutdown (seconds, minimal changes)

**At every point**, the qcow2 on Nutanix is a complete, bootable image — not a chain that needs assembly.

---

### Q11. Once all deltas are on Nutanix (T0 through TN collected), can I start the VM on Nutanix and stop the source?

**Yes — this is exactly what the `cutover` command does.** It automates the entire sequence:

```bash
datamigrate cutover --plan web-server-01-plan.yaml --shutdown-source
```

**What happens internally during cutover**:

```
Step 1: Final incremental sync (TN+1)
        → Captures last few changed blocks while source is still running
        → iSCSI mode: writes delta blocks directly to Volume Group (fast)
        → image mode: patches raw, converts qcow2, re-uploads full file (slow)

Step 2: Power off source VM on VMware
        → Guest OS shuts down gracefully

Step 3: Post-shutdown sync (TN+2)
        → One more CBT query after shutdown — catches any blocks
           that were in-flight or in disk cache during shutdown
        → This is typically a few MB or nothing

Step 4: Create VM on Nutanix AHV
        → Maps CPU, memory, disks, NICs per the migration plan
        → iSCSI mode: detach iSCSI, attach Volume Group to VM
        → image mode: disks reference uploaded qcow2 images
        → NICs mapped to target subnets

Step 5: Power on target VM on Nutanix
        → VM boots on AHV
        → Guest OS starts, apps recover

Step 6: Migration marked as COMPLETED
```

**The downtime window** is only Steps 2-5: source shutdown + final sync + VM creation + boot. For most VMs this is **2-10 minutes**.

**Without `--shutdown-source`**: You can also run `cutover` without shutting down the source. This creates the VM on Nutanix and boots it, but the source keeps running. Useful for **testing** — verify the VM works on Nutanix before committing to the migration.

**Complete timeline visualization**:
```
Source VM: ████████████████████████████████████████░░ (shutdown)
                                                    ↑
T0 sync:  ████████████████░                         |
T1 sync:            ██░                             |
T2 sync:                 █░                         |
T3 sync:                    █░                      |
Cutover:                       ██ (final + create)  |
                                                    |
Target VM:                        ░░░░░░████████████████████
                                       ↑
                                    VM boots on Nutanix

Downtime: only this gap ─────────────►|◄──── 2-10 min
```

---

### Q12. Instead of writing raw/qcow2 to local disk, can we write to a datastore for speed and backup?

**With iSCSI transport (default), this question is moot** — there are no local files at all. Blocks go directly from VMware to the Nutanix Volume Group over iSCSI. No raw file, no qcow2, no local disk usage.

**For the legacy image transport**, writing to a VMware datastore instead of local disk is technically possible but not recommended:

| Factor | Local Disk | VMware Datastore | iSCSI (default) |
|---|---|---|---|
| **Write speed** | Fast (local SSD) | Slower (VMFS over SAN/NAS) | Direct to Nutanix |
| **Storage cost** | Uses migration machine disk | Consumes production datastore space | No intermediate storage |
| **Network hops** | 0 for local write | 1 (to datastore) + 1 (to Nutanix) | 1 (direct to Nutanix) |
| **Backup/durability** | Lost if machine fails | Survives host failure | Already on Nutanix |
| **IOPS impact** | None on production | Competes with VM workloads | None on VMware side |
| **Requires** | Enough local disk space | Datastore access + space | iSCSI network access to Nutanix |

**Why local disk was chosen for image transport:**
1. Datastores are shared — writing 100+ GB temp files impacts all VMs on that storage
2. You'd still need to transfer the file from the datastore to Nutanix (same network cost)
3. Accessing datastore files requires ESXi host credentials or vSphere file manager API — more complex

**Why iSCSI transport eliminates the concern entirely:**
- No intermediate files anywhere — blocks stream directly to Nutanix
- Resumable via block journal — if the process crashes, it picks up where it left off
- The "backup" aspect is inherent — the data is already on Nutanix storage, protected by Nutanix RF2/RF3 replication

**Recommendation:** Use the default `--transport iscsi`. It gives you the speed of direct writes, the backup durability of Nutanix storage, and zero local disk consumption.

---

### Q13. Where does the T0 full copy go? Where do T1..TN deltas go? Can I test-boot after each sync?

#### Image Transport (`--transport image`)

**T0 — Full copy:**
1. Every block is read from the VMware VM disk
2. Written to a local raw file: `/tmp/datamigrate/disk-0.raw`
3. Converted to qcow2: `/tmp/datamigrate/disk-0.qcow2`
4. Uploaded to **Nutanix Images** (visible in Prism Central → Images)
5. You can now create a VM from that image and **boot it to test** — it's a full disk

**T1..TN — Delta syncs:**
1. CBT tells us which blocks changed (e.g., blocks at offset 1000, 5000, 9000)
2. We open the **same local raw file** from T0
3. We **overwrite just those changed blocks** in the raw file (like editing a few pages in a book)
4. The raw file is now up-to-date (T0 + delta merged automatically)
5. Convert to qcow2 again
6. **Re-upload the entire qcow2** to Nutanix Images (replaces the previous version)
7. The image in Nutanix is a **complete, bootable disk** — you can test-boot it

**There is no separate "delta file" in Images.** The delta is merged into the raw file locally, and the full updated qcow2 replaces the old one. After each sync, what sits in Nutanix Images is always a complete disk.

```
T0:  [Read all blocks] → raw file (100 GB) → qcow2 (30 GB) → Upload to Images  ✅ Bootable
T1:  [Read Δ blocks]   → patch raw file    → qcow2 (30 GB) → Re-upload         ✅ Bootable
T2:  [Read Δ blocks]   → patch raw file    → qcow2 (30 GB) → Re-upload         ✅ Bootable

The image in Nutanix after each step:
  After T0: Complete disk (T0)
  After T1: Complete disk (T0 + T1 merged)
  After T2: Complete disk (T0 + T1 + T2 merged)
  Never a delta chain. Always one complete disk.
```

#### iSCSI Transport (`--transport iscsi`) — Default

**T0 — Full copy:**
1. A **Volume Group** is created on Nutanix (think: a raw hard drive on Nutanix)
2. Every block is read from VMware and written **directly** to the Volume Group via iSCSI
3. No local raw file, no qcow2, no upload step
4. The Volume Group disk is a complete, bootable disk — you can attach it to a test VM

**T1..TN — Delta syncs:**
1. CBT tells us which blocks changed
2. We write **only those changed blocks** to the same Volume Group at their exact offsets
3. The disk is automatically up-to-date (writing at the same position overwrites the old data)
4. Still a complete, bootable disk after each sync

```
T0:  [Read all blocks] → iSCSI WriteAt(offset, data) to Volume Group  ✅ Bootable
T1:  [Read Δ blocks]   → iSCSI WriteAt(offset, data) — only deltas    ✅ Bootable
T2:  [Read Δ blocks]   → iSCSI WriteAt(offset, data) — only deltas    ✅ Bootable

The Volume Group after each step:
  After T0: Complete disk (all blocks written)
  After T1: Complete disk (changed blocks overwritten in-place)
  After T2: Complete disk (changed blocks overwritten in-place)
  No merge needed — writing at the same offset IS the merge.
```

#### How to test-boot at any stage

**Image mode:**
1. Go to Prism Central → Images → find the uploaded image
2. Create a new VM → Add Disk → "Clone from Image" → select the image
3. Set CPU/RAM/NIC → Power On → VM boots

**iSCSI mode:**
1. Disconnect iSCSI from the datamigrate machine (pause migration)
2. Attach the Volume Group to a test VM in Prism
3. Power On → VM boots
4. To resume migration: detach from test VM, reconnect iSCSI, continue syncing

#### Simple analogy

**Image mode** = You have a printed document. Each time someone edits a paragraph, you reprint the **entire document** and mail all 30 pages again.

**iSCSI mode** = You're both editing the **same Google Doc**. Edits appear instantly at the right place. You only send the changed paragraph, not the whole document.

In both cases, the document (disk) is always complete and readable. There's never a pile of "change slips" that need assembling.

---

### Summary: End-to-End Flow

```
iSCSI TRANSPORT (default — network efficient):

  Machine running datamigrate (your laptop / jump server):

  T0:  VMware ──[all blocks]──────────────────────► Nutanix VG (iSCSI WriteAt)  100 GB
  T1:  VMware ──[Δ blocks only]──────────────────► Nutanix VG (iSCSI WriteAt)    5 GB
  T2:  VMware ──[Δ blocks only]──────────────────► Nutanix VG (iSCSI WriteAt)  500 MB
  ...
  TN:  VMware ──[Δ blocks only]──────────────────► Nutanix VG (iSCSI WriteAt)   50 MB
                                                                          Total: ~106 GB
  No local staging. No qcow2 conversion. No re-upload.
  Only changed bytes cross the network each sync.

  Cutover:
    1. Final Δ blocks via iSCSI (MB, not GB)
    2. Shutdown source VM
    3. Post-shutdown Δ blocks (a few MB)
    4. Detach iSCSI, attach VG to new VM on Nutanix AHV
    5. Power on target VM

────────────────────────────────────────────────────────────────────────

IMAGE TRANSPORT (legacy — simpler but wasteful):

  T0:  VMware ──[all blocks]──► /staging/disk.raw ──► qcow2 ──► Upload 30 GB
  T1:  VMware ──[Δ blocks]───► patch raw ──► qcow2 ──► Re-upload 30 GB
  T2:  VMware ──[Δ blocks]───► patch raw ──► qcow2 ──► Re-upload 30 GB
  ...
  TN:  VMware ──[Δ blocks]───► patch raw ──► qcow2 ──► Re-upload 30 GB
                                                                 Total: 300 GB

────────────────────────────────────────────────────────────────────────

Both modes:
  Nutanix disk is ALWAYS a complete, bootable disk — never a delta chain.
  Source VM runs throughout — downtime only at cutover (minutes).
  No agents on VMware. No agents on Nutanix. Just this CLI + network access.
```
