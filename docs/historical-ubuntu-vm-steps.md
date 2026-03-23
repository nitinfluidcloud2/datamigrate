  4. Ubuntu VM (ubuntumachine/README.md)

  Only documents creation of the Ubuntu VM in VMware (Ubuntu 24.04, IP 10.10.11.20, web-segment). The Migrate.md file only has ovftool download links.
  No driver injection or boot preparation was documented for Ubuntu.

  ---
  What was NOT done for Ubuntu (gap)

  Unlike the Windows and Alpine VMs, no source-side preparation commands were run or documented for the Ubuntu VM to make it bootable on Nutanix.
  Specifically missing:

  1. No VirtIO driver installation on the Ubuntu VM at source
  2. No initramfs rebuild with virtio modules
  3. No GRUB reconfiguration for non-VMware hypervisor
  4. No fstab UUID conversion documented
  5. No CD/ISO attached to install drivers

  What you would need to do for Ubuntu-to-Nutanix

  Based on the patterns from your Windows and Alpine migrations, the Ubuntu VM would need these steps at the VMware source before export:

  # 1. Install virtio drivers (Ubuntu has them in kernel already, but ensure they're in initramfs)
  sudo apt install -y linux-modules-extra-$(uname -r)

  # 2. Add virtio modules to initramfs
  echo -e "virtio_pci\nvirtio_blk\nvirtio_scsi\nvirtio_net\nsd_mod\nnvme" | sudo tee /etc/initramfs-tools/modules

  # 3. Rebuild initramfs
  sudo update-initramfs -u

  # 4. Ensure fstab uses UUIDs (not /dev/sdX)
  cat /etc/fstab  # verify UUID= entries

  # 5. Remove VMware tools (optional but recommended)
  sudo apt remove open-vm-tools

  # 6. Shutdown cleanly, then export from VMware
  sudo shutdown -h now

  Then on your workstation:
  govc export.ovf -vm "ubuntu-vm" ./ubuntu-vm-export
  qemu-img convert -f vmdk -O qcow2 ./ubuntu-vm-export/ubuntu-vm/ubuntu-vm-disk-0.vmdk ubuntu-vm.qcow2
  # Upload qcow2 to Nutanix via Prism Central Image Service

  Bottom line: The Ubuntu VM migration to Nutanix was not prepared or documented in this folder. The Windows and Alpine VMs had driver injection done,
  but Ubuntu was skipped.