# Incremental Sync (T1) Test — Creating Verifiable Delta Changes

## Purpose

After the initial full sync (T0), we need to create visible changes on the source VM so we can verify that the incremental sync (T1) correctly captures and replicates only the changed blocks.

---

## Step 1: ASCII Banner Script

Install `figlet` and create a timestamped banner file that proves the T1 data landed on the destination.

```bash
# Install figlet for ASCII art banners
sudo apt install -y figlet

# Create the banner script
cat << 'EOF' > /home/ubuntuadmin/migration-marker.sh
#!/bin/bash
echo "=============================================" > /home/ubuntuadmin/MIGRATION_MARKER.txt
echo "  DATAMIGRATE INCREMENTAL SYNC TEST" >> /home/ubuntuadmin/MIGRATION_MARKER.txt
echo "=============================================" >> /home/ubuntuadmin/MIGRATION_MARKER.txt
figlet "T1 SYNC" >> /home/ubuntuadmin/MIGRATION_MARKER.txt
echo "" >> /home/ubuntuadmin/MIGRATION_MARKER.txt
echo "Created: $(date '+%Y-%m-%d %H:%M:%S %Z')" >> /home/ubuntuadmin/MIGRATION_MARKER.txt
echo "Hostname: $(hostname)" >> /home/ubuntuadmin/MIGRATION_MARKER.txt
echo "Kernel: $(uname -r)" >> /home/ubuntuadmin/MIGRATION_MARKER.txt
figlet "$(date '+%H:%M')" >> /home/ubuntuadmin/MIGRATION_MARKER.txt
echo "=============================================" >> /home/ubuntuadmin/MIGRATION_MARKER.txt
cat /home/ubuntuadmin/MIGRATION_MARKER.txt
EOF
chmod +x /home/ubuntuadmin/migration-marker.sh

# Run it
./migration-marker.sh
```

Expected output:

```
=============================================
  DATAMIGRATE INCREMENTAL SYNC TEST
=============================================
 _____ _   ______   ___   _  ____
|_   _/ | / ___\ \ / / \ | |/ ___|
  | | | | \___ \\ V /|  \| | |
  | | | |  ___) || | | |\  | |___
  |_| |_| |____/ |_| |_| \_|\____|

Created: 2026-03-22 14:30:00 UTC
Hostname: ubuntu-vm
Kernel: 6.8.0-51-generic
 _ _  _   _____  ___
/ | | | |___ / / _ \
| | |_| | |_ \| | | |
| |  _  |___) | |_| |
|_|_| |_|____/ \___/
=============================================
```

---

## Step 2: Install Verifiable Tools

Install packages that can be checked at the destination to confirm the T1 delta was applied.

```bash
# Install a few small packages we can check at destination
sudo apt install -y htop neofetch tree cowsay

# Leave proof they're installed
neofetch > /home/ubuntuadmin/neofetch-output.txt
cowsay "Migrated by datamigrate" > /home/ubuntuadmin/cowsay-output.txt
tree /home/ubuntuadmin/ > /home/ubuntuadmin/tree-output.txt
```

Approximate disk usage: ~50 MB in `/usr` for packages + output files in home directory.

---

## Step 3: Download a 100MB Test File

Download a known test file and create a checksum for integrity verification.

```bash
# Generate a 100MB random file (incompressible — guarantees real block changes)
dd if=/dev/urandom of=/home/ubuntuadmin/testfile-100MB.bin bs=1M count=100 status=progress

# Create checksum for verification at destination
sha256sum /home/ubuntuadmin/testfile-100MB.bin > /home/ubuntuadmin/testfile-100MB.sha256
cat /home/ubuntuadmin/testfile-100MB.sha256
```

---

## Total Expected Delta

| Change | Approximate Size |
|--------|-----------------|
| figlet, htop, neofetch, tree, cowsay packages | ~50 MB |
| 100MB test file | 100 MB |
| Banner + output files | < 1 MB |
| **Total new data** | **~150 MB** |

This is enough to clearly see CBT picking up changed blocks during T1 incremental sync.

---

## Verification at Destination (After T1 Sync)

Run these checks on the AHV VM after the incremental sync completes:

```bash
# 1. Check the ASCII banner (confirms file-level changes replicated)
cat /home/ubuntuadmin/MIGRATION_MARKER.txt

# 2. Check installed tools (confirms package installs replicated)
which htop neofetch tree cowsay
cowsay "I survived migration"

# 3. Verify 100MB file integrity (confirms large block-level data is intact)
sha256sum -c /home/ubuntuadmin/testfile-100MB.sha256

# 4. Check neofetch output matches source
cat /home/ubuntuadmin/neofetch-output.txt
```

### Expected Results

| Check | Pass Criteria |
|-------|--------------|
| MIGRATION_MARKER.txt | File exists with correct timestamp |
| `which htop` | Returns `/usr/bin/htop` |
| `which cowsay` | Returns `/usr/games/cowsay` |
| sha256sum check | `testfile-100MB.bin: OK` |
| neofetch output | Matches source VM output |

---

## Running the Incremental Sync

After creating all the changes above on the source VM:

```bash
# From the migration host
./datamigrate migrate sync --plan configs/ubuntu-vm-plan.yaml
```

This will:
1. Create a new snapshot on the source VM
2. Query VMware CBT for changed blocks since the T0 snapshot
3. Transfer only the changed blocks (~150 MB) via iSCSI to the Volume Group
4. Update the migration state in BoltDB
