# Design: VDDK Transport via nbdkit

**Status:** Approved
**Date:** 2026-03-24
**Milestone 1:** T0 full sync via VDDK → local raw file → qcow2 → upload → bootable VM

## Problem

The current iSCSI transport reads thin-provisioned flat VMDKs via HTTP from the ESXi datastore. This is broken — ESXi HTTP does not correctly map thin VMDK grain tables to virtual offsets, producing zeroed ext4 partitions. The stream transport (NFC) works but is sequential-only with no delta support for incremental syncs.

Every production VMware migration tool (Veeam, migratekit, os-migrate, Red Hat MTV) uses VDDK for disk reads. VDDK correctly handles thin provisioning, snapshot chains, and CBT-based delta reads.

## Decisions

| Decision | Choice |
|----------|--------|
| Migration host | Linux VM inside Nutanix cluster (datamigrate-rhel7) |
| T1 write target | Local raw file (repository), not iSCSI VG |
| VDDK integration | nbdkit as subprocess (no cgo) |
| T0 and T1 reader | Both via nbdkit+VDDK |
| First milestone | T0 only — VDDK full sync producing bootable VM |
| VDDK dependency | Auto-download + install on first run |

## Architecture

```
Migration Host (Linux)
┌─────────────────────────────────────────────────────┐
│                                                     │
│  datamigrate CLI                                    │
│  ┌───────────────┐     ┌──────────────────────┐    │
│  │ VDDKReader     │────▶│ nbdkit process       │    │
│  │ (Go, NBD       │     │ (vddk plugin)        │    │
│  │  client)       │     │                      │    │
│  │                │     │ Reads VMware disk via │    │
│  │ Implements     │     │ VDDK NBDSSL transport │    │
│  │ BlockReader    │◀────│                      │    │
│  └───────┬───────┘     └──────────┬───────────┘    │
│          │                        │                  │
│          ▼                        │ NBDSSL           │
│  ┌───────────────┐               │                  │
│  │ Pipeline       │               ▼                  │
│  │ (read→write)   │     ┌──────────────────────┐    │
│  └───────┬───────┘     │ VMware ESXi          │    │
│          │              │ (vCenter managed)     │    │
│          ▼              └──────────────────────┘    │
│  ┌───────────────┐                                  │
│  │ RawFileWriter  │                                  │
│  │ (WriteAt to    │                                  │
│  │  disk-0.raw)   │                                  │
│  └───────┬───────┘                                  │
│          │                                           │
│          ▼                                           │
│  ┌───────────────┐     ┌──────────────────────┐    │
│  │ qemu-img       │────▶│ Nutanix PC v3 API   │    │
│  │ raw → qcow2    │     │ Upload image         │    │
│  └───────────────┘     │ Create VM             │    │
│                         └──────────────────────┘    │
└─────────────────────────────────────────────────────┘
```

## Data Flow (T0)

1. User runs: `./datamigrate migrate start --plan <plan.yaml> --transport vddk`
2. datamigrate checks VDDK + nbdkit dependencies, auto-installs if missing
3. Creates snapshot on source VM (for CBT baseline)
4. Spawns nbdkit subprocess with vddk plugin, pointing at snapshot
5. VDDKReader connects to nbdkit via Unix socket (NBD protocol)
6. Pipeline reads 64 MB chunks sequentially from VDDKReader
7. RawFileWriter writes each chunk to `disk-0.raw` via `WriteAt()`
8. After pipeline completes: verify raw file (`file -s`, `fdisk -l`)
9. Convert: `qemu-img convert -f raw -O qcow2 -c disk-0.raw disk-0.qcow2`
10. Upload qcow2 to Nutanix as image
11. Create VM from image
12. Kill nbdkit process, cleanup socket + temp files

## Components

### 1. nbdkit Process Manager (`internal/vddk/nbdkit.go`)

Manages the nbdkit subprocess lifecycle.

```go
type NBDKit struct {
    cmd       *exec.Cmd
    socket    string   // /tmp/datamigrate-nbd-<id>.sock
    passFile  string   // temp file for vCenter password
}

func StartNBDKit(config NBDKitConfig) (*NBDKit, error)
func (n *NBDKit) WaitReady(timeout time.Duration) error  // verify socket + handshake
func (n *NBDKit) Stop() error                             // kill process, cleanup
```

**nbdkit command:**
```bash
nbdkit --readonly --foreground \
  --unix /tmp/datamigrate-nbd-<migration-id>.sock \
  vddk \
  server=<vcenter-host> \
  user=<username> \
  password=+/tmp/datamigrate-vddk-pass \
  thumbprint=<ssl-thumbprint> \
  vm=moref=<vm-moref> \
  snapshot=<snapshot-moref> \
  file="[<datastore>] <path>.vmdk" \
  transports=nbdssl \
  libdir=/usr/lib/vmware-vix-disklib
```

- Unix socket for local communication (no TCP port conflicts)
- `--foreground` so we control the lifecycle
- `--readonly` for safety
- Password via temp file, not command line (avoids `ps` leaking creds)
- `transports=nbdssl` for encrypted VMware network transport

**Error handling:**
- nbdkit crash → detect via process exit, retry once with fresh process
- Socket timeout → configurable (default 120s per read)
- VDDK auth failure → surface vCenter credential error to user

### 2. VDDKReader (`internal/vddk/reader.go`)

Implements `BlockReader` interface. Reads from nbdkit via NBD protocol over Unix socket.

```go
type VDDKReader struct {
    nbdkit    *NBDKit
    conn      net.Conn     // Unix socket to nbdkit
    capacity  int64        // disk size from NBD handshake
    totalRead int64        // progress tracking
}

func NewVDDKReader(nbdkit *NBDKit) (*VDDKReader, error)
func (r *VDDKReader) ReadBlocks(ctx, extents) (<-chan BlockData, <-chan error)
func (r *VDDKReader) Close() error
func (r *VDDKReader) Capacity() int64
```

**NBD protocol operations needed:**
- `NBD_OPT_EXPORT_NAME` — handshake, get disk size
- `NBD_CMD_READ` — read N bytes at offset (random access)
- `NBD_CMD_DISC` — disconnect

For T0: sequential 64 MB reads from offset 0 to capacity.
For T1 (future): random-access reads at CBT-reported offsets only.

### 3. RawFileWriter (`internal/blockio/rawfile.go`)

Implements `BlockWriter` interface. Writes blocks to a local sparse raw disk image.

```go
type RawFileWriter struct {
    file    *os.File
    path    string
    written int64
}

func NewRawFileWriter(path string, capacity int64) (*RawFileWriter, error)
func (w *RawFileWriter) WriteBlock(ctx, block) error  // file.WriteAt(block.Data, block.Offset)
func (w *RawFileWriter) Finalize() error               // file.Sync()
```

The raw file is created as sparse: `file.Truncate(capacity)` sets the size without allocating blocks. Only written regions consume disk space.

### 4. Auto-Installer (`internal/vddk/install.go`)

Checks and installs VDDK + nbdkit on first run.

```go
func EnsureDependencies() error
func CheckNBDKit() bool          // nbdkit --version
func CheckVDDK() bool            // libvixDiskLib.so exists
func InstallNBDKit() error       // yum install nbdkit nbdkit-vddk-plugin
func InstallVDDK() error         // download + extract to /usr/lib/vmware-vix-disklib
```

**Detection:**
- `nbdkit --version` — if not found, install via `yum install nbdkit nbdkit-vddk-plugin`
- `/usr/lib/vmware-vix-disklib/lib64/libvixDiskLib.so` — if not found, download VDDK

**VDDK download:**
- VDDK 8.0.3, ~124 MB from Broadcom developer portal
- Prompt user to accept license before download
- Configurable via `VDDK_DOWNLOAD_URL` env var or plan YAML `vddk_url` field
- Fallback: print manual install instructions and exit

### 5. Verification + Upload (reuse existing code)

After the raw file is written:

```go
// Verify data integrity
exec.Command("file", "-s", rawPath)   // expect "ext4 filesystem" etc.
exec.Command("fdisk", "-l", rawPath)  // expect GPT + partitions

// Convert and upload (reuse existing nutanix package)
exec.Command("qemu-img", "convert", "-f", "raw", "-O", "qcow2", "-c", rawPath, qcow2Path)
nxClient.CreateImage(ctx, imageName, qcow2Size)
nxClient.UploadImage(ctx, imageUUID, qcow2Path)
```

## Integration with Existing Code

**Minimal changes to existing code:**

- `internal/migration/full_sync.go` — add `case state.TransportVDDK:` block alongside existing stream/iscsi cases
- `internal/state/migration.go` — add `TransportVDDK = "vddk"` constant
- `internal/cli/plan.go` — accept `--transport vddk`
- `internal/config/mapping.go` — add vddk config fields (thumbprint, vddk_url)

**No changes to:**
- Pipeline (`blockio/pipeline.go`) — VDDKReader implements BlockReader
- Nutanix client — reuse existing image upload + VM creation
- VMware client — reuse existing snapshot creation + CBT query
- State management — reuse existing BoltDB state

## SSL Thumbprint

The nbdkit vddk plugin requires the ESXi host's SSL thumbprint for NBDSSL transport.

**How to obtain:**
- Auto-discover from the existing govmomi TLS connection during `validate` or `plan create`
- Store in `SourceConfig.Thumbprint` field in the plan YAML
- Fallback: user provides via `--thumbprint` flag or `VCENTER_THUMBPRINT` env var

```yaml
# In plan YAML
source:
  vcenter: pcc-147-135-35-91.ovh.us
  username: admin@pcc-147-135-35-91.ovh.us
  thumbprint: "AA:BB:CC:DD:..."  # auto-populated by plan create
```

**Error handling:** If thumbprint is wrong, nbdkit fails immediately with a clear TLS error. We surface this as "vCenter SSL thumbprint mismatch — re-run plan create to refresh."

## Multi-Disk Handling

VMs can have multiple disks. Each disk gets its own nbdkit process, raw file, and qcow2.

```
For a VM with 2 disks:

  Disk 0 (50 GB):  nbdkit-<id>-disk0.sock → disk-0.raw → disk-0.qcow2 → Image 0
  Disk 1 (100 GB): nbdkit-<id>-disk1.sock → disk-1.raw → disk-1.qcow2 → Image 1
```

Disks are processed sequentially (one nbdkit at a time) to limit resource usage. Each nbdkit process is started, used, and stopped before moving to the next disk.

## Repository Directory Layout

```
/tmp/datamigrate/<migration-name>/
  disk-0.raw          # Raw disk image (sparse, ~disk capacity)
  disk-0.qcow2        # Compressed for upload (~10-15% of raw)
  disk-1.raw          # Second disk (if VM has multiple)
  disk-1.qcow2
  metadata.json        # Disk geometry, changeId, sync history
```

Note: `disk-N.raw` can be deleted after `disk-N.qcow2` conversion completes to reduce peak disk usage.

## NBD Client Implementation

Use a minimal hand-rolled NBD client (not a library). The protocol subset we need is tiny:

1. Newstyle handshake: read `NBDMAGIC` + `IHAVEOPT`, send `NBD_OPT_EXPORT_NAME`
2. `NBD_CMD_READ` — read N bytes at offset
3. `NBD_CMD_DISC` — disconnect

nbdkit uses newstyle negotiation by default. The client must handle the `NBDMAGIC` → `IHAVEOPT` → option haggling phase before sending `NBD_OPT_EXPORT_NAME`. Structured replies (`NBD_OPT_STRUCTURED_REPLY`) are NOT needed — decline if offered.

A hand-rolled client is ~200 lines of Go. No external dependency needed for 3 operations.

## Failure Cleanup

| Failure | Cleanup Action |
|---------|---------------|
| Pipeline fails mid-copy | Kill nbdkit, remove partial raw file, remove socket + pass file |
| User hits Ctrl+C | Signal handler kills nbdkit, cleanup temp files |
| nbdkit dies mid-read | Detect via process exit, retry once with fresh nbdkit. If retry fails, cleanup and report error |
| qcow2 conversion fails | Keep raw file (user can retry conversion manually), report error |
| Upload fails | Keep qcow2 (user can retry upload), report error |

Use `defer` in Go to ensure nbdkit process and temp files are always cleaned up.

## Future: T1 Incremental (Not in Milestone 1)

The same nbdkit+VDDK infrastructure supports T1 with minimal additions:

1. CBT query gives changed extents (existing code)
2. VDDKReader reads ONLY those extents via `NBD_CMD_READ` at specific offsets (random access)
3. RawFileWriter patches the raw file at exact offsets
4. Convert + upload updated qcow2

Network transfer = delta only. This is the key advantage over stream transport.

## Testing

- Unit tests: mock NBD server for VDDKReader
- Integration test: full T0 on migration-host against ubuntu10
- Verification: mount raw file, check `file -s`, compare with source
- Preflight check: `nc -zv <esxi-host> 902` to verify NBDSSL port access

## Risks

| Risk | Likelihood | Mitigation |
|------|-----------|------------|
| VDDK license restrictions | Medium | Document Broadcom licensing; VDDK is free for backup use |
| nbdkit not available on RHEL7 | Low | Install from EPEL or build from source |
| NBD protocol implementation bugs | Low | Hand-rolled client is ~200 lines, simple to debug |
| VDDK version incompatibility | Low | Test with VDDK 7.0 and 8.0; document tested version matrix |
| Disk space on migration host | Medium | Check before T0; warn if < 2x disk size; delete raw after qcow2 conversion |
| ESXi port 902 blocked | Medium | Preflight connectivity check; document NBDSSL requires direct ESXi access on port 902 |
| nbdkit-vddk-plugin ABI mismatch | Medium | Pin tested nbdkit + VDDK versions; build nbdkit from source if needed |

## Package Location

New code goes in `internal/vddk/` (not `internal/transport/vddk/`). The existing `internal/transport/` package defines transport mode enums and stubs — it doesn't contain reader/writer implementations. The readers live in `internal/vmware/` and writers in `internal/blockio/`. Following this pattern, `internal/vddk/` is the right home for VDDK-specific code (nbdkit management + NBD reader).
