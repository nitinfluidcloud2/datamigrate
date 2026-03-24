# Design: Local Repository Approach for VMware-to-Nutanix Migration

**Status:** Proposed
**Date:** 2026-03-24

## Problem Statement

The current iSCSI transport has a critical data correctness bug. The `RawDiskReader`
reads the flat VMDK file directly from the VMware datastore via HTTPS. This flat VMDK
does **not** contain the full disk data in several common scenarios:

- **Thin-provisioned disks** — unwritten blocks are not present in the flat file
- **Snapshot chains** — the flat VMDK is the base; delta VMDKs in the chain are separate files
- **Changed Block Tracking deltas** — reading from the flat file misses blocks that live in snapshot delta files

NFC export handles all of these correctly (VMware reassembles the full disk image
through its snapshot chain), but our NFC code path currently only works with the
`stream` transport, which produces a streamOptimized VMDK file — not a raw image
suitable for random-access patching.

The iSCSI transport's _write_ side (sending blocks directly to a Nutanix Volume Group)
is also fragile: hard to inspect, hard to retry, and dependent on iSCSI connectivity.

### Root Cause Investigation (2026-03-23/24)

Extensive debugging on ubuntu10 (50 GB, Ubuntu 24.04, UEFI, thin-provisioned) revealed:

**Symptoms:**
- iSCSI T0 reports 50 GB read + 50 GB written, no errors
- VG has valid GPT at LBA 0, valid FAT32 on gpt1 (0-1GB)
- gpt2 and gpt3 (ext4, 1GB-50GB) = ALL ZEROS (`file -s` shows "data" not ext4)
- Stream transport (NFC) produces a bootable VM with identical source data

**Eliminated causes:**
- iSCSI write bugs (ITT corruption, NOP-In handling) — no NOP-Ins observed during transfer
- Snapshot chain / incomplete consolidation — `consolidationNeeded: false`, no delta files
- Sector size mismatch, LBA overflow — all within safe ranges

**Confirmed root cause: Thin provisioning + ESXi HTTP serving**

```
Datastore evidence:
  govc device.info:     File: [ssd-002032] ubuntu10/ubuntu10.vmdk
  govc datastore.ls:    ubuntu10.vmdk = 6.6 GB (allocated)
  Virtual size:         50 GB
  HTTP download:        52.58 GB (zero-fills unallocated regions)
```

The disk is thin-provisioned with only 6.6 GB allocated on VMFS. The ESXi HTTP
server at `/folder/disk-flat.vmdk` serves the full virtual size but does **not
correctly map thin VMDK grain tables to virtual offsets**. Allocated blocks
(including the ext4 superblock) are served as zeros at their virtual offset
positions, even though the data exists in the 6.6 GB of allocated grains.

**NFC `ExportSnapshot` works because** it goes through VMware's internal virtual
disk layer which correctly resolves thin VMDK grain mapping + snapshot chains.
The HTTP `/folder/` endpoint serves the raw VMFS file without this resolution.

**This is not fixable in our code** — it's a limitation of the ESXi HTTP datastore
access for thin-provisioned VMDKs. The flat VMDK HTTP approach is fundamentally
unreliable for thin disks.

### Impact

- `RawDiskReader` (used by iSCSI T0 when CBT fails): **BROKEN for thin disks**
- `RangeReader` (used by iSCSI T1 for delta reads): **BROKEN for thin disks**
- `DiskReader` via NFC (used by stream transport): **WORKS correctly**

This affects ALL thin-provisioned VMDKs, which is the default for most modern
VMware deployments. Thick eager-zeroed VMDKs may work correctly via HTTP but
this is not the common case.

## Proposed Solution: Local Block Repository

Maintain a **local raw disk image file** on the migration host that serves as the
single source of truth for the migrated disk. All reads come from VMware via NFC
(which correctly handles snapshot chains). All writes go to a local file (simple,
inspectable, retryable). Upload to Nutanix happens as a separate step after
verification.

This is the same model used by Veeam Backup & Replication.

## Architecture Overview

```
  VMware vSphere                    Migration Host                   Nutanix AHV
 +-----------------+          +-------------------------+       +------------------+
 |                 |   NFC    |                         |  v3   |                  |
 |  VM + Snapshots |--------->|  Local Repository       |------>|  Prism Central   |
 |  (source of     |          |  /data/repo/<vm>/       |  API  |                  |
 |   truth)        |          |    disk-0.raw  (50 GB)  |       |  Image Service   |
 |                 |          |    disk-0.qcow2 (6 GB)  |       |  VM Management   |
 +-----------------+          +-------------------------+       +------------------+
```

## Workflows

### T0: Full Initial Sync

```
 VMware                     Migration Host                       Nutanix
 ------                     --------------                       -------

 1. Create snapshot
        |
        v
 2. NFC ExportSnapshot
    (streamOptimized VMDK)
        |                   3. Save to temp file
        |                      disk-0.vmdk
        |                          |
        |                   4. qemu-img convert
        |                      vmdk -> raw
        |                      disk-0.raw (repository)
        |                          |
        |                   5. qemu-img convert
        |                      raw -> qcow2
        |                      disk-0.qcow2
        |                          |
        |                   6. Verify locally           7. Upload qcow2 as
        |                      - file -s disk-0.raw        Nutanix Image (v3)
        |                      - fdisk -l disk-0.raw           |
        |                                               8. (Optional) Create
        |                                                  test VM, verify boot
        |
 9. Keep snapshot for
    CBT baseline
```

**Step details:**

| Step | Command / API | Notes |
|------|--------------|-------|
| 1 | `CreateSnapshot_Task` with `quiesce=true` | Snapshot name: `datamigrate-t0-<timestamp>` |
| 2 | NFC `ExportSnapshot` | Returns streamOptimized VMDK stream |
| 3 | Write stream to `disk-0.vmdk` | Same as current stream transport |
| 4 | `qemu-img convert -f vmdk -O raw disk-0.vmdk disk-0.raw` | Creates the repository file |
| 5 | `qemu-img convert -f raw -O qcow2 -c disk-0.raw disk-0.qcow2` | `-c` for compression |
| 6 | `file -s disk-0.raw`, `fdisk -l disk-0.raw` | Sanity check partition table |
| 7 | `POST /images` with qcow2 body | Prism Central v3 image API |
| 8 | Create VM referencing image UUID | Optional — for early validation |
| 9 | Keep snapshot, record its `changeId` | Needed as CBT baseline for T1 |

**Disk space usage during T0:**

```
disk-0.vmdk   ~6 GB   (streamOptimized, compressed)
disk-0.raw    50 GB   (full disk, sparse on filesystem)
disk-0.qcow2  ~6 GB   (compressed for upload)
              ------
Total:        ~62 GB peak (vmdk can be deleted after raw conversion)
After cleanup: ~56 GB  (raw + qcow2)
```

### T1..TN: Incremental Sync

```
 VMware                     Migration Host                       Nutanix
 ------                     --------------                       -------

 1. Create new snapshot
        |
 2. CBT QueryChangedDiskAreas
    (prev snapshot -> new)
        |
        v
    Changed extents:
    [offset=0x1000, len=0x2000]
    [offset=0xA000, len=0x1000]
        |
 3. For each extent:
    Read bytes via NFC or
    HTTP Range from snapshot
        |                   4. Patch repository:
        |                      file.WriteAt(data, offset)
        |                      for each changed extent
        |                          |
        |                   5. qemu-img convert
        |                      raw -> qcow2
        |                          |
        |                   6. Upload qcow2 as
        |                      new Nutanix image
        |                          |
        |                   7. (Optional) Recreate VM
        |                      from latest image
        |
 8. Delete old snapshot,
    keep new one for
    next CBT baseline
```

**Step details for the patch operation (step 4):**

```go
// Pseudocode for patching changed blocks into the repository
repo, _ := os.OpenFile("disk-0.raw", os.O_RDWR, 0644)
defer repo.Close()

for _, extent := range changedExtents {
    data := readFromNFC(snapshot, disk, extent.Offset, extent.Length)
    _, err := repo.WriteAt(data, extent.Offset)
    // ... handle error
}
```

This is the critical difference from the current stream transport, which re-downloads
the entire disk on every sync. Here we only read and write the changed blocks.

**How T1 patching works (the key concept):**

The raw repository file is a living document. Each sync patches it in-place:

```
After T0:  disk-0.raw = complete disk (all blocks from NFC export)
After T1:  disk-0.raw = T0 + T1 changed blocks overwritten at exact offsets
After T2:  disk-0.raw = T0 + T1 + T2 merged in-place
After TN:  disk-0.raw = always a complete, bootable disk image
```

At any point, you can convert `disk-0.raw` → qcow2 → upload → create VM.
There are no separate delta files to chain or merge.

**Reading changed extents — four strategies:**

| Strategy | Method | Reads | Correct? | Performance |
|----------|--------|-------|----------|-------------|
| **A** | Full NFC re-read + extract | Entire disk | Yes | Bad — 300 GB for 100 MB delta |
| **B** | HTTP Range (flat VMDK) | Only deltas | **No** — same snapshot bug | Good for simple cases |
| **C** | VDDK random read | Only deltas | Yes | Best — but needs C bindings |
| **D (recommended)** | NFC lease + selective read | Only deltas | Yes | Good — reads only what changed |

**Strategy D: NFC Lease + Selective Read (recommended)**

The NFC `ExportSnapshot` lease gives access to the full disk as a sequential
stream. But we don't need to download ALL of it. The approach:

1. CBT gives us changed extents: `[(offset=1GB, len=64KB), (offset=5GB, len=1MB), ...]`
2. Open NFC lease for the snapshot (same as T0)
3. Read the NFC stream sequentially, but **only keep data that falls within changed extents**
4. Discard all other data (skip over it)
5. Write kept data to `disk-0.raw` at exact offsets via `WriteAt()`

```go
// Pseudocode for Strategy D
nfcStream := openNFCExport(snapshot, disk)
changedMap := buildExtentMap(cbtExtents)  // quick lookup: is offset in a changed region?

offset := int64(0)
for {
    chunk := readFromNFC(nfcStream, 64*1024*1024)  // 64 MB at a time
    if chunk == nil { break }

    // Check which parts of this chunk overlap with changed extents
    for _, overlap := range changedMap.Overlapping(offset, len(chunk)) {
        // Extract just the changed bytes and patch the repository
        data := chunk[overlap.Start-offset : overlap.End-offset]
        repo.WriteAt(data, overlap.Start)
    }
    offset += int64(len(chunk))
}
```

This reads the full NFC stream but only writes changed blocks to the repository.
The network cost is still the full disk size, but:
- It is **correct** (NFC handles snapshot chains)
- The disk I/O is minimal (only changed blocks written)
- No VDDK dependency

**Future optimization:** If NFC sequential read is too slow for large disks with
tiny deltas, implement Strategy C (VDDK) for true random access. Or investigate
whether govmomi's NFC lease supports seeking (HTTP Range on the NFC URL).

**Alternative for known-safe scenarios (no snapshot chain):**

If we can guarantee there is **exactly one snapshot** (ours) and no pre-existing
chain, then the flat VMDK accurately represents the pre-snapshot state, and HTTP
Range reads (Strategy B) are safe for the delta. This can be validated by checking
the snapshot tree before reading. Use Strategy B with a safety check:

```go
// Only use HTTP Range if snapshot chain is simple (our snapshot only)
if len(snapshotTree) == 1 && snapshotTree[0].Name == ourSnapshotName {
    // Safe to use HTTP Range reads from flat VMDK
    useStrategyB()
} else {
    // Fall back to NFC sequential read (Strategy D)
    useStrategyD()
}
```

### Cutover

```
 VMware                     Migration Host                       Nutanix
 ------                     --------------                       -------

 1. Final incremental sync
    (T_N — same as above,
     minimal delta)
        |
 2. Power off source VM -----> Downtime starts
        |
 3. (Optional) One more
    CBT pass after poweroff
    to catch final writes
        |                   4. Final patch + convert
        |                          |
        |                   5. Upload final qcow2
        |                          |
        |                   6. Create production VM
        |                      with correct:
        |                      - vCPU, RAM
        |                      - NIC -> subnet mapping
        |                      - Boot config (UEFI/BIOS)
        |                          |
        |                   7. Power on target VM
        |                      Downtime ends
        |
 8. Validate:
    - VM boots
    - Network reachable
    - Application health
        |
 9. Cleanup:
    - Delete VMware snapshots
    - Delete local repository
    - (Optional) Delete old
      Nutanix images
```

**Downtime window** = time for steps 2-7. With a small final delta:

- CBT query + read delta: ~30 seconds
- Patch repository: ~5 seconds
- Convert raw -> qcow2: ~2-3 minutes (50 GB disk)
- Upload qcow2: ~2-5 minutes (6 GB over 1 Gbps)
- Create VM + power on: ~30 seconds

**Estimated downtime: 5-10 minutes** for a 50 GB disk with small final delta.

## Repository Directory Layout

```
/data/datamigrate/
  repo/
    <migration-id>/
      disk-0.raw          # The repository — raw disk image
      disk-0.qcow2        # Latest converted image for upload
      disk-0.vmdk         # Temp: NFC export (deleted after T0 conversion)
      disk-1.raw          # Second disk (if VM has multiple)
      disk-1.qcow2
      metadata.json        # Disk geometry, last snapshot changeId, etc.
```

**metadata.json:**

```json
{
  "vm_name": "rhel6-app-server",
  "vm_moid": "vm-1234",
  "disks": [
    {
      "key": 2000,
      "label": "Hard disk 1",
      "capacity_bytes": 53687091200,
      "repository_file": "disk-0.raw",
      "last_snapshot": "snapshot-5678",
      "last_change_id": "52 de 5a 12 ... 00 00 00 02",
      "syncs": [
        {"type": "full", "timestamp": "2026-03-24T10:00:00Z", "bytes_written": 53687091200},
        {"type": "incremental", "timestamp": "2026-03-24T14:00:00Z", "bytes_written": 104857600}
      ]
    }
  ]
}
```

## Comparison with Existing Transports

```
                    stream          iscsi             repository (new)
                    ------          -----             ----------------
T0 read method      NFC (correct)   Datastore HTTP    NFC (correct)
                                    (BROKEN for
                                     snapshots)

T0 write target     Local VMDK      Nutanix VG        Local raw file
                    + qcow2 upload  (direct iSCSI)    + qcow2 upload

T1 read method      NFC full disk   Datastore HTTP    NFC (changed
                    (re-downloads   (reads flat VMDK   extents only)
                     everything)     — BROKEN)

T1 write method     Full re-upload  WriteAt to VG     WriteAt to local
                                    via iSCSI         file, then upload

Inspectable?        Yes (file)      No (VG opaque)    Yes (file)

Retry on failure?   Restart full    Complex (iSCSI    Re-read extent,
                    export          reconnect)        re-patch file

Platform needs      qemu-img        iSCSI initiator   qemu-img
                                    (or pure-Go)

Disk space needed   ~1x disk size   None (remote)     ~1x disk size
on migration host

Data correctness    CORRECT         BROKEN            CORRECT
```

## Advantages

1. **Correctness** — NFC reads go through VMware's snapshot chain assembly. No flat
   VMDK bugs. This is the same data path VMware's own export uses.

2. **Inspectable** — The raw repository file is a standard disk image. Debug with
   standard tools:
   ```
   file -s disk-0.raw
   fdisk -l disk-0.raw
   losetup /dev/loop0 disk-0.raw && mount /dev/loop0p1 /mnt
   ```

3. **Simple I/O** — `os.File.WriteAt()` for patching. No iSCSI protocol, no SCSI
   commands, no connection management.

4. **Portable** — Works on Mac (dev), Linux (production), Docker. No kernel modules,
   no iSCSI initiator packages.

5. **Verifiable** — Convert to qcow2 and boot in QEMU locally before uploading to
   Nutanix. Catch boot issues early.

6. **Retryable** — If upload fails, retry. If a patch fails, re-read the extent and
   retry. The repository file is persistent.

7. **Proven model** — This is how Veeam, Zerto, and most backup products work.

## Trade-offs

1. **Local disk space** — Need ~1x the VM disk size on the migration host. A 300 GB
   VM needs 300 GB of local storage (raw file is sparse if the filesystem supports it,
   so actual usage may be less).

2. **Full qcow2 upload each sync** — Each T1..TN requires converting the full raw to
   qcow2 and re-uploading. A 300 GB disk might produce a 30-60 GB qcow2. On a 1 Gbps
   link, that is 4-8 minutes per upload. Compare to iSCSI which would only send the
   changed bytes (could be 100 MB).

3. **Conversion time** — `qemu-img convert` for a 300 GB raw -> qcow2 takes 3-5
   minutes depending on I/O speed. This adds to every sync cycle.

4. **Not suitable for continuous replication** — The upload overhead makes sub-minute
   RPO impractical. This is a periodic sync model (hourly or daily).

## Implementation Plan

### Phase 1: T0 via Repository (minimal changes)

The current `stream` transport already does most of T0. Refactor to:

1. Extract the NFC export + VMDK save logic into a `repository` package
2. Add raw conversion step after VMDK download
3. Store metadata.json alongside the raw file
4. Keep the existing qcow2 upload + VM creation logic

**Files to modify:**
- `cmd/migrate.go` — add `--transport repository` option
- New package: `internal/repository/` — repository management
- `internal/repository/t0.go` — full sync logic
- `internal/repository/convert.go` — qemu-img wrapper
- `internal/repository/metadata.go` — metadata read/write

### Phase 2: T1 Incremental Patching — Delta-Only Read via NFC

The critical requirement: **only transfer changed blocks at T1, not the whole VM.**

CBT tells us WHAT changed. The question is HOW to read just those bytes.

**NFC approach for delta-only reads:**

govmomi's NFC `ExportSnapshot` returns a sequential streamOptimized VMDK stream.
This is NOT random-access — you can't seek to offset 5 GB and read 1 MB. But the
streamOptimized VMDK format has a key property: it contains **grain tables** that
map virtual offsets to positions in the stream. The grains that correspond to
unallocated/unchanged regions are stored as zero markers (tiny metadata), not
full data blocks.

**Practical approach for delta reads:**

```
Step 1: CBT query → changed extents list
        [(offset=1GB, len=64KB), (offset=5GB, len=1MB), ...]

Step 2: NFC ExportSnapshot → streamOptimized VMDK stream
        Read the stream, parse VMDK grain table headers
        For each grain (typically 64KB):
          - If grain offset falls within a changed extent → save the data
          - If grain offset is NOT in a changed extent → skip/discard
        This is a single sequential pass over the NFC stream.

Step 3: Patch repository
        For each saved grain:
          repo.WriteAt(grainData, grainOffset)

Step 4: Convert raw → qcow2, upload, create VM
```

**Why this works efficiently:**

The NFC stream transfers only ALLOCATED grains. For a 50 GB disk with 6 GB of
actual data, the NFC stream is ~6 GB (compressed). We read the full 6 GB stream
but only write the changed grains to the repository. For a typical T1 with 100 MB
of changes, we read 6 GB over the network but only patch 100 MB to disk.

**Even better: govmomi HTTP download with Range headers on NFC URL**

The NFC lease provides an HTTPS URL for the disk. We may be able to use HTTP Range
requests on this URL to read specific byte ranges, getting true random access
through the snapshot chain. This needs investigation — if it works, T1 would
transfer ONLY the delta bytes:

```go
// Potential approach — needs testing
lease := nfc.ExportSnapshot(snapshot)
diskURL := lease.URLs[diskKey]  // HTTPS URL for the disk

for _, extent := range changedExtents {
    req, _ := http.NewRequest("GET", diskURL, nil)
    req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", extent.Offset, extent.Offset+extent.Length-1))
    resp, _ := http.Do(req)
    data, _ := io.ReadAll(resp.Body)
    repo.WriteAt(data, extent.Offset)
}
```

If NFC URLs support Range headers, this is the optimal solution — delta-only
network transfer + delta-only disk write. Same correctness as NFC (handles
snapshot chains) with the efficiency of HTTP Range requests.

**TODO: Investigate whether govmomi NFC lease URLs support HTTP Range requests.**

**Implementation steps:**

1. CBT query for changed extents (already exists in `vmware` package)
2. Test NFC URL Range support — if it works, implement random-access delta reader
3. Fallback: sequential NFC stream + grain filter (read all, save only changed)
4. Implement `WriteAt()` patcher for the raw repository file
5. Wire up: CBT query → read deltas → patch → convert → upload

**Files to add:**
- `internal/repository/incremental.go` — CBT + patch + convert + upload
- `internal/repository/nfc_delta_reader.go` — read changed extents via NFC

### Phase 3: Cutover Workflow

1. Wire up the final sync + power-off + create-VM sequence
2. Add validation step (upload, create VM, check power-on)

### Phase 4: Cleanup and Migration Path

1. Deprecate `stream` and `iscsi` transports (or keep as legacy)
2. Make `repository` the default transport
3. Clean up old transport code

## Key Implementation Details

### Sparse File Handling

The raw repository file should be created as a sparse file to save disk space:

```go
// Create a sparse file of the target disk size
f, _ := os.Create("disk-0.raw")
f.Truncate(diskSizeBytes) // Sets size without allocating blocks
// Actual disk usage: 0 bytes until data is written
```

After T0, only the written blocks consume disk space. For a thin-provisioned
50 GB VMware disk with 15 GB of actual data, the raw file will consume ~15 GB
on disk (with filesystem sparse file support).

### NFC Extent Reading for T1

The NFC export gives a sequential stream (streamOptimized VMDK). For T1, we need
only the changed extents. Two strategies:

**Strategy A: Full NFC re-read + extract (simple, correct)**
```
NFC ExportSnapshot -> temp VMDK -> convert to raw -> extract changed extents -> patch
```
This re-reads the full disk but is guaranteed correct. Wasteful for small deltas
but simple to implement. Good enough for Phase 2.

**Strategy B: Datastore HTTP Range reads (efficient, limited)**
```
For each changed extent:
  HTTP GET /folder/<vm>/<disk>-flat.vmdk  Range: bytes=offset-offset+length
  -> patch into repository
```
This only reads changed bytes but reads from the flat VMDK, which has the same
snapshot chain issue. Only safe when there is **no active snapshot chain** (i.e.,
we delete the old snapshot before reading).

**Strategy C: VDDK random read (ideal, complex)**
```
VDDK VixDiskLib_Read(disk, sector, numSectors, buf)
```
True random access through the snapshot chain. Requires VDDK binaries and cgo.
Could be a future optimization.

**Recommendation:** Start with Strategy A. Optimize to B or C later if T1
performance is unacceptable.

### qemu-img Integration

```go
func ConvertRawToQcow2(rawPath, qcow2Path string) error {
    cmd := exec.Command("qemu-img", "convert",
        "-f", "raw",
        "-O", "qcow2",
        "-c",              // compress
        "-o", "cluster_size=2M",  // larger clusters = better compression
        rawPath, qcow2Path,
    )
    cmd.Stderr = os.Stderr
    return cmd.Run()
}

func ConvertVmdkToRaw(vmdkPath, rawPath string) error {
    cmd := exec.Command("qemu-img", "convert",
        "-f", "vmdk",
        "-O", "raw",
        vmdkPath, rawPath,
    )
    cmd.Stderr = os.Stderr
    return cmd.Run()
}
```

Prerequisite: `qemu-img` must be installed on the migration host.
- Mac: `brew install qemu`
- RHEL: `yum install qemu-img`
- Docker: include in Dockerfile

### Upload Optimization: Differential Images (Future)

Instead of re-uploading the full qcow2 each time, a future optimization could:

1. Keep the previous qcow2 on Nutanix as a base image
2. Create a qcow2 with only the changed blocks (qcow2 backing file)
3. Upload only the differential qcow2

This would reduce upload size to only the delta, similar to iSCSI's advantage.
However, Nutanix's image API may not support qcow2 backing files, so this needs
investigation.

## Risk Assessment

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| Local disk space exhaustion | Medium | High | Check space before T0, warn if < 2x disk size |
| qemu-img not installed | Low | High | Check at startup, fail fast with install instructions |
| NFC re-read too slow for T1 | Medium | Medium | Acceptable for Phase 2; optimize with Strategy B/C later |
| Upload fails mid-transfer | Medium | Low | Retry upload; qcow2 file persists locally |
| Sparse file not supported (some filesystems) | Low | Medium | Detect and warn; fallback to full allocation |
| Nutanix image quota exceeded | Low | Medium | Delete old images before uploading new ones |

## Decision Log

| Decision | Rationale |
|----------|-----------|
| Raw file as repository (not qcow2) | Need random-access `WriteAt()` for patching; qcow2 requires qemu-nbd for random writes |
| NFC for all reads | Correctness over performance; flat VMDK reads are fundamentally broken for snapshot chains |
| qcow2 for upload (not raw) | 5-10x compression; 50 GB raw -> 6 GB qcow2 saves upload time |
| Strategy A for T1 initially | Simple, correct; optimize later |
| New `repository` transport | Clean separation; don't break existing transports during development |
