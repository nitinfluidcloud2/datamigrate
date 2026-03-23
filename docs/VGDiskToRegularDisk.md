# Converting Volume Group Disk to Regular VM Disk (vDisk)

## Problem

After migrating a VM via iSCSI transport, the disk is attached as a **VG passthrough disk** (`scsi.0`, shows 0 GiB in Prism UI). The data is there and the VM boots, but the disk is tied to the Volume Group. We want to convert it to a regular, independent VM disk.

**Official Nutanix KB**: [KB-9788: Convert a disk from Nutanix Volumes to a direct attached disk](https://portal.nutanix.com/kb/9788) (requires portal login)

---

## Approach 1: Image Intermediate via acli (RECOMMENDED)

**Status**: Validated by Nutanix Community users. Most commonly documented approach.

### Steps (run on CVM via SSH)

```bash
# 1. Find the VG and its disk UUID
acli vg.list
acli vg.get datamigrate-ubuntu-vm
# Note the vmdisk_uuid and container_name from the output

# 2. Create image from VG disk (can run while VM is still up)
acli image.create ubuntu2404-migrated \
  source_url=nfs://127.0.0.1/<container-name>/.acropolis/vmdisk/<vmdisk-uuid> \
  container=<container-name> \
  image_type=kDiskImage

# 3. Power off VM
acli vm.off ubuntu-vm-ahv

# 4. Detach VG from VM
# Via Prism UI: Volume Groups → select VG → Update → remove VM attachment
# Or via v4 API: POST /api/volumes/v4.1/config/volume-groups/{vgUUID}/$actions/detach-vm

# 5. Add cloned disk to VM
acli vm.disk_create ubuntu-vm-ahv clone_from_image=ubuntu2404-migrated bus=scsi

# 6. Power on
acli vm.on ubuntu-vm-ahv

# 7. Verify in Prism that disk shows correct size (300 GiB)

# 8. Clean up: delete image, delete VG when confirmed working
acli image.delete ubuntu2404-migrated
acli vg.delete datamigrate-ubuntu-vm
```

### Pros
- Well-documented, validated by Nutanix Community
- Clean result: proper native VM disk
- Image can be reused to create multiple VMs
- Image creation can happen while VM is still running (downtime only for swap)

### Cons
- Requires VM downtime (~10-20 min for swap)
- Temporarily uses 2x storage (VG disk + image + new VM disk)
- For 300GB, image creation takes ~5-20 min depending on cluster I/O

---

## Approach 2: Prism Element v2 API — vm_disk_clone

**Status**: Theoretical. The v2 API schema includes `volume_group_uuid` in `vm_disk_clone` but community validation is sparse.

### API Call

```bash
# POST to Prism Element (CVM IP), NOT Prism Central
curl -sk -u admin:PASSWORD \
  -X POST \
  -H "Content-Type: application/json" \
  -d '{
    "uuid": "VM_UUID",
    "vm_disks": [{
      "is_cdrom": false,
      "vm_disk_clone": {
        "disk_address": {
          "device_bus": "SCSI",
          "device_index": 0,
          "vmdisk_uuid": "VG_DISK_VMDISK_UUID",
          "volume_group_uuid": "VG_UUID"
        },
        "storage_container_uuid": "TARGET_CONTAINER_UUID"
      }
    }]
  }' \
  'https://CVM_IP:9440/api/nutanix/v2.0/vms/VM_UUID/disks/attach'
```

### Pros
- Single API call, no intermediate image
- Direct storage-layer clone (fast)

### Cons
- Requires VM downtime
- Uses older v2 API (Prism Element only, not Prism Central)
- Not widely validated for VG disks

---

## Approach 3: acli vm.disk_create clone_from_vmdisk

**Status**: Partially validated. Works for regular VM disks; uncertain for VG disks directly.

```bash
# Get the vmdisk_uuid from the VG
acli vg.get datamigrate-ubuntu-vm

# Clone it to the target VM (after detaching VG)
acli vm.disk_create ubuntu-vm-ahv clone_from_vmdisk=<vmdisk-uuid> bus=scsi
```

### Pros
- Simple single command, no intermediate image
- Fast (storage-layer clone)

### Cons
- Requires VM downtime
- May fail for VG disks specifically (different metadata)
- Fall back to Approach 1 if it fails

---

## Approach 4: dd Inside the VM

**Status**: Always works. Universal fallback.

### Steps

```bash
# 1. Add a new empty 300GB regular disk to the VM via Prism UI
#    (Attach Disk → Allocate on Storage Container → 300 GiB → SCSI)

# 2. Inside the VM, identify the disks
lsblk
# /dev/sda = VG passthrough disk (300GB, has OS)
# /dev/sdb = new empty disk (300GB)

# 3. Clone the disk
dd if=/dev/sda of=/dev/sdb bs=64M status=progress conv=notrunc oflag=direct iflag=direct

# 4. Power off VM
# 5. Detach VG from VM
# 6. Set boot order to new disk (scsi.1 → scsi.0)
# 7. Power on and verify
```

### Pros
- **Always works** regardless of Nutanix version or API support
- Minimal downtime (dd runs while VM is up, only brief shutdown to swap)
- Simple and well-understood

### Cons
- Slow: 300GB at ~200 MB/s = ~25 min. Realistically 30-60 min under load.
- Uses VM CPU and I/O during copy

---

## Approach 5: Automate in datamigrate (Future)

Add a `datamigrate convertdisk` command that:
1. Gets the VG disk's vmdisk_uuid via v4 API
2. Creates an Image from VG disk via v3 API (or acli)
3. Updates the VM to add disk from image
4. Detaches VG
5. Cleans up

This would be the end-to-end automated approach for production migrations.

---

## Comparison

| Approach | Downtime | Total Time | Validated | Automation |
|----------|----------|------------|-----------|------------|
| 1. Image intermediate (acli) | ~10-20 min | 30-40 min | Yes | Manual (CVM SSH) |
| 2. v2 API clone | ~5-10 min | ~15 min | Partial | API call |
| 3. acli clone_from_vmdisk | ~5-10 min | ~15 min | Uncertain | Manual (CVM SSH) |
| 4. dd inside VM | ~2-5 min (swap) | 30-60 min | Yes | Manual (VM SSH) |
| 5. datamigrate command | ~10-20 min | 30-40 min | Not yet | Fully automated |

## Recommendation

**Use Approach 1 (Image Intermediate via acli)** for now — it's the most validated approach.

If CVM SSH is not available (e.g., OVH restrictions), use **Approach 4 (dd)** as fallback.

For production at scale, implement **Approach 5** to automate the full flow.

## References

- [Nutanix KB-9788: Convert VG disk to direct attached disk](https://portal.nutanix.com/kb/9788)
- [Nutanix Community: Create Disk Image From Volume Group](https://next.nutanix.com/installation-configuration-23/how-to-create-a-disk-image-from-volume-group-39013)
- [Nutanix Community: vDisk Part 3 - Creating an Image from an Existing vDisk](https://next.nutanix.com/intelligent-operations-26/ahv-vdisk-part-3-creating-an-image-of-or-from-an-existing-vdisk-33686)
- [Nutanix v2 API Reference](https://www.nutanix.dev/api_reference/apis/prism_v2.html)
- [Nutanix Bible: CLI Reference](https://www.nutanixbible.com/19b-cli.html)
