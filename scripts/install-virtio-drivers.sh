#!/bin/bash
# Install virtio drivers into initramfs for AHV migration
# Run this on the SOURCE VM (VMware) BEFORE migration
# Supports: Ubuntu 20.04+, RHEL/CentOS 7+, Debian 11+

set -e

echo "=== Virtio Driver Installer for AHV Migration ==="

# Detect OS
if [ -f /etc/os-release ]; then
    . /etc/os-release
    OS=$ID
    VER=$VERSION_ID
else
    echo "ERROR: Cannot detect OS"
    exit 1
fi

echo "Detected: $OS $VER (kernel: $(uname -r))"

# Check if virtio modules exist in kernel
VIRTIO_SCSI=$(find /lib/modules/$(uname -r) -name "virtio_scsi*" 2>/dev/null | head -1)
if [ -z "$VIRTIO_SCSI" ]; then
    echo "ERROR: virtio_scsi module not found in kernel $(uname -r)"
    echo "You may need to install a newer kernel or virtio driver package"
    exit 1
fi
echo "Found virtio_scsi: $VIRTIO_SCSI"

MODULES="virtio_pci virtio_scsi virtio_blk virtio_net"

case $OS in
    ubuntu|debian)
        echo "Adding virtio modules to /etc/initramfs-tools/modules..."
        for mod in $MODULES; do
            if ! grep -q "^$mod$" /etc/initramfs-tools/modules 2>/dev/null; then
                echo "$mod" | sudo tee -a /etc/initramfs-tools/modules
            else
                echo "  $mod already present"
            fi
        done
        echo "Rebuilding initramfs..."
        sudo update-initramfs -u
        ;;

    rhel|centos|rocky|alma|fedora)
        echo "Adding virtio modules via dracut..."
        sudo mkdir -p /etc/dracut.conf.d
        echo "add_drivers+=\" virtio_pci virtio_scsi virtio_blk virtio_net \"" | sudo tee /etc/dracut.conf.d/virtio.conf
        echo "Rebuilding initramfs..."
        sudo dracut -f
        ;;

    sles|opensuse*)
        echo "Adding virtio modules via dracut..."
        sudo mkdir -p /etc/dracut.conf.d
        echo "add_drivers+=\" virtio_pci virtio_scsi virtio_blk virtio_net \"" | sudo tee /etc/dracut.conf.d/virtio.conf
        echo "Rebuilding initramfs..."
        sudo dracut -f
        ;;

    *)
        echo "WARNING: Unsupported OS '$OS'. Manually add these modules to initramfs:"
        echo "  $MODULES"
        exit 1
        ;;
esac

# Verify
echo ""
echo "=== Verification ==="
case $OS in
    ubuntu|debian)
        echo "Checking initramfs for virtio modules..."
        for mod in $MODULES; do
            if lsinitramfs /boot/initrd.img-$(uname -r) 2>/dev/null | grep -q "$mod"; then
                echo "  $mod: FOUND"
            else
                echo "  $mod: MISSING (may still work if built-in)"
            fi
        done
        ;;
    *)
        echo "Checking initramfs for virtio modules..."
        for mod in $MODULES; do
            if lsinitrd /boot/initramfs-$(uname -r).img 2>/dev/null | grep -q "$mod"; then
                echo "  $mod: FOUND"
            else
                echo "  $mod: MISSING (may still work if built-in)"
            fi
        done
        ;;
esac

echo ""
echo "Done! VM is ready for AHV migration."
