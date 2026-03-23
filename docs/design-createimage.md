# Design: Convert Volume Group Disk to Native AHV VM Disk

## Goal

After writing raw disk blocks to a Nutanix Volume Group (VG) via iSCSI, create a VM on AHV that boots from that data. This document provides **actionable API details** for every known approach.

---

## Background: The Problem

Our migration pipeline:

```
VMware snapshot + CBT deltas
  -> raw block reads (VDDK/govmomi)
  -> iSCSI writes to Nutanix Volume Group
  -> ??? create VM that uses this disk data
```

The VG now contains a perfect block-level copy of the VMware disk. The question is how to get from "VG with raw disk data" to "bootable AHV VM with native disk".

---

## Approach 1: VG Direct Attach (Passthrough)

**Status: WORKING — already implemented in datamigrate**

Attach the Volume Group directly to a VM. The VM sees the VG disk as a SCSI device.

### How It Works

1. Create VM (no disks) via Prism Central v3 API
2. Attach VG to VM via v4 Volumes API

### API Calls

**Step 1: Create VM (v3 API)**

```bash
curl -sk -u admin:PASSWORD -X POST \
  -H "Content-Type: application/json" \
  -d '{
    "spec": {
      "name": "migrated-vm",
      "resources": {
        "num_sockets": 2,
        "num_vcpus_per_socket": 1,
        "memory_size_mib": 4096,
        "power_state": "OFF",
        "machine_type": "PC",
        "nic_list": [{
          "subnet_reference": {"kind": "subnet", "uuid": "SUBNET_UUID"}
        }]
      },
      "cluster_reference": {"kind": "cluster", "uuid": "CLUSTER_UUID"}
    },
    "metadata": {"kind": "vm"}
  }' \
  'https://PC:9440/api/nutanix/v3/vms'
```

**Step 2: Detach iSCSI client from VG (v4 API)**

```bash
# First list iSCSI clients
curl -sk -u admin:PASSWORD -X GET \
  -H "NTNX-Request-Id: $(uuidgen)" \
  'https://PC:9440/api/volumes/v4.1/config/volume-groups/VG_UUID/external-iscsi-attachments'

# Then detach each client
curl -sk -u admin:PASSWORD -X POST \
  -H "Content-Type: application/json" \
  -H "NTNX-Request-Id: $(uuidgen)" \
  -d '{"extId": "ISCSI_CLIENT_EXT_ID"}' \
  'https://PC:9440/api/volumes/v4.1/config/volume-groups/VG_UUID/$actions/detach-iscsi-client'
```

**Step 3: Attach VG to VM (v4 API)**

```bash
curl -sk -u admin:PASSWORD -X POST \
  -H "Content-Type: application/json" \
  -H "NTNX-Request-Id: $(uuidgen)" \
  -d '{"extId": "VM_UUID"}' \
  'https://PC:9440/api/volumes/v4.1/config/volume-groups/VG_UUID/$actions/attach-vm'
```

### Pros
- Already working in our codebase
- Zero data copy — VM uses the VG disk directly
- Fastest path to booting (seconds, not minutes)
- Supports incremental sync (keep writing to VG, VM sees updates)

### Cons
- Disk shows as 0 GiB in Prism UI (cosmetic issue)
- VG must remain for the lifetime of the VM
- Requires virtio-scsi drivers in guest OS (Linux has them; Windows needs VirtIO drivers)
- Day-2 operations (snapshots, cloning) may behave differently than native disks
- Not how Nutanix Move or Veeam produce their final artifact

---

## Approach 2: Image Intermediate via acli (CVM SSH)

**Status: VALIDATED — documented in Nutanix KB-9788 and community posts**

Create a Nutanix Image from the VG disk's underlying vmdisk, then create a VM disk from that image.

### Prerequisites

- SSH access to a CVM (Controller VM)
- Know the VG disk's `vmdisk_uuid` and `container_name`

### Step-by-Step

```bash
# 1. Get VG details and find the vmdisk_uuid
acli vg.get VOLUME_GROUP_NAME
# Output includes:
#   disk_list {
#     vmdisk_uuid: "abcd1234-..."
#     storage_container_name: "default-container"
#   }

# 2. Create image from the VG disk's backing vmdisk
acli image.create migrated-disk-image \
  source_url=nfs://127.0.0.1/default-container/.acropolis/vmdisk/abcd1234-... \
  container=default-container \
  image_type=kDiskImage

# 3. Verify image creation
acli image.get migrated-disk-image

# 4. Create VM with disk cloned from the image
acli vm.disk_create migrated-vm clone_from_image=migrated-disk-image bus=scsi

# 5. Or: create a new VM entirely
acli vm.create migrated-vm memory=4096M num_vcpus=2
acli vm.disk_create migrated-vm clone_from_image=migrated-disk-image bus=scsi
acli vm.nic_create migrated-vm network=vlan0
acli vm.on migrated-vm

# 6. Clean up
acli image.delete migrated-disk-image   # optional
acli vg.delete VOLUME_GROUP_NAME         # after verification
```

### NFS Source URL Format

```
nfs://127.0.0.1/<container_name>/.acropolis/vmdisk/<vmdisk_uuid>
```

This accesses the raw vdisk data through the local CVM's NFS mount of the Nutanix distributed storage.

### Pros
- Well-validated by Nutanix community and KB articles
- Produces a clean native AHV VM disk
- Image can be reused to create multiple VMs
- Image creation can run while VM is still using VG (no downtime for the copy phase)

### Cons
- Requires CVM SSH access (may not be available in all environments, e.g. NC2 on AWS)
- Temporarily uses 2-3x storage (VG + image + cloned disk)
- For 300 GiB disk: image creation takes ~5-20 min, total process ~15-40 min
- Not fully automatable via REST API alone

---

## Approach 3: Prism Element v2 API — Create Image from vmdisk_uuid

**Status: VALIDATED — the v2 images API supports vm_disk_clone_spec**

Create an image directly from a vmdisk_uuid using the Prism Element v2 REST API. No CVM SSH needed.

### API Call

```bash
curl -sk -u admin:PASSWORD -X POST \
  -H "Content-Type: application/json" \
  -d '{
    "annotation": "Migrated disk image from Volume Group",
    "image_type": "DISK_IMAGE",
    "name": "migrated-disk-image",
    "vm_disk_clone_spec": {
      "disk_address": {
        "vmdisk_uuid": "VG_DISK_VMDISK_UUID"
      },
      "storage_container_uuid": "TARGET_CONTAINER_UUID"
    }
  }' \
  'https://PE_IP:9440/PrismGateway/services/rest/v2.0/images/'
```

**Important**: This goes to Prism Element (PE), NOT Prism Central (PC). The PE IP is typically a CVM IP (e.g., `172.16.3.10:9440`).

### Getting the vmdisk_uuid from the VG

Via v4 API — list VG disks:

```bash
curl -sk -u admin:PASSWORD -X GET \
  -H "NTNX-Request-Id: $(uuidgen)" \
  'https://PC:9440/api/volumes/v4.1/config/volume-groups/VG_UUID/disks'
```

The response includes disk `extId` values. These are the vmdisk UUIDs.

Via acli on CVM:

```bash
acli vg.get VOLUME_GROUP_NAME
# Look for disk_list[].vmdisk_uuid
```

### Then Attach Image Disk to VM

After the image is created, attach a clone of it to a VM:

```bash
# Option A: v2 API (Prism Element)
curl -sk -u admin:PASSWORD -X POST \
  -H "Content-Type: application/json" \
  -d '{
    "uuid": "VM_UUID",
    "vm_disks": [{
      "is_cdrom": false,
      "vm_disk_clone": {
        "disk_address": {
          "vmdisk_uuid": "IMAGE_VMDISK_UUID"
        },
        "storage_container_uuid": "TARGET_CONTAINER_UUID"
      }
    }]
  }' \
  'https://PE_IP:9440/PrismGateway/services/rest/v2.0/vms/VM_UUID/disks/attach'

# Option B: v3 API (Prism Central) — create VM with image reference
curl -sk -u admin:PASSWORD -X POST \
  -H "Content-Type: application/json" \
  -d '{
    "spec": {
      "name": "migrated-vm",
      "resources": {
        "num_sockets": 2,
        "num_vcpus_per_socket": 1,
        "memory_size_mib": 4096,
        "power_state": "OFF",
        "disk_list": [{
          "data_source_reference": {
            "kind": "image",
            "uuid": "IMAGE_UUID"
          },
          "device_properties": {
            "device_type": "DISK",
            "disk_address": {
              "adapter_type": "SCSI",
              "device_index": 0
            }
          }
        }],
        "nic_list": [{
          "subnet_reference": {"kind": "subnet", "uuid": "SUBNET_UUID"}
        }]
      },
      "cluster_reference": {"kind": "cluster", "uuid": "CLUSTER_UUID"}
    },
    "metadata": {"kind": "vm"}
  }' \
  'https://PC:9440/api/nutanix/v3/vms'
```

### Pros
- Fully automatable via REST API (no CVM SSH needed)
- Clean native image + VM disk result
- Image UUID returned in response — can be used immediately
- Works from any machine with API access

### Cons
- Requires Prism Element API access (not just Prism Central)
- Need to know the vmdisk_uuid from the VG (requires v4 VG disk list or CVM acli)
- Image creation time is proportional to disk size
- Temporarily uses 2-3x storage
- v2 API is considered legacy (but still fully functional)

---

## Approach 4: Prism Element v2 API — Direct vm_disk_clone from VG vmdisk

**Status: PARTIALLY VALIDATED — the v2 schema supports this but community validation is sparse**

Skip the image intermediate entirely. Clone the VG disk directly to a VM disk using the v2 `vm_disk_clone` with `vmdisk_uuid`.

### API Call

```bash
curl -sk -u admin:PASSWORD -X POST \
  -H "Content-Type: application/json" \
  -d '{
    "uuid": "VM_UUID",
    "vm_disks": [{
      "is_cdrom": false,
      "vm_disk_clone": {
        "disk_address": {
          "device_bus": "SCSI",
          "device_index": 0,
          "vmdisk_uuid": "VG_DISK_VMDISK_UUID"
        },
        "storage_container_uuid": "TARGET_CONTAINER_UUID"
      }
    }]
  }' \
  'https://PE_IP:9440/PrismGateway/services/rest/v2.0/vms/VM_UUID/disks/attach'
```

### Alternative: Include volume_group_uuid

The v2 API schema also supports `volume_group_uuid` in the disk_address:

```bash
curl -sk -u admin:PASSWORD -X POST \
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
  'https://PE_IP:9440/PrismGateway/services/rest/v2.0/vms/VM_UUID/disks/attach'
```

### Pros
- Single API call — no image intermediate
- Fast storage-layer clone (metadata operation, not full data copy on same container)
- Minimal temporary storage overhead
- Fully automatable

### Cons
- Not widely validated for VG-backed vmdisk_uuids specifically
- Prism Element only (not Prism Central)
- v2 API is legacy
- May fail on some AOS versions — needs testing
- **Must test on target cluster before relying on this in production**

---

## Approach 5: Prism Central v3 API — Create Image with source_uri

**Status: THEORETICAL — v3 images API supports source_uri but NFS access from PC is unclear**

The v3 API for creating images supports a `source_uri` field. If we can construct an NFS URI pointing to the VG disk's backing vmdisk, this would work from Prism Central.

### API Call

```bash
curl -sk -u admin:PASSWORD -X POST \
  -H "Content-Type: application/json" \
  -d '{
    "spec": {
      "name": "migrated-disk-image",
      "resources": {
        "image_type": "DISK_IMAGE",
        "source_uri": "nfs://127.0.0.1/default-container/.acropolis/vmdisk/VG_DISK_VMDISK_UUID"
      },
      "description": "Migrated from VMware VG"
    },
    "api_version": "3.1.0",
    "metadata": {
      "kind": "image"
    }
  }' \
  'https://PC:9440/api/nutanix/v3/images'
```

**Then upload completes automatically** — the source_uri tells Prism to copy from the NFS path.

### Caveats

- The `nfs://127.0.0.1/...` URI works from a CVM because the CVM has local NFS access to the storage fabric. When this request goes through Prism Central, it is forwarded to Prism Element which executes it on a CVM, so it should still work.
- This is the API equivalent of `acli image.create ... source_url=nfs://...`
- Requires knowing the container name and vmdisk_uuid

### Pros
- Uses Prism Central v3 API (our existing API surface)
- No CVM SSH needed
- Produces a standard Nutanix image
- Can be followed by v3 VM creation with image data_source_reference

### Cons
- Need to resolve: does `nfs://127.0.0.1/...` work when sent through Prism Central?
- Need the vmdisk_uuid (from v4 VG disk list)
- Need the container name (from v4 VG disk metadata or cluster config)
- Image creation time proportional to disk size

---

## Approach 6: Prism Central v4 VMM API — Create VM with Disk from Image

**Status: DOCUMENTED — the v4 VMM API has the cleanest disk specification**

The v4 VMM API (`/api/vmm/v4.0/ahv/config/vms`) supports creating VMs with disks. The disk model uses `backingInfo` which can reference an image via `dataSource`.

### REST API JSON Structure (v4)

```bash
curl -sk -u admin:PASSWORD -X POST \
  -H "Content-Type: application/json" \
  -H "NTNX-Request-Id: $(uuidgen)" \
  -d '{
    "name": "migrated-vm",
    "description": "Migrated from VMware",
    "cluster": {
      "extId": "CLUSTER_UUID"
    },
    "numSockets": 2,
    "numCoresPerSocket": 1,
    "memorySizeBytes": 4294967296,
    "disks": [{
      "diskAddress": {
        "busType": "SCSI",
        "index": 0
      },
      "backingInfo": {
        "$objectType": "vmm.v4.ahv.config.VmDisk",
        "dataSource": {
          "reference": {
            "$objectType": "vmm.v4.ahv.config.ImageReference",
            "imageExtId": "IMAGE_UUID"
          }
        }
      }
    }],
    "nics": [{
      "networkInfo": {
        "subnet": {
          "extId": "SUBNET_UUID"
        }
      }
    }]
  }' \
  'https://PC:9440/api/vmm/v4.0/ahv/config/vms'
```

### v4 Disk Model — ADSF Volume Group Reference

The v4 API also supports **directly referencing a Volume Group** as a disk backing:

```json
{
  "diskAddress": {
    "busType": "SCSI",
    "index": 0
  },
  "backingInfo": {
    "$objectType": "vmm.v4.ahv.config.ADSFVolumeGroupReference",
    "volumeGroupExtId": "VG_UUID"
  }
}
```

This is the v4 API equivalent of VG passthrough (Approach 1) — the VM disk is backed by the Volume Group.

### Python SDK Equivalent

From the official Nutanix code samples:

```python
import ntnx_vmm_py_client.models.vmm.v4.ahv.config as AhvVmConfig

# Disk cloned from an image
cloned_disk = AhvVmConfig.Disk.Disk(
    backing_info=AhvVmConfig.VmDisk.VmDisk(
        data_source=AhvVmConfig.DataSource.DataSource(
            reference=AhvVmConfig.ImageReference.ImageReference(
                image_ext_id=image_ext_id
            )
        )
    ),
    disk_address=AhvVmConfig.DiskAddress.DiskAddress(
        bus_type=AhvVmConfig.DiskBusType.DiskBusType.SCSI,
        index=0
    ),
)

# Disk backed by a Volume Group
vg_disk = AhvVmConfig.Disk.Disk(
    backing_info=AhvVmConfig.ADSFVolumeGroupReference.ADSFVolumeGroupReference(
        volume_group_ext_id=vg_uuid
    ),
    disk_address=AhvVmConfig.DiskAddress.DiskAddress(
        bus_type=AhvVmConfig.DiskBusType.DiskBusType.SCSI,
        index=0
    ),
)
```

### Pros
- Modern, well-documented API
- Clean JSON structure
- Supports both image-backed and VG-backed disks
- Works from Prism Central
- Has official SDKs (Python, Java, Go)

### Cons
- Requires pc.2022.9 or later
- Still needs the image creation step first (Approach 3 or 5)
- v4 API requires ETag/If-Match headers for updates

---

## Approach 7: Read VG via iSCSI + Upload to Image Service

**Status: FULLY PORTABLE — works from any machine with network access**

Read the VG disk data back over iSCSI and upload it directly to the Nutanix Image Service. This is a fallback that does not require CVM SSH or Prism Element API access.

### Workflow

```
1. Connect to VG via iSCSI (we already know how to do this)
2. Read all blocks from the iSCSI target
3. Stream raw disk data to Nutanix Image Service via PUT
4. Create VM with disk from that image
```

### API Calls

**Step 1: Create empty image placeholder (v3)**

```bash
curl -sk -u admin:PASSWORD -X POST \
  -H "Content-Type: application/json" \
  -d '{
    "spec": {
      "name": "migrated-disk-image",
      "resources": {"image_type": "DISK_IMAGE"},
      "description": "Migrated from VMware via datamigrate"
    },
    "metadata": {"kind": "image"}
  }' \
  'https://PC:9440/api/nutanix/v3/images'
# Returns IMAGE_UUID
```

**Step 2: Upload raw disk data to the image**

```bash
curl -sk -u admin:PASSWORD -X PUT \
  -H "Content-Type: application/octet-stream" \
  --data-binary @/path/to/raw-disk.img \
  'https://PC:9440/api/nutanix/v3/images/IMAGE_UUID/file'
```

Or stream from iSCSI in our Go code:

```go
// Pseudo-code: read from iSCSI, pipe to image upload
reader := iscsi.NewBlockReader(portal, target, lun)
err := nxClient.UploadImageStream(ctx, imageUUID, reader, diskSizeBytes)
```

**Step 3: Create VM with disk from image (v3)**

```bash
curl -sk -u admin:PASSWORD -X POST \
  -H "Content-Type: application/json" \
  -d '{
    "spec": {
      "name": "migrated-vm",
      "resources": {
        "disk_list": [{
          "data_source_reference": {"kind": "image", "uuid": "IMAGE_UUID"},
          "device_properties": {
            "device_type": "DISK",
            "disk_address": {"adapter_type": "SCSI", "device_index": 0}
          }
        }],
        ...
      }
    },
    "metadata": {"kind": "vm"}
  }' \
  'https://PC:9440/api/nutanix/v3/vms'
```

### Pros
- Works from any machine (no CVM SSH, no PE access needed)
- We already have iSCSI read capabilities
- We already have image upload capabilities
- Fully portable across all Nutanix deployment types (on-prem, NC2, etc.)

### Cons
- Reads entire disk over network TWICE (once for migration, once for image upload)
- For a 300 GiB disk: ~30-60 min at 100 MB/s network throughput
- Doubles the network I/O compared to other approaches
- Uses temporary storage for the image

---

## How Nutanix Move Does It

Based on research of Nutanix Move's architecture:

### Architecture

- Move is a VM appliance running on the target AHV cluster
- **Source agent**: Uses VADP/VDDK/CBT to read VMware disk blocks
- **Disk reader**: Reads blocks from source using platform-specific APIs
- **Disk writer**: Writes blocks to target Nutanix storage

### Target-Side Mechanism

Nutanix Move runs inside the cluster and has direct access to the Nutanix distributed storage fabric (ADSF) via NFS. The Move appliance:

1. **Creates a vdisk directly** on the target storage container via NFS writes through Stargate (the Nutanix storage I/O manager)
2. **Writes blocks directly** to the vdisk as they arrive from the source
3. **Creates a VM** referencing the vdisk when migration is complete
4. The VM gets a **native AHV disk** — not a VG passthrough

Move does NOT use Volume Groups or iSCSI on the target side. It writes directly to the storage fabric because it runs inside the cluster with privileged NFS access.

### Why datamigrate Uses VGs Instead

datamigrate runs **outside** the cluster (on the operator's machine or a remote VM). It cannot write directly to ADSF via NFS. Instead, it uses iSCSI to Volume Groups, which is the only block-level write path available to external clients.

This means datamigrate needs an additional step that Move does not: converting the VG-backed disk to a native VM disk.

---

## Comparison Matrix

| # | Approach | API Surface | CVM SSH? | PE API? | Data Copy? | Downtime | Automation |
|---|----------|-------------|----------|---------|------------|----------|------------|
| 1 | VG Direct Attach | v3 + v4 Volumes | No | No | None | None | Full |
| 2 | acli Image Intermediate | CVM CLI | **Yes** | No | 1x clone | ~15-40 min | Manual |
| 3 | PE v2 Image from vmdisk | PE v2 | No | **Yes** | 1x clone | ~15-40 min | Full |
| 4 | PE v2 Direct vm_disk_clone | PE v2 | No | **Yes** | 1x clone | ~5-15 min | Full |
| 5 | PC v3 Image from source_uri | PC v3 | No | No | 1x clone | ~15-40 min | Full |
| 6 | v4 VMM + Image | v4 VMM | No | No | 1x clone | ~15-40 min | Full |
| 7 | iSCSI Read + Image Upload | v3 | No | No | 2x (read+upload) | ~30-60 min | Full |

---

## Recommended Implementation Strategy

### Phase 1: MVP — Approach 5 + 6 (PC v3 Image from source_uri + v4/v3 VM creation)

**Why**: This uses only Prism Central APIs that we already have client code for.

**Workflow**:

```
1. Get VG disk's vmdisk_uuid via v4 Volumes API
   GET /api/volumes/v4.1/config/volume-groups/{VG_UUID}/disks

2. Get storage container name
   (from VG disk metadata or cluster config)

3. Create image from VG disk via v3 images API with source_uri
   POST /api/nutanix/v3/images
   body: { spec.resources.source_uri: "nfs://127.0.0.1/{container}/.acropolis/vmdisk/{vmdisk_uuid}" }

4. Wait for image creation task to complete
   GET /api/nutanix/v3/tasks/{task_uuid}

5. Create VM with disk from image via v3 API
   POST /api/nutanix/v3/vms
   body: { spec.resources.disk_list[0].data_source_reference: { kind: "image", uuid: IMAGE_UUID } }

6. Power on VM

7. Detach and delete VG (cleanup)
```

**Implementation in Go**: We already have `CreateImage`, `WaitForTask`, and `CreateVM` methods. We need to add:

- `ListVGDisks(ctx, vgUUID) ([]VGDiskInfo, error)` — get vmdisk_uuid from v4 VG disk list
- `CreateImageFromVdisk(ctx, name, containerName, vmdiskUUID) (string, error)` — create image with source_uri
- `GetStorageContainerName(ctx, containerUUID) (string, error)` — resolve container UUID to name

### Phase 2: Fallback — Approach 7 (iSCSI Read + Image Upload)

If source_uri does not work through PC (e.g., `nfs://127.0.0.1` is not resolvable), fall back to reading the VG disk over iSCSI and uploading to the Image Service.

This reuses our existing iSCSI reader and image upload code.

### Phase 3: Fast Path — Approach 4 (PE v2 Direct Clone)

If the environment provides PE API access, test and use the direct `vm_disk_clone` path. This avoids creating an image entirely and is the fastest approach.

### Phase 4: Production Polish

- Auto-detect which approach is available
- Support all three backends behind a single `FinalizeNativeDisk()` interface
- Add rollback (reattach VG if native boot fails)

---

## Key API Details Summary

### Getting VG Disk vmdisk_uuid

**v4 Volumes API**:
```
GET /api/volumes/v4.1/config/volume-groups/{VG_UUID}/disks
```

Response includes disk objects with `extId` (this IS the vmdisk_uuid).

**acli (CVM)**:
```bash
acli vg.get VOLUME_GROUP_NAME
# disk_list[].vmdisk_uuid
```

### Creating Image from vmdisk_uuid

**v2 Prism Element API**:
```
POST /PrismGateway/services/rest/v2.0/images/
Body: { name, image_type: "DISK_IMAGE", vm_disk_clone_spec: { disk_address: { vmdisk_uuid }, storage_container_uuid } }
```

**v3 Prism Central API** (via source_uri):
```
POST /api/nutanix/v3/images
Body: { spec: { name, resources: { image_type: "DISK_IMAGE", source_uri: "nfs://127.0.0.1/CONTAINER/.acropolis/vmdisk/VMDISK_UUID" } }, metadata: { kind: "image" } }
```

**acli (CVM)**:
```bash
acli image.create NAME source_url=nfs://127.0.0.1/CONTAINER/.acropolis/vmdisk/UUID container=CONTAINER image_type=kDiskImage
```

### Creating VM Disk from Image

**v3 Prism Central API**:
```json
"disk_list": [{
  "data_source_reference": {"kind": "image", "uuid": "IMAGE_UUID"},
  "device_properties": {"device_type": "DISK", "disk_address": {"adapter_type": "SCSI", "device_index": 0}}
}]
```

**v4 VMM API**:
```json
"disks": [{
  "diskAddress": {"busType": "SCSI", "index": 0},
  "backingInfo": {
    "$objectType": "vmm.v4.ahv.config.VmDisk",
    "dataSource": {
      "reference": {
        "$objectType": "vmm.v4.ahv.config.ImageReference",
        "imageExtId": "IMAGE_UUID"
      }
    }
  }
}]
```

**v2 Prism Element API**:
```
POST /PrismGateway/services/rest/v2.0/vms/{VM_UUID}/disks/attach
Body: { uuid: VM_UUID, vm_disks: [{ vm_disk_clone: { disk_address: { vmdisk_uuid: IMAGE_VMDISK_UUID }, storage_container_uuid } }] }
```

### VG Direct Attach (v4)

```
POST /api/volumes/v4.1/config/volume-groups/{VG_UUID}/$actions/attach-vm
Body: {"extId": "VM_UUID"}
```

---

## Proposed Go Interface

```go
// FinalizeMethod indicates which approach was used
type FinalizeMethod string

const (
    FinalizeSourceURI  FinalizeMethod = "source_uri"   // v3 image from NFS source_uri
    FinalizeISCSIRead  FinalizeMethod = "iscsi_read"    // Read VG via iSCSI + upload image
    FinalizePEClone    FinalizeMethod = "pe_clone"      // PE v2 direct vm_disk_clone
    FinalizeVGAttach   FinalizeMethod = "vg_attach"     // Direct VG attach (no conversion)
)

type FinalizeRequest struct {
    MigrationID      string
    VGUUID           string
    VMName           string
    ClusterUUID      string
    SubnetUUID       string
    NumCPUs          int32
    MemoryMB         int64
    BootType         string           // "UEFI" or "LEGACY"
    ImageName        string           // name for intermediate image
    DeleteVGOnDone   bool
    DeleteImageOnDone bool
    PreferredMethod  FinalizeMethod   // "" = auto-detect
}

type FinalizeResult struct {
    VMUUID       string
    ImageUUID    string
    DiskUUID     string
    Method       FinalizeMethod
    VGDeleted    bool
    ImageDeleted bool
}
```

---

## Open Questions to Resolve by Testing

1. **Does `source_uri: nfs://127.0.0.1/...` work when sent through Prism Central v3 API?**
   - If yes: Approach 5 is our primary path
   - If no: Fall back to Approach 3 (PE v2) or Approach 7 (iSCSI read)

2. **Does PE v2 `vm_disk_clone` accept a VG-backed vmdisk_uuid?**
   - If yes: Approach 4 is the fastest path
   - If no: Must use image intermediate

3. **What is the `extId` returned by v4 VG disk list?**
   - Is it the same as the `vmdisk_uuid` used by acli and PE v2 API?
   - Need to verify by comparing values from both APIs

4. **How to get the container name from the VG disk?**
   - v4 VG disk response may include `storageContainerId`
   - Need PE v2 `storage_containers/{uuid}` to resolve ID to name
   - Or use v4 storage API

---

## References

- [Nutanix KB-9788: Convert VG disk to direct attached disk](https://portal.nutanix.com/kb/9788)
- [Nutanix Community: Create Disk Image From Volume Group](https://next.nutanix.com/installation-configuration-23/how-to-create-a-disk-image-from-volume-group-39013)
- [Nutanix Community: vDisk Part 3 - Creating an Image from an Existing vDisk](https://next.nutanix.com/intelligent-operations-26/ahv-vdisk-part-3-creating-an-image-of-or-from-an-existing-vdisk-33686)
- [Nutanix Community: Attach Disk from Image Service to Existing VM](https://next.nutanix.com/how-it-works-22/how-can-i-attach-a-disk-from-the-image-service-to-an-exiting-vm-32845)
- [Nutanix v2 API Reference](https://www.nutanix.dev/api_reference/apis/prism_v2.html)
- [Nutanix v3 API Reference](https://www.nutanix.dev/api_reference/apis/prism_v3.html)
- [Nutanix v4 VMM API Reference](https://developers.nutanix.com/api-reference?namespace=vmm&version=v4.0.b1)
- [Nutanix v4 Volumes API Reference](https://developers.nutanix.com/api-reference?namespace=volumes)
- [Nutanix v4 API User Guide](https://www.nutanix.dev/nutanix-api-user-guide/)
- [Nutanix v4 Create VM Python SDK Sample](https://www.nutanix.dev/code_samples/v4-api-create-prism-central-vm-python-sdk/)
- [Nutanix v4 Create Shell VM with curl](https://www.nutanix.dev/code_samples/create-shell-vm-with-json-spec-v4-api-and-curl/)
- [Nutanix Move Architecture (AWS)](https://docs.aws.amazon.com/guidance/latest/migrating-vmware-virtual-machines-to-nutanix-cloud-clusters-on-aws/nutanix-move-architecture-overview.html)
- [Nutanix Bible: VM Migration Architecture](https://www.nutanixbible.com/21b-vm-migration-arch.html)
- [Nutanix Terraform Provider v2 VM Resource](https://registry.terraform.io/providers/nutanix/nutanix/2.3.2/docs/resources/virtual_machine_v2)
- [Nutanix Bible: CLI Reference](https://www.nutanixbible.com/19b-cli.html)
