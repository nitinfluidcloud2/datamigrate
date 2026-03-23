# Migration Approach Comparison: datamigrate vs Zerto vs Veeam vs Nutanix Move

## How Each Tool Gets Disk Data onto Nutanix AHV

### Nutanix Move
- **Runs as**: VM on the AHV cluster itself
- **Replication**: Uses VMware CBT (same as datamigrate) — snapshot + changed block queries
- **Disk writes**: Writes **directly to vDisks in storage containers** via internal CVM APIs (192.168.5.x backplane)
- **No intermediate step**: vDisks are regular VM disks from the start
- **VM creation**: v2/v3 API with direct vDisk UUID references
- **VG problem**: Does NOT exist — disks are already regular vDisks

### Zerto
- **Runs as**: Zerto Cloud Appliance (ZCA) on the AHV cluster
- **Replication**: Continuous journal-based (NOT CBT). VRA on each ESXi host intercepts I/O in real-time via VAIO filter
- **Disk writes**: ZCA writes to **vDisks in storage containers** via internal cluster access
- **Journal**: Maintains write-ordered log on target for point-in-time recovery
- **VM creation**: v2/v3 API with direct vDisk UUID references
- **VG problem**: Does NOT exist — disks are already regular vDisks

### Veeam
- **Runs as**: Backup Proxy VM on the AHV cluster
- **Replication**: Backup-then-restore model. Backs up VMware VM (CBT-based), then restores to AHV
- **Disk writes**: Uploads disk data as **Nutanix Images** via v3 API, then clones image to VM disk
- **Image is intermediate**: Backup → convert to raw → upload as Image → clone to disk
- **VM creation**: v3 API with `data_source_reference` pointing to the uploaded Image
- **VG problem**: Does NOT exist — uses Images, not Volume Groups

### datamigrate (this project)
- **Runs as**: External CLI tool (on migration host VM inside cluster, or remotely)
- **Replication**: VMware CBT — snapshot + changed block queries (same as Move)
- **Disk writes**: Writes blocks to **Volume Group via iSCSI** (pure Go initiator)
- **VG is intermediate**: Data goes to VG, then needs conversion to regular disk
- **VM creation**: v3 API + v4 API to attach VG
- **VG problem**: YES — disk is passthrough, not a regular vDisk

---

## The Core Architectural Difference

All three commercial tools **run an appliance on the Nutanix cluster itself**:

```
Move/Zerto/Veeam Proxy (VM on AHV)
    ↓
  192.168.5.x backplane (internal CVM communication)
    ↓
  Direct vDisk writes to storage container
    ↓
  Regular VM disk — no conversion needed
```

datamigrate runs externally and uses public APIs:

```
datamigrate (external or migration host VM)
    ↓
  iSCSI over network (port 3260)
    ↓
  Volume Group (VG) disk
    ↓
  ??? conversion step needed → regular VM disk
```

---

## Storage Mechanism Comparison

| Tool | Where Disk Data Goes | Intermediate? | Runs On-Cluster? | Conversion Needed? |
|------|---------------------|---------------|-------------------|-------------------|
| Nutanix Move | vDisk in storage container | No | Yes | No |
| Zerto | vDisk in storage container | No | Yes (ZCA) | No |
| Veeam | Image → clone to disk | Image is intermediate | Yes (proxy) | No (clone handles it) |
| datamigrate | Volume Group via iSCSI | VG is intermediate | Optional | **Yes** |

## VM Creation Comparison

| Tool | API | Disk Attachment Method | Boot Ready? |
|------|-----|----------------------|-------------|
| Nutanix Move | v2/v3 | Direct vDisk UUID reference | Yes |
| Zerto | v2/v3 | Direct vDisk UUID reference | Yes |
| Veeam | v3 | Clone from Image (`data_source_reference`) | Yes |
| datamigrate | v3 + v4 | VG passthrough (`attach-vm`) | Yes, but VG-tied |

## Replication Comparison

| Tool | Method | Incremental | Network Efficiency |
|------|--------|-------------|-------------------|
| Nutanix Move | VMware CBT snapshots | Yes (CBT deltas) | Only changed blocks |
| Zerto | Real-time I/O intercept (VAIO) | Continuous | Every write, as it happens |
| Veeam | CBT backup + restore | Yes (incremental backup) | Full disk re-upload per restore |
| datamigrate | VMware CBT snapshots | Yes (CBT deltas) | Only changed blocks (iSCSI) |

---

## Options to Solve datamigrate's VG-to-Disk Problem

### Option A: v2 API vDisk Clone (BEST — zero extra data movement)

The VG disk is internally a regular vDisk in a storage container. The v2 API's `vm_disk_clone` can reference it by `vmdisk_uuid`:

```bash
# 1. Get the VG's internal vDisk UUID
curl -sk -u admin:PASS \
  'https://CVM_IP:9440/api/nutanix/v2.0/volume_groups/VG_UUID' \
  | jq '.disk_list[].vmdisk_uuid'

# 2. Create VM with disk cloned from that vDisk
curl -sk -u admin:PASS -X POST \
  -H "Content-Type: application/json" \
  -d '{
    "name": "ubuntu-vm-ahv",
    "memory_mb": 4096,
    "num_vcpus": 2,
    "vm_disks": [{
      "vm_disk_clone": {
        "disk_address": {
          "vmdisk_uuid": "VMDISK_UUID_FROM_VG"
        },
        "minimum_size": 322122547200
      }
    }],
    "vm_nics": [{"network_uuid": "SUBNET_UUID"}],
    "boot": {"uefi_boot": true}
  }' \
  'https://CVM_IP:9440/api/nutanix/v2.0/vms'
```

**Pros**: No extra data movement. Clone is a fast metadata/CoW operation (~seconds).
**Cons**: Uses v2 API (Prism Element, not Prism Central). Needs testing to confirm VG vmdisk_uuid works.
**Downtime**: ~1 min (create VM + clone)

### Option B: Image Upload from VG (Veeam's approach — safe fallback)

Read the data back from the VG via iSCSI, upload as Nutanix Image, create VM from image:

```
iSCSI READ from VG → upload as Image → create VM with clone from image
```

**Pros**: Well-supported, standard Nutanix path. Same as what Veeam does.
**Cons**: Doubles data movement (300GB read back + 300GB upload = 600GB extra I/O).
**Downtime**: ~30-60 min for 300GB

### Option C: VG Passthrough (current approach — works but not ideal)

Keep VG attached as passthrough disk. VM boots and works.

**Pros**: Already working. Zero extra data movement.
**Cons**: Disk tied to VG. Shows 0 GiB in UI. VG deletion kills the disk. Not standard.
**Downtime**: None

### Option D: dd Inside VM (universal fallback)

Boot VM from VG, add empty regular disk, dd the data, swap:

```bash
# Inside the VM:
dd if=/dev/sda of=/dev/sdb bs=64M status=progress
```

**Pros**: Always works. No API dependencies.
**Cons**: Slow (~25-60 min for 300GB). Requires VM access. Brief downtime for swap.
**Downtime**: ~2-5 min (after dd completes)

---

## Recommended Path for datamigrate

### Short-term: Option A (v2 API vDisk Clone)
1. Write data to VG via iSCSI (current working approach)
2. At cutover: get VG disk's `vmdisk_uuid` via v2 API
3. Create VM using v2 API `vm_disk_clone` with that `vmdisk_uuid`
4. Delete VG — VM disk is now independent
5. **Test this approach** — if v2 clone from VG vmdisk works, this is the cleanest solution

### Fallback: Option B (Image Upload)
If Option A doesn't work with VG vDisks:
1. At cutover: read VG disk via iSCSI, stream to Image upload API
2. Create VM with disk cloned from image
3. Delete VG and image

### Long-term: Deploy datamigrate as on-cluster appliance
Like Move/Zerto/Veeam, deploy datamigrate as a VM on the target cluster with internal storage access. This eliminates the VG intermediate step entirely.

---

## Key Takeaway

The fundamental difference is **where the tool runs**:
- On-cluster tools (Move, Zerto, Veeam proxy) write directly to vDisks — no conversion needed
- External tools (datamigrate) must use public APIs (iSCSI → VG or HTTP → Image) — conversion needed at cutover

The v2 API `vm_disk_clone` with VG's `vmdisk_uuid` is the most promising approach to bridge this gap without extra data movement. It needs validation.
