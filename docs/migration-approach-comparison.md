# Migration Approach Comparison

Comparison of four approaches for migrating VMware VMs to Nutanix AHV.

**Test VM:** ubuntu10 — 50 GB disk (6.6 GB allocated), Ubuntu 24.04, UEFI, thin-provisioned

## Approach Summary

| | 1. OVFTool/govc Export | 2. Stream (NFC) | 3. iSCSI (Direct VG) | 4. Repository (Proposed) |
|---|---|---|---|---|
| **Tool** | `govc export.ovf` or `ovftool` | `datamigrate migrate start --transport stream` | `datamigrate migrate start --transport iscsi` | `datamigrate migrate start --transport repository` |
| **Read method** | OVF export (NFC internally) | NFC `ExportSnapshot` | HTTP flat VMDK from datastore | NFC `ExportSnapshot` |
| **Write target** | Local VMDK → qcow2 → upload | Local VMDK → qcow2 → upload | Nutanix Volume Group (iSCSI) | Local raw file → qcow2 → upload |
| **Thin disk safe?** | Yes (NFC resolves grains) | Yes (NFC resolves grains) | **NO** — HTTP serves broken data | Yes (NFC resolves grains) |
| **Snapshot safe?** | Yes (exports from snapshot) | Yes (exports from snapshot) | **NO** — reads stale flat file | Yes (exports from snapshot) |

## T0 Full Sync

| | OVFTool/govc | Stream | iSCSI | Repository |
|---|---|---|---|---|
| **Network transfer** | Full disk (~6 GB compressed) | Full disk (~6 GB compressed) | Full disk (50 GB raw) | Full disk (~6 GB compressed) |
| **Time (50 GB disk, 1 Gbps)** | ~10-15 min (export + convert + upload) | ~10-15 min (export + convert + upload) | ~10 min (direct write) | ~10-15 min (export + convert + upload) |
| **Time (300 GB disk, 1 Gbps)** | ~60-90 min | ~60-90 min | ~60 min | ~60-90 min |
| **Local disk needed** | ~2x disk (VMDK + qcow2) | ~2x disk (VMDK + qcow2) | None (remote VG) | ~2x disk (raw + qcow2) |
| **Output** | Nutanix Image | Nutanix Image | Nutanix Volume Group | Local raw file + Nutanix Image |
| **Data correctness** | Correct | Correct | **BROKEN** (thin disks) | Correct |

## T1..TN Incremental Sync

| | OVFTool/govc | Stream | iSCSI | Repository |
|---|---|---|---|---|
| **CBT support** | No | No (re-exports full disk) | Yes (CBT delta → WriteAt) | Yes (CBT delta → patch raw file) |
| **Network transfer per sync** | Full disk each time | Full disk each time | **Delta only** (e.g., 100 MB) | Delta read + full qcow2 upload |
| **Time (50 GB, 100 MB changed)** | ~10-15 min | ~10-15 min | ~30 sec | ~5-8 min (delta + convert + upload) |
| **Time (300 GB, 5 GB changed)** | ~60-90 min | ~60-90 min | ~5 min | ~15-20 min (delta + convert + upload) |
| **Efficiency** | Terrible (full re-export) | Terrible (full re-export) | Excellent (delta only) | Good (delta read, full upload) |
| **Data correctness** | Correct | Correct | **BROKEN** (thin disks) | Correct |

## Cutover (Downtime Window)

| | OVFTool/govc | Stream | iSCSI | Repository |
|---|---|---|---|---|
| **Final sync** | Full re-export | Full re-export | Delta only | Delta only |
| **Downtime (50 GB disk)** | 15-20 min | 15-20 min | 2-5 min | 5-10 min |
| **Downtime (300 GB disk)** | 60-90 min | 60-90 min | 5-10 min | 15-25 min |
| **Near-zero downtime?** | No | No | Yes (if it worked) | Possible with small deltas |

## Reliability & Operations

| | OVFTool/govc | Stream | iSCSI | Repository |
|---|---|---|---|---|
| **Retry on failure** | Restart full export | Restart full export | Complex (iSCSI reconnect) | Re-read extent, re-patch file |
| **Inspectable?** | Yes (local files) | Yes (local files) | No (VG is opaque) | Yes (local raw file) |
| **Verify before boot** | Mount qcow2, check | Mount qcow2, check | Can't easily inspect VG | Mount raw file, check partitions |
| **Platform requirements** | govc/ovftool + qemu-img | qemu-img | Pure Go iSCSI initiator | qemu-img |
| **Works on Mac?** | Yes | Yes | Yes (pure Go) | Yes |
| **Works in Docker?** | Yes | Yes | Yes | Yes |
| **Automation level** | Manual (CLI commands) | Automated (datamigrate) | Automated (datamigrate) | Automated (datamigrate) |

## Data Correctness

| | OVFTool/govc | Stream | iSCSI | Repository |
|---|---|---|---|---|
| **Thin provisioned disks** | ✅ Correct | ✅ Correct | ❌ Zeros for allocated blocks | ✅ Correct |
| **Snapshot chains** | ✅ Handles chain | ✅ Handles chain | ❌ Reads stale base only | ✅ Handles chain |
| **UEFI boot** | ✅ (with grub-mkstandalone) | ✅ (with grub-mkstandalone) | ❌ (data integrity issue) | ✅ (with grub-mkstandalone) |
| **Legacy BIOS boot** | ✅ Tested (rhel6) | ✅ Tested (rhel6) | ❌ Not reliable | ✅ Expected to work |

## Cost Comparison (10 syncs of 300 GB disk, 5 GB daily delta)

| | OVFTool/govc | Stream | iSCSI | Repository |
|---|---|---|---|---|
| **T0 network** | ~60 GB | ~60 GB | 300 GB | ~60 GB |
| **T1-T9 network** | 9 × 60 GB = 540 GB | 9 × 60 GB = 540 GB | 9 × 5 GB = 45 GB | 9 × (5 GB read + 60 GB upload) = 585 GB |
| **Total network** | **600 GB** | **600 GB** | **345 GB** | **645 GB** |
| **Local disk** | ~120 GB peak | ~120 GB peak | 0 GB | ~360 GB (raw + qcow2) |

## Verdict

| Approach | Best For | Status |
|---|---|---|
| **OVFTool/govc** | One-time manual migrations, small VMs | Works but no incremental |
| **Stream** | POC, testing, small VMs without frequent syncs | ✅ Working, tested |
| **iSCSI** | Would be ideal for large VMs with frequent syncs | ❌ Broken (thin VMDK read bug) |
| **Repository** | Production use — correct reads + incremental patching | 🔧 To be implemented |

## Recommendation

**Short term:** Use **Stream** for migrations. Proven working for both UEFI (ubuntu10) and Legacy BIOS (rhel6-test).

**Long term:** Implement **Repository** approach with NFC reads + local raw patching + qcow2 upload. Add VDDK (cgo) for efficient delta-only reads in the future.

**iSCSI transport** should be deprecated or restricted to thick-provisioned disks only. The fundamental issue (ESXi HTTP server not correctly serving thin VMDK grains) is not fixable in our code.
