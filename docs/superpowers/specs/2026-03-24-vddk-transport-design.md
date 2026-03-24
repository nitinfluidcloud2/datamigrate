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

## External Dependencies

### Required on Migration Host

| Dependency | Version | Purpose | Size | Install Method |
|---|---|---|---|---|
| **nbdkit** | >= 1.30 | NBD server that hosts VDDK plugin | ~2 MB | `yum install nbdkit` (RHEL/CentOS) |
| **nbdkit-vddk-plugin** | matches nbdkit | Plugin that connects nbdkit to VDDK | ~100 KB | `yum install nbdkit-vddk-plugin` |
| **VDDK (VMware Virtual Disk Development Kit)** | 7.0.3 or 8.0.3 | C libraries for reading VMware virtual disks | ~124 MB | Download from Broadcom |
| **qemu-img** | >= 2.0 | Converts raw → qcow2 for Nutanix upload | ~5 MB | `yum install qemu-img` |

### How to Install

**Option A: Auto-install (default)**

datamigrate auto-detects and installs missing dependencies on first run:

```
$ ./datamigrate migrate start --plan configs/ubuntu10-plan.yaml --transport vddk

Checking dependencies...
  nbdkit:              not found → installing via yum...          OK
  nbdkit-vddk-plugin:  not found → installing via yum...          OK
  VDDK libs:           not found → downloading VDDK 8.0.3...      OK (124 MB)
  qemu-img:            found (/usr/bin/qemu-img 2.12.0)           OK

All dependencies ready.
```

Auto-install requires root/sudo on the migration host.

**Option B: Manual install**

```bash
# 1. Install nbdkit + plugin
sudo yum install -y epel-release
sudo yum install -y nbdkit nbdkit-vddk-plugin qemu-img

# 2. Download VDDK from Broadcom developer portal
#    URL: https://developer.broadcom.com/sdks/vmware-virtual-disk-development-kit-vddk/8.0
#    Requires free Broadcom account
#    Download: VMware-vix-disklib-8.0.3-XXXXXXX.x86_64.tar.gz

# 3. Extract VDDK
sudo tar xzf VMware-vix-disklib-*.tar.gz -C /usr/lib/
sudo ln -sf /usr/lib/vmware-vix-disklib-distrib /usr/lib/vmware-vix-disklib

# 4. Set library path
echo '/usr/lib/vmware-vix-disklib/lib64' | sudo tee /etc/ld.so.conf.d/vddk.conf
sudo ldconfig

# 5. Verify
nbdkit --dump-plugin vddk
# Should print: vddk_default_libdir=/usr/lib/vmware-vix-disklib
```

**Option C: Environment variables (custom paths)**

```bash
export VDDK_LIBDIR=/opt/custom/vmware-vix-disklib    # custom VDDK location
export VDDK_DOWNLOAD_URL=https://internal-mirror/vddk-8.0.3.tar.gz  # internal mirror
```

Or in plan YAML:
```yaml
vddk:
  libdir: /opt/custom/vmware-vix-disklib
  download_url: https://internal-mirror/vddk-8.0.3.tar.gz
```

### VDDK Licensing

VDDK is free for backup/migration use. It requires accepting the Broadcom EULA. The auto-installer prompts the user to accept before downloading. For automated/CI environments, set `VDDK_ACCEPT_LICENSE=yes` to skip the prompt.

### Platform Support

| Platform | nbdkit | VDDK | Status |
|---|---|---|---|
| RHEL 7/8/9 | yum/dnf | x86_64 Linux only | Supported |
| Ubuntu 20.04+ | apt install nbdkit | x86_64 Linux only | Supported |
| macOS | Not available | Not available | Use stream transport instead |
| Docker | Include in Dockerfile | Bundle in image | Supported (Linux container) |

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

## Alternative: Pure Go VMDK Parser (VDDK-Free)

### VDDK Licensing Concern

VDDK requires Broadcom's proprietary `libvixDiskLib.so` regardless of integration method:

| Approach | Open Source Part | Broadcom Part (required) |
|---|---|---|
| nbdkit + VDDK plugin | nbdkit (BSD) | `libvixDiskLib.so` from Broadcom |
| cgo + VDDK | Go wrapper code | `libvixDiskLib.so` from Broadcom |

Post-Broadcom acquisition (2024+), VDDK licensing is increasingly restrictive:
- Cannot redistribute VDDK libraries without partner agreement
- Commercial products need a separate distribution agreement
- Developer portal access becoming harder to obtain
- All open-source tools (migratekit, vjailbreak, kubevirt) use BYOVDDK — none bundle it

### VDDK-Free Alternative: Parse StreamOptimized VMDK in Pure Go

Our NFC `ExportSnapshot` already returns correct, complete data for thin disks and
snapshot chains. The stream is in streamOptimized VMDK format (compressed grains
with grain tables). We can parse this in pure Go.

**How it works:**

```
T0:  NFC ExportSnapshot → streamOptimized VMDK stream
         → Parse grain tables (pure Go)
         → Decompress grains (zlib)
         → WriteAt(data, virtualOffset) to disk-0.raw
         → qemu-img convert → qcow2 → upload → create VM

T1:  CBT query (govmomi SOAP API) → changed extents list
     NFC ExportSnapshot → streamOptimized VMDK stream (full disk)
         → Parse grains, KEEP only those in changed extents
         → WriteAt(changedData, offset) to disk-0.raw
         → qemu-img convert → qcow2 → upload → recreate VM
```

Go libraries available for VMDK parsing:
- `github.com/Velocidex/go-vmdk` — reads VMDK files locally
- `github.com/masahiro331/go-vmdk-parser` — parses VMDK format
- `github.com/libyal/libvmdk` — C reference (format documentation)

### Comparison: All Approaches (50 GB disk, 100 MB changed at T1)

| | Stream (today) | VDDK (Broadcom needed) | Pure Go VMDK Parser |
|---|---|---|---|
| **T0 network** | ~6 GB (compressed) | ~50 GB (raw) | ~6 GB (compressed) |
| **T0 disk write** | ~56 GB (vmdk+qcow2) | 50 GB (raw) | 50 GB (raw) + 6 GB (qcow2) |
| **T0 correctness** | ✅ | ✅ | ✅ |
| **Can detect T1 delta?** | ❌ No CBT | ✅ CBT + delta only | ✅ CBT (govmomi API) |
| **T1 network** | ~6 GB (full re-download) | **100 MB** (delta only) | ~6 GB (full re-download) |
| **T1 disk write** | ~56 GB (full rewrite) | **100 MB** (delta only) | **100 MB** (patch changed blocks) |
| **T1 time** | ~10-15 min | **~30 sec** | ~5-8 min |
| **Cutover downtime (50 GB)** | 15-20 min | 2-5 min | 5-10 min |
| **Cutover downtime (300 GB)** | 60-90 min | 5-10 min | 15-25 min |
| **Broadcom dependency** | None | **Yes (VDDK license)** | None |
| **Thin disk safe?** | ✅ | ✅ | ✅ |
| **Works on Mac?** | ✅ | ❌ Linux only | ✅ |
| **Legal risk** | Zero | Broadcom can restrict | Zero |
| **External dependencies** | qemu-img | nbdkit + VDDK + qemu-img | qemu-img |
| **Complexity** | Low (exists) | Medium (nbdkit/cgo) | Medium (VMDK parser) |

### Trade-off Summary

| | VDDK | Pure Go |
|---|---|---|
| **T1 network cost** | Only delta (100 MB) | Full disk compressed (6 GB) |
| **T1 disk I/O** | Only delta (100 MB) | Only delta (100 MB) — same! |
| **Legal risk** | Broadcom dependency | Zero — our code |
| **Long-term viability** | Dependent on Broadcom | Independent, fully portable |
| **Extra T1 time (50 GB)** | Baseline | +1 min (6 GB on 1 Gbps) |
| **Extra T1 time (300 GB)** | Baseline | +5-10 min (30 GB on 1 Gbps) |

### Recommended Strategy

1. **Implement Pure Go VMDK Parser first** (VDDK-free, no licensing risk)
   - Milestone 1: T0 full sync via NFC → parse VMDK → raw file → qcow2 → bootable VM
   - Milestone 2: T1 incremental via CBT + NFC re-read + filter → patch raw → upload
2. **Add VDDK as optional optimization later** (for users who want delta-only T1 reads)
   - BYOVDDK pattern: user supplies VDDK libraries
   - Provides delta-only network transfer at T1 (100 MB vs 6 GB)
   - Not required — pure Go path works without it

This gives us a fully independent, VDDK-free migration tool as the default, with
VDDK as an optional performance boost for users willing to accept the licensing.

## Risks

| Risk | Likelihood | Mitigation |
|------|-----------|------------|
| VMDK parser bugs (grain table edge cases) | Medium | Use existing Go libraries; extensive testing with thin/thick/UEFI/Legacy VMs |
| Disk space on migration host | Medium | Check before T0; warn if < 2x disk size; delete raw after qcow2 conversion |
| NFC re-read too slow for T1 (large disks) | Medium | Acceptable for most workloads; add VDDK as optional optimization for large VMs |
| StreamOptimized VMDK format changes | Low | Format is stable since VMDK spec v3; documented by libvmdk |
| qemu-img not available | Low | Check at startup; provide install instructions |
| VDDK license restrictions (if VDDK optional path used) | Medium | BYOVDDK pattern; document Broadcom licensing; VDDK is optional, not required |

## Package Location

New code goes in `internal/repository/` for the pure Go VMDK parser and repository management.
Optional VDDK code goes in `internal/vddk/` if/when added as an optimization.

```
internal/
  repository/
    reader.go         # StreamOptimized VMDK parser → raw blocks
    writer.go         # RawFileWriter (WriteAt to disk-0.raw)
    convert.go        # qemu-img wrapper (raw → qcow2)
    metadata.go       # Sync state, changeIds, history
    t0.go             # T0 full sync orchestration
    incremental.go    # T1+ CBT + NFC re-read + patch
  vddk/               # Optional, future
    nbdkit.go         # nbdkit process management
    reader.go         # NBD client (VDDKReader)
    install.go        # VDDK auto-installer
```
