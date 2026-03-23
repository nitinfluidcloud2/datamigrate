#!/bin/bash
# Cutover: Convert Volume Group to bootable AHV VM
# Run this on migration-host after final incremental sync
#
# Prerequisites:
#   - qemu-img installed
#   - Environment variables: NUTANIX_USERNAME, NUTANIX_PASSWORD, NUTANIX_ENDPOINT
#
# Usage: Edit the variables below, then run each step manually or run the full script

set -e

# --- Configuration ---
PLAN_FILE="${1:-configs/ubuntu10-plan.yaml}"
STATE_DIR="/tmp/datamigrate/ubuntu10"
ISCSI_PORTAL="172.16.3.254:3260"
INITIATOR_IQN="iqn.2026-01.com.datamigrate:initiator"
CLUSTER_UUID="00063ca3-b484-4dba-605d-66f6fc6cf210"
VM_NAME="ubuntu10-ahv"
IMAGE_NAME="ubuntu10-migrated-disk"
NUM_CPUS=1
MEMORY_MB=2048
BOOT_TYPE="UEFI"
QCOW2_PATH="/tmp/ubuntu10-disk.qcow2"

NX_AUTH="$NUTANIX_USERNAME:$NUTANIX_PASSWORD"
NX_URL="https://$NUTANIX_ENDPOINT:9440"

# --- Auto-detect VG UUID from state.db ---
echo "=== Reading VG UUID from state.db ==="
VG_UUID=$(strings "$STATE_DIR/state.db" | grep -o 'volume_group_id":"[^"]*' | head -1 | cut -d'"' -f3)
if [ -z "$VG_UUID" ]; then
    echo "ERROR: Could not find VG UUID in state.db"
    exit 1
fi
VG_TARGET_IQN="iqn.2010-06.com.nutanix:datamigrate-ubuntu10-$VG_UUID"
echo "  VG UUID: $VG_UUID"
echo "  Target IQN: $VG_TARGET_IQN"

echo ""
echo "=== Cutover: VG → qcow2 → Image → VM ==="

# ============================================================
# Step 1: Whitelist iSCSI initiator on VG
# ============================================================
echo ""
echo "=== Step 1: Whitelist iSCSI initiator ==="
curl -sk -u $NUTANIX_USERNAME:$NUTANIX_PASSWORD -X POST \
  -H "Content-Type: application/json" \
  -H "NTNX-Request-Id: $(uuidgen)" \
  -d "{\"iscsiInitiatorName\":\"$INITIATOR_IQN\"}" \
  "$NX_URL/api/volumes/v4.1/config/volume-groups/$VG_UUID/\$actions/attach-iscsi-client"
echo ""
echo "  Waiting for task..."
sleep 5

# ============================================================
# Step 2: Read VG over iSCSI → qcow2 (using qemu-img)
# ============================================================
echo ""
echo "=== Step 2: Read VG over iSCSI → qcow2 ==="
echo "  Portal: $ISCSI_PORTAL"
echo "  Target: $VG_TARGET_IQN"
echo "  Output: $QCOW2_PATH"
time qemu-img convert --image-opts \
  "driver=iscsi,transport=tcp,portal=$ISCSI_PORTAL,target=$VG_TARGET_IQN,lun=0,initiator-name=$INITIATOR_IQN" \
  -O qcow2 "$QCOW2_PATH"
ls -lh "$QCOW2_PATH"

# ============================================================
# Step 3: Create Nutanix Image
# ============================================================
echo ""
echo "=== Step 3: Create Nutanix Image ==="
CREATE_RESP=$(curl -sk -u $NUTANIX_USERNAME:$NUTANIX_PASSWORD -X POST \
  -H "Content-Type: application/json" \
  -d "{\"spec\":{\"name\":\"$IMAGE_NAME\",\"description\":\"Migrated from VMware VG\",\"resources\":{\"image_type\":\"DISK_IMAGE\"}},\"metadata\":{\"kind\":\"image\"}}" \
  "$NX_URL/api/nutanix/v3/images")
echo "$CREATE_RESP"

# Extract UUID and task UUID from create response
IMAGE_UUID=$(echo "$CREATE_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['metadata']['uuid'])" 2>/dev/null)
TASK_UUID=$(echo "$CREATE_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['status']['execution_context']['task_uuid'])" 2>/dev/null)

if [ -z "$IMAGE_UUID" ]; then
    echo "ERROR: Could not extract image UUID from create response"
    exit 1
fi

echo "  Image UUID: $IMAGE_UUID"
echo "  Task UUID: $TASK_UUID"

# Poll task until complete
if [ -n "$TASK_UUID" ]; then
    echo "  Waiting for image task to complete..."
    for i in $(seq 1 30); do
        TASK_STATUS=$(curl -sk -u $NUTANIX_USERNAME:$NUTANIX_PASSWORD \
          "https://$NUTANIX_ENDPOINT:9440/api/nutanix/v3/tasks/$TASK_UUID" | \
          python3 -c "import sys,json; print(json.load(sys.stdin).get('status','UNKNOWN'))" 2>/dev/null)
        echo "  [$i] Task status: $TASK_STATUS"
        if [ "$TASK_STATUS" = "SUCCEEDED" ]; then
            echo "  Image ready!"
            break
        elif [ "$TASK_STATUS" = "FAILED" ]; then
            echo "ERROR: Image creation task failed"
            exit 1
        fi
        sleep 5
    done
fi

# ============================================================
# Step 5: Upload qcow2 to Image
# ============================================================
echo ""
echo "=== Step 5: Upload qcow2 to Image ==="
echo "  Uploading $(ls -lh $QCOW2_PATH | awk '{print $5}')..."
time curl -sk -u $NUTANIX_USERNAME:$NUTANIX_PASSWORD -X PUT \
  -H "Content-Type: application/octet-stream" \
  -T "$QCOW2_PATH" \
  "$NX_URL/api/nutanix/v3/images/$IMAGE_UUID/file"
echo ""
echo "  Upload complete. Waiting for image to become active..."
sleep 15

# ============================================================
# Step 6: Create VM from Image
# ============================================================
echo ""
echo "=== Step 6: Create VM ==="
curl -sk -u $NUTANIX_USERNAME:$NUTANIX_PASSWORD -X POST \
  -H "Content-Type: application/json" \
  -d "{\"spec\":{\"name\":\"$VM_NAME\",\"description\":\"Migrated from VMware\",\"resources\":{\"num_sockets\":$NUM_CPUS,\"num_vcpus_per_socket\":1,\"memory_size_mib\":$MEMORY_MB,\"power_state\":\"ON\",\"machine_type\":\"PC\",\"boot_config\":{\"boot_type\":\"$BOOT_TYPE\"},\"disk_list\":[{\"data_source_reference\":{\"kind\":\"image\",\"uuid\":\"$IMAGE_UUID\"},\"device_properties\":{\"device_type\":\"DISK\",\"disk_address\":{\"adapter_type\":\"SCSI\",\"device_index\":0}}}],\"nic_list\":[]},\"cluster_reference\":{\"kind\":\"cluster\",\"uuid\":\"$CLUSTER_UUID\"}},\"metadata\":{\"kind\":\"vm\"}}" \
  "$NX_URL/api/nutanix/v3/vms"

echo ""
echo ""
echo "========================================="
echo "  Cutover complete!"
echo "  VM: $VM_NAME"
echo "  Image: $IMAGE_NAME ($IMAGE_UUID)"
echo "  Boot: $BOOT_TYPE"
echo "========================================="
echo ""
echo "--- Individual commands for manual execution ---"
echo ""
echo "# Step 1: Whitelist iSCSI initiator"
echo "curl -sk -u \$NUTANIX_USERNAME:\$NUTANIX_PASSWORD -X POST -H \"Content-Type: application/json\" -H \"NTNX-Request-Id: \$(uuidgen)\" -d '{\"iscsiInitiatorName\":\"$INITIATOR_IQN\"}' \"$NX_URL/api/volumes/v4.1/config/volume-groups/$VG_UUID/\\\$actions/attach-iscsi-client\""
echo ""
echo "# Step 2: Read VG over iSCSI → qcow2"
echo "qemu-img convert --image-opts \"driver=iscsi,transport=tcp,portal=$ISCSI_PORTAL,target=$VG_TARGET_IQN,lun=0,initiator-name=$INITIATOR_IQN\" -O qcow2 $QCOW2_PATH"
echo ""
echo "# Step 3: Create image"
echo "curl -sk -u \$NUTANIX_USERNAME:\$NUTANIX_PASSWORD -X POST -H \"Content-Type: application/json\" -d '{\"spec\":{\"name\":\"$IMAGE_NAME\",\"description\":\"Migrated from VMware VG\",\"resources\":{\"image_type\":\"DISK_IMAGE\"}},\"metadata\":{\"kind\":\"image\"}}' \"$NX_URL/api/nutanix/v3/images\""
echo ""
echo "# Step 4: Get image UUID"
echo "curl -sk -u \$NUTANIX_USERNAME:\$NUTANIX_PASSWORD -X POST -H \"Content-Type: application/json\" -d '{\"kind\":\"image\",\"filter\":\"name==$IMAGE_NAME\"}' \"$NX_URL/api/nutanix/v3/images/list\" | python3 -m json.tool | grep -E '\"uuid\"'"
echo ""
echo "# Step 5: Upload qcow2 (replace IMAGE_UUID)"
echo "curl -sk -u \$NUTANIX_USERNAME:\$NUTANIX_PASSWORD -X PUT -H \"Content-Type: application/octet-stream\" -T $QCOW2_PATH \"$NX_URL/api/nutanix/v3/images/IMAGE_UUID/file\""
echo ""
echo "# Step 6: Create VM (replace IMAGE_UUID)"
echo "curl -sk -u \$NUTANIX_USERNAME:\$NUTANIX_PASSWORD -X POST -H \"Content-Type: application/json\" -d '{\"spec\":{\"name\":\"$VM_NAME\",\"description\":\"Migrated from VMware\",\"resources\":{\"num_sockets\":$NUM_CPUS,\"num_vcpus_per_socket\":1,\"memory_size_mib\":$MEMORY_MB,\"power_state\":\"ON\",\"machine_type\":\"PC\",\"boot_config\":{\"boot_type\":\"$BOOT_TYPE\"},\"disk_list\":[{\"data_source_reference\":{\"kind\":\"image\",\"uuid\":\"IMAGE_UUID\"},\"device_properties\":{\"device_type\":\"DISK\",\"disk_address\":{\"adapter_type\":\"SCSI\",\"device_index\":0}}}],\"nic_list\":[]},\"cluster_reference\":{\"kind\":\"cluster\",\"uuid\":\"$CLUSTER_UUID\"}},\"metadata\":{\"kind\":\"vm\"}}' \"$NX_URL/api/nutanix/v3/vms\""
