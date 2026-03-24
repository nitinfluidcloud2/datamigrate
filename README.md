# datamigrate

VMware vSphere to Nutanix AHV VM migration tool using block-level replication with near-zero downtime.

## How it works

1. Takes a VMware snapshot and reads disk blocks via NFC export
2. Converts to local raw disk image, then compressed qcow2
3. Uploads qcow2 to Nutanix Image Store
4. Subsequent syncs transfer only changed blocks (CBT delta) — patches the local raw file
5. Final cutover: last delta sync, shutdown source, create VM on Nutanix AHV, power on

The local raw file is always a complete, bootable disk after every sync. You can create a VM and test-boot at any stage.

## How Data Flows

```
VMware snapshot ──NFC export──▶ streamOptimized VMDK (compressed)
                                      │
                                Save to temp file
                                      │
                                qemu-img convert vmdk → raw (disk-0.raw)
                                      │
                                qemu-img convert raw → qcow2 (compressed)
                                      │
                                Upload qcow2 ──HTTP PUT──▶ Nutanix Image Store
                                      │
                                createvm (from image UUID in state DB)
```

**T0 (Full Sync):** NFC exports the entire disk → saves as VMDK → converts to raw file (the "repository") → converts to qcow2 → uploads to Nutanix.

**T1..TN (Incremental):** CBT identifies changed blocks → NFC re-reads full disk → only changed blocks patched into the raw file → converts to qcow2 → re-uploads.

**Correctly handles:** Thin-provisioned disks, snapshot chains, UEFI and Legacy BIOS boot. Pure Go, no VDDK dependency.

**Tested on:**
| OS | Boot | Disk | Snapshots | Result |
|---|---|---|---|---|
| Ubuntu 24.04 | UEFI | 50 GB thin | Yes | ✅ T0 + T1 + boot |
| Windows | UEFI | 25 GB | - | ✅ T0 + T1 + boot |
| Alpine Linux | UEFI | - | Many | ✅ T0 + boot |

## Quick Start

```bash
# 1. Build
make build-linux

# 2. Deploy to migration host
scp bin/datamigrate-linux-amd64 user@migration-host:~/datamigrate

# 3. Discover VMs on vCenter
./datamigrate discover --vcenter <vcenter-url> --username <user>

# 4. Create migration plan
./datamigrate plan create --vm <vm-name> --transport repository

# 5. Run T0 full sync (NFC → VMDK → raw → qcow2 → upload to Nutanix)
./datamigrate migrate start --plan configs/<vm-name>-plan.yaml

# 6. Create VM on Nutanix AHV (reads image UUID from state DB)
./datamigrate createvm --plan configs/<vm-name>-plan.yaml --boot-type UEFI --power-on

# 7. Verify VM boots in Prism Central

# 8. (Optional) Run incremental syncs, then recreate VM with latest data
./datamigrate migrate sync --plan configs/<vm-name>-plan.yaml
./datamigrate createvm --plan configs/<vm-name>-plan.yaml --boot-type UEFI --power-on

# 9. Cleanup when done
./datamigrate cleanup --plan configs/<vm-name>-plan.yaml
```

## Migration Lifecycle

### Full picture: T0 → T1..TN → Cutover

```
T0 (Full Sync)           T1..TN (Incremental)         Cutover
─────────────────        ─────────────────────         ────────────────────
migrate start            migrate sync (repeat)         createvm --power-on

┌─────────────┐          ┌─────────────┐               ┌─────────────┐
│ VMware VM   │          │ VMware VM   │               │ VMware VM   │
│ (running)   │          │ (running)   │               │ (shut down) │
│             │          │             │               │             │
│ Snapshot T0 │          │ Snapshot TN │               │ Final snap  │
└──────┬──────┘          └──────┬──────┘               └──────┬──────┘
       │ NFC export              │ NFC + CBT delta             │ last delta
       │ full disk               │ patch raw file              │
       ▼                         ▼                             ▼
┌─────────────┐          ┌─────────────┐               ┌─────────────┐
│ Local raw   │          │ Local raw   │               │ Nutanix VM  │
│ + qcow2     │          │ (patched)   │               │ (powered on)│
│ + image     │          │ + new qcow2 │               │ from image  │
└─────────────┘          └─────────────┘               └─────────────┘

Source VM: RUNNING        Source VM: RUNNING             Source VM: OFF
Downtime:  ZERO           Downtime:  ZERO                Downtime: 5-15 min
```

### Where does data live?

**On migration host:**
- `disk-0.raw` — local raw disk image (the repository), patched in-place each sync
- `disk-0.qcow2` — compressed image for upload, regenerated each sync
- Only one copy of each — no accumulation over T0..TN

**On Nutanix:**
- **Image Store** — each sync uploads a new image named `<vm-name>-disk-<key>-<datetime>`
- `createvm` reads the latest image UUID from state DB and creates a VM from it

### Testing a migration before cutover

After T0 or any sync, create a test VM:

```bash
./datamigrate createvm --plan configs/<vm-name>-plan.yaml --boot-type UEFI --power-on
```

Delete the test VM from Prism before the next sync (old image stays, new one is created on next sync).

## Prerequisites

### Where to run

The tool can run from **any machine** with network access to both VMware and Nutanix — your laptop, a cloud VM, or a datacenter VM. No CVM access or special Nutanix infrastructure needed.

For best performance, run it from a machine with fast network to both VMware and Nutanix (e.g., a VM in the same datacenter).

### Network Requirements

```
  Migration Host (anywhere — laptop, cloud VM, datacenter VM)
  ┌──────────────┐
  │ datamigrate  │──── HTTPS (443) ────► vCenter API (discovery, snapshots, CBT)
  │              │
  │              │──── TCP (902) ───────► ESXi hosts (NFC disk export)
  │              │
  │              │──── HTTPS (9440) ────► Nutanix Prism Central (image upload, VM creation)
  └──────────────┘
```

| Destination | Port | Protocol | Purpose |
|---|---|---|---|
| vCenter | 443 | HTTPS | VM discovery, snapshots, CBT queries |
| ESXi hosts | 902 | TCP | NFC data transfer (disk export) |
| Prism Central | 9440 | HTTPS | Image upload, VM creation |

**Port 902** is ESXi's standard NFC (Network File Copy) port — open by default on all ESXi hosts for vSphere operations (vMotion, NFC export, file transfers).

No CVM access, no iSCSI ports, no special Nutanix infrastructure required.

### System requirements

| Requirement | |
|---|---|
| **OS** | Linux or macOS |
| **CPU** | 2+ vCPUs |
| **RAM** | 512 MB per VM |
| **Disk** | ~1.2x VM disk size (raw + qcow2) |
| **Dependencies** | `qemu-img` (`yum install qemu-img` or `brew install qemu`) |
| **Go** | 1.24+ (build only) |

### Memory usage and performance

**RAM is NOT a bottleneck.** The tool streams blocks through a small in-memory buffer — it does NOT load the entire disk into RAM.

The NFC stream is saved to disk and converted — not held in memory.

**Memory per VM migration:**

| Component | Memory |
|---|---|
| Pipeline buffer (16 channel slots × 1 MB block) | ~16 MB |
| Writer workers (4 workers × 1 MB block each) | ~4 MB |
| Go runtime, HTTP clients, state DB | ~50 MB |
| **Total per VM** | **~70 MB** |

For parallel migrations:

| VMs in parallel | RAM needed | Recommended |
|---|---|---|
| 1 | ~70 MB | 512 MB |
| 5 | ~350 MB | 1 GB |
| 10 | ~700 MB | 2 GB |
| 20 | ~1.4 GB | 4 GB |

**4 GB RAM is enough for 20+ parallel VM migrations.**

### Disk sizing

Each VM being migrated needs local disk for a raw file + qcow2 file:

```
Disk per VM  =  VM disk size (raw, sparse)  +  compressed qcow2 (~10-30% of raw)
             ≈  1.2x the VM's disk size
```

| VMs | Avg VM disk | Disk needed |
|---|---|---|
| 1 VM | 50 GB | ~60 GB |
| 1 VM | 100 GB | ~120 GB |
| 1 VM | 500 GB | ~600 GB |

Note: The raw file is sparse — actual disk usage depends on how much data the VM has. A 50 GB disk with 6 GB of data uses ~6 GB on disk (+ ~6 GB qcow2 = ~12 GB total).

```bash
# Example: mount a dedicated disk for migration staging
sudo mkfs.ext4 /dev/sdb
sudo mkdir -p /data
sudo mount /dev/sdb /data
# Then set staging directory in the plan YAML:
# staging:
#   directory: /data/datamigrate
```

### Nutanix prerequisites

The tool uses the Nutanix Prism Central v3 API. A few things must be configured on the Nutanix side before you start.

#### Required setup (one-time, by Nutanix admin)

**1. Prism Central API access**

The tool needs a user account on Prism Central with permissions to:

| API Operation | Prism Central Permission |
|---|---|
| Create/upload Images | Image Admin or Cluster Admin |
| Create/power on VMs | VM Admin or Cluster Admin |
| List subnets/clusters | Viewer (any role) |

A user with **Cluster Admin** role covers everything. For least-privilege, create a service account with only the roles above.

**3. Target cluster UUID**

You need the UUID of the Nutanix cluster where VMs will be created. Find it in:

```
Prism Central → Hardware → Clusters → click cluster → copy UUID from URL
```

Or via API:
```bash
curl -sk -u admin:password \
  https://prism.example.com:9440/api/nutanix/v3/clusters/list \
  -X POST -d '{"kind":"cluster"}' | python3 -m json.tool | grep uuid
```

Put this in your config:
```yaml
target:
  cluster_uuid: "0005b6f1-8f91-01f0-0000-000000012345"
```

**4. Target subnet UUID(s)**

You need subnet UUIDs for network mapping — which Nutanix subnet should the migrated VM's NIC connect to.

```bash
# List available subnets
curl -sk -u admin:password \
  https://prism.example.com:9440/api/nutanix/v3/subnets/list \
  -X POST -d '{"kind":"subnet"}' | python3 -m json.tool | grep -E '"name"|"uuid"'
```

Use the UUID in plan creation:
```bash
datamigrate plan create --config config.yaml --vm web-server-01 \
  --network-map "subnet-uuid-here"
```

#### What the tool creates automatically on Nutanix

You do NOT need to pre-create these — the tool handles them:

| Resource | Created when | Named | Cleaned up |
|---|---|---|---|
| **Image** | `migrate start` / `migrate sync` | `<vm-name>-disk-<N>-<datetime>` | Manually via Prism |
| **Target VM** | `createvm` | `<vm-name>-ahv` | Manually if unwanted |

#### Nutanix checklist

```
[ ] Prism Central user account with Cluster Admin (or equivalent) role
[ ] Cluster UUID noted
[ ] Target subnet UUID(s) noted for network mapping
[ ] Port 9440 accessible from datamigrate machine to Prism Central
[ ] Enough storage capacity on Nutanix for the migrated VM images
```

### VMware prerequisites

No special setup is required on VMware. The tool only needs:

```
[ ] vCenter user account with these permissions:
    - VirtualMachine.State.CreateSnapshot
    - VirtualMachine.State.RemoveSnapshot
    - VirtualMachine.Config.ChangeTracking (to enable CBT)
    - VirtualMachine.Provisioning.GetVmFiles (to read disk blocks via NFC)
    - VirtualMachine.Interact.PowerOff (only for cutover with --shutdown-source)
[ ] Port 443 accessible from datamigrate machine to vCenter
[ ] Port 902 accessible from datamigrate machine to ESXi hosts (NFC data transfer)
```

A user with **Administrator** role works. For least-privilege, create a role with only the permissions above.

CBT (Changed Block Tracking) is enabled automatically by the tool — you do NOT need to enable it manually.

### Network access

| Destination | Port | Required for |
|---|---|---|
| VMware vCenter | 443 (HTTPS) | All commands |
| VMware ESXi hosts | 902 (NFC) | Block reading via NFC lease |
| Nutanix Prism Central | 9440 (HTTPS) | All commands |
| Nutanix Data Services IP | 3260 (iSCSI) | iSCSI transport only |

### Software dependencies

**For iSCSI transport (production):**
- `open-iscsi` — provides `iscsiadm` for iSCSI block device access
- `go` 1.24+ — only needed to build the binary; not needed at runtime

**For stream transport (testing from Mac):**
- `qemu-img` — converts NFC VMDK to qcow2 (`brew install qemu` on Mac)
- `go` 1.24+ — build only

**For Docker (iSCSI from Mac):**
- `docker` — the included Dockerfile bundles open-iscsi + qemu-img
- Run with `--privileged` flag for iSCSI block device access

### Linux setup (RHEL/CentOS/Rocky)

```bash
# Install iSCSI initiator
sudo yum install -y iscsi-initiator-utils
sudo systemctl enable --now iscsid

# Install Go (build only — skip if using pre-built binary)
sudo yum install -y golang

# Install qemu-img (only if using --transport image)
sudo yum install -y qemu-img

# Build datamigrate
git clone <repo-url> && cd datamigrate
make build

# Copy binary to PATH
sudo cp bin/datamigrate /usr/local/bin/
```

### Linux setup (Ubuntu/Debian)

```bash
# Install iSCSI initiator
sudo apt-get update
sudo apt-get install -y open-iscsi
sudo systemctl enable --now iscsid

# Install Go (build only — skip if using pre-built binary)
sudo apt-get install -y golang-go

# Install qemu-img (only if using --transport image)
sudo apt-get install -y qemu-utils

# Build datamigrate
git clone <repo-url> && cd datamigrate
make build

# Copy binary to PATH
sudo cp bin/datamigrate /usr/local/bin/
```

### Docker setup (iSCSI from Mac)

If you want to test iSCSI transport from macOS, use the included Dockerfile. Docker on Mac runs a LinuxKit VM with a real Linux kernel, so `iscsiadm` works inside a privileged container.

```bash
# Build the image (includes open-iscsi + qemu-img)
docker build -t datamigrate .

# Create plan with iSCSI transport
go run ./cmd/datamigrate plan create --vm my-vm --creds-dir configs --transport iscsi

# Run migration (--privileged needed for iSCSI block device access)
docker run --rm -it --privileged \
  -v $(pwd)/configs:/migration/configs \
  datamigrate migrate start --plan configs/my-vm-plan.yaml

# If Nutanix data services IP is not reachable from Docker, add --network host:
docker run --rm -it --privileged --network host \
  -v $(pwd)/configs:/migration/configs \
  datamigrate migrate start --plan configs/my-vm-plan.yaml
```

The Dockerfile bundles: `datamigrate` binary + `open-iscsi` + `qemu-img` + `iscsid` (auto-started).

### Verify setup

```bash
# Check iSCSI is ready
sudo iscsiadm --version

# Check network access
curl -sk https://vcenter.example.com/rest/com/vmware/cis/session   # should return 401
curl -sk https://prism.example.com:9440/api/nutanix/v3/clusters/list  # should return 401

# Check datamigrate is installed
datamigrate --help
```

## Installation

### Build from source (on your dev machine)

```bash
git clone <repo-url> && cd datamigrate

# Build for current platform
make build                # → bin/datamigrate

# Build for Linux (to deploy on VMware datacenter VM)
make build-linux          # → bin/datamigrate-linux-amd64

# Build for macOS (Intel + Apple Silicon)
make build-mac            # → bin/datamigrate-darwin-amd64
                          # → bin/datamigrate-darwin-arm64

# Build all platforms at once
make build-all

# Or install to $GOPATH/bin
make install
```

### Deploy to Linux VM

```bash
# Copy the binary + sample config files to the Linux VM
scp bin/datamigrate-linux-amd64    user@migration-vm:/usr/local/bin/datamigrate
scp configs/example.yaml           user@migration-vm:~/migration/config.yaml
scp configs/vmware.creds.example   user@migration-vm:~/migration/vmware.creds
scp configs/nutanix.creds.example  user@migration-vm:~/migration/nutanix.creds

# On the Linux VM
chmod +x /usr/local/bin/datamigrate
chmod 600 ~/migration/vmware.creds ~/migration/nutanix.creds

# Edit the files with your real values
vi ~/migration/config.yaml         # set vcenter, prism_central, cluster_uuid
vi ~/migration/vmware.creds        # set username, password, host
vi ~/migration/nutanix.creds       # set username, password, host

datamigrate --help
```

No Go installation needed on the target machine — the binary is self-contained.

Sample files included in the repo:

| File | Purpose |
|---|---|
| `configs/example.yaml` | Config template — connection hosts, cluster UUID, staging dir |
| `configs/vmware.creds.example` | VMware credentials template — username, password, host |
| `configs/nutanix.creds.example` | Nutanix credentials template — username, password, host |

## How to run (end-to-end)

This section walks through the complete process from a fresh Linux VM to a migrated VM on Nutanix.

### Step 1: Prepare the Linux VM

```bash
# Install iSCSI (required for default transport)
sudo yum install -y iscsi-initiator-utils     # RHEL/CentOS
# OR
sudo apt-get install -y open-iscsi            # Ubuntu/Debian

sudo systemctl enable --now iscsid
```

### Step 2: Deploy the binary

```bash
# On your dev machine — build the Linux binary
make build-linux

# Copy to the Linux VM in the VMware datacenter
scp bin/datamigrate-linux-amd64 user@migration-vm:/usr/local/bin/datamigrate
```

```bash
# On the Linux VM
chmod +x /usr/local/bin/datamigrate
datamigrate --help
```

### Step 3: Create config and credentials files

```bash
mkdir -p ~/migration
cd ~/migration
```

**3a. Copy the example files from the repo** (or create them manually):

```bash
# If you cloned the repo, copy the templates
cp configs/example.yaml        ~/migration/config.yaml
cp configs/vmware.creds.example  ~/migration/vmware.creds
cp configs/nutanix.creds.example ~/migration/nutanix.creds

# Or if you only have the binary, create them from scratch (see below)
```

**3b. Edit config.yaml** — connection settings, no passwords needed here:

```yaml
# ~/migration/config.yaml
source:
  vcenter: "vcenter.example.com"
  insecure: true

target:
  prism_central: "prism.example.com"
  insecure: true
  cluster_uuid: "0005b6f1-8f91-01f0-0000-000000012345"

staging:
  directory: "/tmp/datamigrate"
```

**3c. Edit vmware.creds** — VMware credentials (kept separate from config):

```
# ~/migration/vmware.creds
# VMware vCenter credentials
# This file is git-ignored and should have permissions 0600

username = administrator@vsphere.local
password = "my-vcenter-password"
host = vcenter.example.com
```

**3d. Edit nutanix.creds** — Nutanix credentials (kept separate from config):

```
# ~/migration/nutanix.creds
# Nutanix Prism Central credentials
# This file is git-ignored and should have permissions 0600

username = admin
password = "my-prism-password"
host = prism.example.com
```

**3e. Lock down permissions** — only your user can read the creds files:

```bash
chmod 600 ~/migration/vmware.creds ~/migration/nutanix.creds
```

**3f. Verify the file layout:**

```bash
ls -la ~/migration/

# Expected:
# -rw-r--r--  config.yaml              ← safe, no passwords
# -rw-------  vmware.creds             ← restricted, has VMware password
# -rw-------  nutanix.creds            ← restricted, has Nutanix password
```

The tool automatically picks up `vmware.creds` and `nutanix.creds` from the **same directory** as the config file you pass with `--config`. No extra flags needed — just place them next to `config.yaml`.

**How credentials are resolved** (highest priority wins):
1. Environment variables (`DATAMIGRATE_SOURCE_PASSWORD`, etc.)
2. Creds files (`vmware.creds`, `nutanix.creds` next to config/plan file)
3. Values in config.yaml / plan.yaml

### Step 4: Verify connectivity

```bash
datamigrate validate --config ~/migration/config.yaml

# Expected output:
# Validating VMware vCenter connection... OK
# Validating Nutanix Prism Central connection... OK
# All validations passed.
```

If validation fails, check:
- Network access: vCenter (port 443), Prism Central (port 9440), Data Services IP (port 3260)
- Credentials in vmware.creds / nutanix.creds
- iSCSI Data Services IP is configured in Prism Element

### Step 5: Discover VMs

```bash
datamigrate discover --vcenter vcenter.example.com \
  --username administrator@vsphere.local \
  --password "my-vcenter-password"

# Output:
# NAME              MOREF   POWER      CPUs  MEMORY   DISKS         NICs
# web-server-01     vm-42   poweredOn  4     8.0 GB   2 (150.0 GB)  1
# db-server         vm-55   poweredOn  8     32.0 GB  3 (500.0 GB)  2
# app-server        vm-61   poweredOn  2     4.0 GB   1 (50.0 GB)   1
```

Note the VM name you want to migrate.

### Step 6: Create a migration plan

Before creating the plan, you need the **network mapping**. Storage mapping is optional.

#### 6a. Find the network mapping (`--network-map`) — REQUIRED

This tells the tool which Nutanix subnet to connect the migrated VM's NIC to.

**For single-NIC VMs** (most common), just pass the Nutanix subnet UUID:

```bash
--network-map "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
```

That's it. No VMware network name needed. The tool creates one NIC on the target VM connected to that subnet.

**For multi-NIC VMs**, use `source-network:subnet-uuid` format to map each NIC to the correct subnet:

```bash
--network-map "Production-Net:subnet-uuid-1" \
--network-map "Backup-Net:subnet-uuid-2"
```

The source network name (left of `:`) is just a label for your reference — it helps you track "Production NIC goes to subnet X, Backup NIC goes to subnet Y." The tool creates one NIC per `--network-map` entry.

**How to find the Nutanix subnet UUID:**

```bash
# Via API:
curl -sk -u admin:password \
  https://prism.example.com:9440/api/nutanix/v3/subnets/list \
  -X POST -H "Content-Type: application/json" \
  -d '{"kind":"subnet","length":50}' | python3 -m json.tool

# Look for:
# "name": "VLAN100-Subnet"
# "uuid": "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
```

```bash
# Or in Prism Central UI:
# Network & Security → Subnets → find your subnet → copy UUID from browser URL
```

**Summary:**

```
Single NIC:  --network-map "subnet-uuid"
Multi NIC:   --network-map "NIC1-label:subnet-uuid-1" --network-map "NIC2-label:subnet-uuid-2"
```

#### 6b. Storage mapping (`--storage-map`) — OPTIONAL, skip it

**You don't need this flag.** The tool reads blocks directly from the VM via CBT — it doesn't care which VMware datastore the VMDK lives on.

- **iSCSI transport:** Creates a Volume Group on the cluster's default storage container automatically
- **Image transport:** Uploads to the Nutanix Image Service automatically

The `--storage-map` flag exists for future use (targeting a specific Nutanix container). Skip it.

#### 6c. Create the plan

```bash
# Production (Linux host with iSCSI):
datamigrate plan create \
  --creds-dir ~/migration \
  --vm web-server-01 \
  --transport iscsi \
  --network-map "subnet-uuid"

# Testing from Mac (stream via qcow2 upload):
datamigrate plan create \
  --creds-dir ~/migration \
  --vm web-server-01 \
  --transport stream \
  --network-map "subnet-uuid"

# Docker on Mac (iSCSI via Docker):
datamigrate plan create \
  --creds-dir configs \
  --vm web-server-01 \
  --transport iscsi

# Output:
# Migration plan created: configs/web-server-01-plan.yaml
# VM: web-server-01 (moref: vm-42)
# CPUs: 4, Memory: 8192 MB
# Disks: 2, NICs: 1
#   disk-2000: 100.0 GB ([datastore1] web-server-01/web-server-01.vmdk) [datastore1]
#   disk-2001: 50.0 GB ([datastore1] web-server-01/web-server-01_1.vmdk) [datastore1]
```

**Transport mode selection:**

| Scenario | Use `--transport` |
|---|---|
| Linux host near VMware + Nutanix | `iscsi` (fastest, production) |
| Mac for testing | `stream` (needs qemu-img) |
| Docker on Mac for iSCSI testing | `iscsi` (run inside Docker with `--privileged`) |
| No iSCSI, no qemu-img | `image` (legacy, needs local disk) |

Review the plan:

```bash
datamigrate plan show ~/migration/web-server-01-plan.yaml

# Shows full plan with source/target config (passwords redacted),
# network mappings, and target VM spec (CPU/RAM)
```

### Step 7: Run full sync (T0)

This is the longest step — copies the entire VM disk to Nutanix.

```bash
datamigrate migrate start --plan ~/migration/web-server-01-plan.yaml

# Output (live progress every 5 seconds):
# Migration initialized: web-server-01-migration
# VM: web-server-01, Disks: 2
# Starting full sync (T0)...
#   [disk-0] 45.2% (45.2 GB / 100.0 GB) @ 125.3 MB/s ETA: 7m22s
#   [disk-0] 100.0% (100.0 GB / 100.0 GB) @ 118.7 MB/s ETA: 0s
#   [disk-1] 100.0% (50.0 GB / 50.0 GB) @ 130.1 MB/s ETA: 0s
# Full sync complete. Run 'datamigrate migrate sync' for incremental syncs.
```

At this point, a Volume Group named `datamigrate-web-server-01` appears in Nutanix Prism with the VM's disks. The source VM keeps running — no downtime.

### Step 8: Run incremental syncs (T1..TN)

Run this periodically (daily, hourly, or whenever you want). Each run transfers only the blocks that changed since the last sync.

```bash
# Run as many times as needed — each sync gets smaller and faster
datamigrate migrate sync --plan ~/migration/web-server-01-plan.yaml

# Output:
# Starting incremental sync...
#   [disk-0] 100.0% (4.8 GB / 4.8 GB) @ 145.2 MB/s ETA: 0s
#   [disk-1] 100.0% (200.0 MB / 200.0 MB) @ 150.0 MB/s ETA: 0s
# Incremental sync complete.
```

Check status anytime:

```bash
datamigrate migrate status --plan ~/migration/web-server-01-plan.yaml
```

Keep syncing until deltas are small (MB, not GB) — this minimizes cutover downtime.

### Step 9: Cutover (go live on Nutanix)

When ready for the final switch:

```bash
datamigrate cutover --plan ~/migration/web-server-01-plan.yaml --shutdown-source

# Output:
# Starting cutover...
# Cutover complete! VM is now running on Nutanix AHV.
```

This does: final sync → shutdown source VM → last sync → create VM on Nutanix → power on. Downtime: **2-10 minutes**.

Without `--shutdown-source`: creates the VM on Nutanix without shutting down the source — useful for **testing** before committing.

### Step 10: Cleanup

After verifying the VM works on Nutanix:

```bash
datamigrate cleanup --plan ~/migration/web-server-01-plan.yaml

# Output:
# Removing VMware snapshots... OK
# Removing staging files... OK
# Removing migration state... OK
# Cleanup complete.
```

### Complete command sequence (copy-paste ready)

```bash
# ── ONE-TIME SETUP ──
sudo yum install -y iscsi-initiator-utils && sudo systemctl enable --now iscsid
mkdir -p ~/migration

# Copy sample creds files and edit with real credentials
cp configs/example.yaml          ~/migration/config.yaml
cp configs/vmware.creds.example  ~/migration/vmware.creds
cp configs/nutanix.creds.example ~/migration/nutanix.creds
vi ~/migration/config.yaml        # set vcenter, prism_central, cluster_uuid
vi ~/migration/vmware.creds       # set username, password, host
vi ~/migration/nutanix.creds      # set username, password, host
chmod 600 ~/migration/vmware.creds ~/migration/nutanix.creds

# ── PER-VM MIGRATION ──
datamigrate validate  --config ~/migration/config.yaml
datamigrate discover  --vcenter vcenter.example.com --username admin --password secret
datamigrate plan create --config ~/migration/config.yaml --vm web-server-01 \
  --network-map "subnet-uuid" \
  --output ~/migration/web-server-01-plan.yaml

datamigrate migrate start  --plan ~/migration/web-server-01-plan.yaml    # T0 full sync
datamigrate migrate sync   --plan ~/migration/web-server-01-plan.yaml    # T1 delta
datamigrate migrate sync   --plan ~/migration/web-server-01-plan.yaml    # T2 delta
datamigrate migrate sync   --plan ~/migration/web-server-01-plan.yaml    # T3 delta
datamigrate migrate status --plan ~/migration/web-server-01-plan.yaml    # check progress

datamigrate cutover --plan ~/migration/web-server-01-plan.yaml --shutdown-source  # go live
datamigrate cleanup --plan ~/migration/web-server-01-plan.yaml                    # cleanup
```

## Quick start

```bash
# 1. Discover VMs on vCenter
datamigrate discover --vcenter vcenter.example.com --username admin --password secret

# 2. Create a migration plan
datamigrate plan create \
  --config configs/config.yaml \
  --vm web-server-01 \
  --network-map "subnet-uuid"

# 3. Validate connectivity
datamigrate validate --config configs/config.yaml

# 4. Start full sync (T0)
datamigrate migrate start --plan web-server-01-plan.yaml

# 5. Run incremental syncs (T1..TN) — repeat as needed
datamigrate migrate sync --plan web-server-01-plan.yaml

# 6. Check status
datamigrate migrate status --plan web-server-01-plan.yaml

# 7. Final cutover
datamigrate cutover --plan web-server-01-plan.yaml --shutdown-source

# 8. Cleanup snapshots and temp artifacts
datamigrate cleanup --plan web-server-01-plan.yaml
```

## Step-by-step migration guide

This tool is **not** a continuously running daemon. You run each command manually (or via cron/script) when you're ready. The tool tracks all state between runs automatically.

### How block tracking works across runs

VMware returns a `changeID` after every CBT (Changed Block Tracking) query. This is like a bookmark — it tells VMware "next time, give me only blocks that changed after this point." The tool saves this changeID to a local state DB after each sync, so even if days pass between runs, VMware knows exactly which blocks changed.

```
T0: QueryChangedBlocks(changeID="*")         → ALL blocks      + changeID="52 83 ... 0a"
T1: QueryChangedBlocks(changeID="52 83...0a") → changed blocks + changeID="7f a1 ... 3c"
T2: QueryChangedBlocks(changeID="7f a1...3c") → changed blocks + changeID="c4 11 ... 8f"
```

### DAY 1: Setup (one-time)

```bash
# Discover VMs on vCenter — pick the one you want to migrate
datamigrate discover --vcenter vcenter.example.com --username admin --password secret

# Output:
# NAME              MOREF   POWER      CPUs  MEMORY   DISKS         NICs
# web-server-01     vm-42   poweredOn  4     8.0 GB   2 (150.0 GB)  1
# db-server         vm-55   poweredOn  8     32.0 GB  3 (500.0 GB)  2

# Create a migration plan for the VM
datamigrate plan create \
  --config configs/config.yaml \
  --vm web-server-01 \
  --network-map "subnet-uuid"

# Output:
# Migration plan created: web-server-01-plan.yaml
# VM: web-server-01 (moref: vm-42)
# CPUs: 4, Memory: 8192 MB
# Disks: 2, NICs: 1

# Validate connectivity to both endpoints
datamigrate validate --config configs/config.yaml

# Output:
# Validating VMware vCenter connection... OK
# Validating Nutanix Prism Central connection... OK
# All validations passed.
```

### DAY 1: T0 — Full sync (run once, takes hours for large VMs)

```bash
datamigrate migrate start --plan web-server-01-plan.yaml
```

What happens internally:
1. Snapshot VM on VMware
2. Enable CBT (Changed Block Tracking)
3. `QueryChangedBlocks(changeID="*")` — gets ALL blocks
4. Create Volume Group on Nutanix named `datamigrate-web-server-01`
5. Read all blocks from VMware, write to Volume Group via iSCSI
6. Save `changeID="52 83 ... 0a"` to state.db
7. Remove snapshot
8. State: `CREATED` → `FULL_SYNC` → `SYNCING`

Output while running:
```
Migration initialized: web-server-01-migration
VM: web-server-01, Disks: 2
Starting full sync (T0)...
  [disk-0] 45.2% (45.2 GB / 100.0 GB) @ 125.3 MB/s ETA: 7m22s
  [disk-0] 100.0% (100.0 GB / 100.0 GB) @ 118.7 MB/s ETA: 0s
  [disk-1] 100.0% (50.0 GB / 50.0 GB) @ 130.1 MB/s ETA: 0s
Full sync complete. Run 'datamigrate migrate sync' for incremental syncs.
```

Check status anytime:
```bash
datamigrate migrate status --plan web-server-01-plan.yaml

# Output:
# Migration: web-server-01-migration
# VM: web-server-01
# Status: SYNCING
# Transport: iscsi
# Sync Count: 1
# Volume Group: vg-uuid-abc123
#
# DISK  FILE                CAPACITY   COPIED     IMAGE UUID  LAST SYNC
# 0     [web-server-01].vmdk  100.0 GB   100.0 GB              14:00:05
# 1     [web-server-01_1].vmdk  50.0 GB    50.0 GB              14:15:22
```

### DAY 2: T1 — First incremental sync

```bash
datamigrate migrate sync --plan web-server-01-plan.yaml
```

What happens internally:
1. Reads saved `changeID="52 83 ... 0a"` from state.db
2. Snapshot VM
3. `QueryChangedBlocks(changeID="52 83 ... 0a")` — gets ONLY blocks changed since T0
4. Writes ONLY those blocks to the same Volume Group via iSCSI (e.g., 5 GB not 100 GB)
5. Saves new `changeID="7f a1 ... 3c"` to state.db
6. Remove snapshot
7. State: `SYNCING` → `CUTOVER_READY`

```
Starting incremental sync...
  [disk-0] 100.0% (4.8 GB / 4.8 GB) @ 145.2 MB/s ETA: 0s
  [disk-1] 100.0% (200.0 MB / 200.0 MB) @ 150.0 MB/s ETA: 0s
Incremental sync complete.
```

### DAY 3: T2, T3... TN — More incremental syncs

```bash
# Run as many times as needed — each sync gets smaller and faster
datamigrate migrate sync --plan web-server-01-plan.yaml   # T2: maybe 500 MB
datamigrate migrate sync --plan web-server-01-plan.yaml   # T3: maybe 200 MB
datamigrate migrate sync --plan web-server-01-plan.yaml   # T4: maybe 50 MB
```

Keep running syncs until the delta is small enough that cutover downtime is acceptable.

### CUTOVER DAY: Final sync + switch to Nutanix

```bash
datamigrate cutover --plan web-server-01-plan.yaml --shutdown-source
```

What happens internally:
1. Final incremental sync (T_last) — usually seconds/minutes
2. Shutdown source VM on VMware (graceful guest OS shutdown)
3. One more sync post-shutdown — catches in-flight disk writes
4. Disconnect iSCSI from datamigrate machine
5. Create VM on Nutanix AHV (CPU/RAM/NICs mapped from plan)
6. Attach Volume Group disks to the new VM
7. Power on → VM boots on Nutanix
8. State: `CUTTING_OVER` → `COMPLETED`

```
Starting cutover...
Cutover complete! VM is now running on Nutanix AHV.
```

Downtime is only steps 2-7: typically **2-10 minutes**.

### CLEANUP: Remove snapshots and temp artifacts

```bash
datamigrate cleanup --plan web-server-01-plan.yaml
```

```
Removing VMware snapshots... OK
Removing staging files in /tmp/datamigrate/web-server-01/web-server-01-migration... OK
Removing migration state... OK
Cleanup complete.
```

### What's stored in the state DB between runs

The state DB (`/tmp/datamigrate/<vm-name>/state.db`) is what connects T0 → T1 → T2 → TN. It persists across reboots and stores:

```
state.db (after T0):
  ├── migration: "web-server-01-migration"
  ├── status: "SYNCING"
  ├── transport: "iscsi"
  ├── volume_group_id: "vg-uuid-abc123"
  ├── sync_count: 1
  ├── disk-0:
  │     changeID: "52 83 ... 0a"       ← VMware CBT bookmark
  │     bytes_copied: 107374182400     ← 100 GB
  │     last_synced: "2026-03-10 14:00:00"
  └── disk-1:
        changeID: "9e f2 ... 1b"
        bytes_copied: 53687091200      ← 50 GB
        last_synced: "2026-03-10 14:15:00"

state.db (after T3):
  ├── status: "CUTOVER_READY"
  ├── sync_count: 4
  ├── disk-0:
  │     changeID: "c4 11 ... 8f"       ← updated each sync
  │     bytes_copied: 112742891520     ← cumulative
  │     last_synced: "2026-03-12 09:30:00"
  └── disk-1:
        changeID: "b7 3e ... 2d"
        bytes_copied: 53901238272
        last_synced: "2026-03-12 09:32:00"
```

The `changeID` is the chain that connects each sync. Even if you wait days between runs, VMware knows exactly which blocks changed since the last saved changeID.

## Configuration

### Config file (YAML)

Create a config file (see `configs/example.yaml`):

```yaml
source:
  vcenter: "vcenter.example.com"
  username: "administrator@vsphere.local"
  password: "changeme"          # or use creds file / env var
  insecure: true

target:
  prism_central: "prism.example.com"
  username: "admin"
  password: "changeme"          # or use creds file / env var
  insecure: true
  cluster_uuid: "00000000-0000-0000-0000-000000000000"

staging:
  directory: "/tmp/datamigrate"
```

### Credentials files (recommended)

Keep passwords out of your config file by using separate `.creds` files. This is safer — you can commit `config.yaml` to version control while keeping secrets local.

#### Step-by-step setup

**Step 1:** Copy the example templates

```bash
cp configs/vmware.creds.example configs/vmware.creds
cp configs/nutanix.creds.example configs/nutanix.creds
```

**Step 2:** Edit with your real credentials

```bash
# configs/vmware.creds
username = administrator@vsphere.local
password = "my-vcenter-password"
host = vcenter.example.com
```

```bash
# configs/nutanix.creds
username = admin
password = "my-prism-password"
host = prism.example.com
```

**Step 3:** Lock down permissions

```bash
chmod 600 configs/vmware.creds configs/nutanix.creds
```

**Step 4:** Remove passwords from config.yaml (optional — creds files override them)

```yaml
# configs/config.yaml — no passwords needed
source:
  vcenter: "vcenter.example.com"
  insecure: true

target:
  prism_central: "prism.example.com"
  insecure: true
  cluster_uuid: "00000000-0000-0000-0000-000000000000"

staging:
  directory: "/tmp/datamigrate"
```

**Step 5:** Run as usual — credentials are picked up automatically

```bash
# The tool finds vmware.creds and nutanix.creds in the same directory as config.yaml
datamigrate validate --config configs/config.yaml

./datamigrate plan create --config configs/config.yaml --vm web-server-01
# if in configs the creds are there
./datamigrate plan create --vm ubuntu-vm


# Plan files also pick up creds from their directory
# If plan is saved in configs/, it will use the same creds files
datamigrate migrate start --plan configs/web-server-01-plan.yaml
```

**Step 6:** Verify `.gitignore` excludes creds files (already included)

```
*.creds
```

#### Final file layout

```
configs/
  config.yaml              # safe to commit — no passwords
  vmware.creds             # git-ignored, chmod 600
  nutanix.creds            # git-ignored, chmod 600
  vmware.creds.example     # template, safe to commit
  nutanix.creds.example    # template, safe to commit
```

#### Creds file format

```bash
# Lines starting with # are comments
# Format: KEY = VALUE (spaces around = are optional)
# Values can be quoted with single or double quotes

username = administrator@vsphere.local
password = "my-password"
host = vcenter.example.com
```

| Key | What it sets |
|---|---|
| `username` | Login username |
| `password` | Login password |
| `host` | Server hostname (overrides `vcenter` or `prism_central` in config) |

#### Where does the tool look for creds files?

The tool looks in the **same directory** as the file you pass:

| Command | Looks for creds in |
|---|---|
| `--config configs/config.yaml` | `configs/vmware.creds` + `configs/nutanix.creds` |
| `--plan /opt/plans/vm1-plan.yaml` | `/opt/plans/vmware.creds` + `/opt/plans/nutanix.creds` |
| `--config /etc/datamigrate/config.yaml` | `/etc/datamigrate/vmware.creds` + `/etc/datamigrate/nutanix.creds` |

If creds files don't exist, the tool silently falls back to config file values. No error.

### Environment variables

Environment variables take the highest priority and override both config file and creds file values:

| Variable | Description |
|---|---|
| `DATAMIGRATE_SOURCE_VCENTER` | vCenter hostname |
| `DATAMIGRATE_SOURCE_USERNAME` | vCenter username |
| `DATAMIGRATE_SOURCE_PASSWORD` | vCenter password |
| `DATAMIGRATE_TARGET_PRISM_CENTRAL` | Prism Central hostname |
| `DATAMIGRATE_TARGET_USERNAME` | Prism Central username |
| `DATAMIGRATE_TARGET_PASSWORD` | Prism Central password |

Example:
```bash
# Override just the password via env var, use creds file for everything else
export DATAMIGRATE_SOURCE_PASSWORD="vault-injected-secret"
datamigrate validate --config configs/config.yaml
```

### Credential resolution order

Priority from highest to lowest:

1. **Environment variables** (`DATAMIGRATE_SOURCE_PASSWORD`, etc.)
2. **Credentials files** (`vmware.creds`, `nutanix.creds` in same directory as config/plan)
3. **Config/plan file** values (YAML `password` field)

The first non-empty value wins. You can mix sources — e.g., username from creds file, password from env var, host from config.yaml.

## Transport modes

### iSCSI (default)

Writes blocks directly to a Nutanix Volume Group via iSCSI. Only changed blocks cross the network on incremental syncs.

```bash
datamigrate plan create --config config.yaml --vm myvm --transport iscsi
```

**Network transfer example** (100 GB VM, 10 syncs):
- T0: 100 GB (full disk)
- T1: 5 GB (delta only)
- T2: 500 MB (delta only)
- Total: ~110 GB

No local disk space needed. No intermediate files.

### Image (legacy)

Writes to a local raw file, converts to qcow2, uploads the full qcow2 to Nutanix Images each sync.

```bash
datamigrate plan create --config config.yaml --vm myvm --transport image
```

**Network transfer example** (100 GB VM → 30 GB qcow2, 10 syncs):
- T0: 30 GB (full qcow2)
- T1: 30 GB (full re-upload)
- T2: 30 GB (full re-upload)
- Total: ~300 GB

Requires local disk space for raw + qcow2 files. Simpler but less efficient.

## CLI commands

| Command | Description |
|---|---|
| `datamigrate discover` | List VMs on vCenter |
| `datamigrate validate --config <path>` | Test connectivity to vCenter and Nutanix |
| `datamigrate plan create --config <path> --vm <name>` | Create a migration plan |
| `datamigrate plan show <plan-file>` | Display a migration plan (passwords redacted) |
| `datamigrate migrate start --plan <plan>` | Full sync (T0) |
| `datamigrate migrate sync --plan <plan>` | Incremental sync (T1..TN) |
| `datamigrate migrate status --plan <plan>` | Show migration progress |
| `datamigrate cutover --plan <plan> [--shutdown-source]` | Final sync + create VM on AHV |
| `datamigrate cleanup --plan <plan>` | Remove snapshots and temp artifacts |

## Typical workflow

```
discover → plan create → validate → migrate start → migrate sync (repeat) → cutover → cleanup
```

```
Source VM:  ████████████████████████████████████████░░ (shutdown)
T0 sync:   ████████████████░
T1 sync:             ██░
T2 sync:                  █░
T3 sync:                     █░
Cutover:                        ██ (final Δ + create VM)
Target VM:                         ░░░░████████████████████
                                        ↑ VM boots on AHV
Downtime:  ─────────────────────────────►|◄── 2-10 min
```

## Parallel migration (multiple VMs)

You can migrate multiple VMs simultaneously by running one instance per VM. Each VM gets its own isolated staging directory automatically (e.g., `/tmp/datamigrate/web-server-01/`, `/tmp/datamigrate/db-server/`), so there are no conflicts.

```bash
# Create plans for each VM
datamigrate plan create --config config.yaml --vm web-server-01
datamigrate plan create --config config.yaml --vm db-server
datamigrate plan create --config config.yaml --vm app-server

# Run full syncs in parallel
datamigrate migrate start --plan web-server-01-plan.yaml &
datamigrate migrate start --plan db-server-plan.yaml &
datamigrate migrate start --plan app-server-plan.yaml &
wait

# Run incremental syncs in parallel
datamigrate migrate sync --plan web-server-01-plan.yaml &
datamigrate migrate sync --plan db-server-plan.yaml &
datamigrate migrate sync --plan app-server-plan.yaml &
wait

# Check status
datamigrate migrate status --plan web-server-01-plan.yaml
datamigrate migrate status --plan db-server-plan.yaml

# Cutover each VM when ready
datamigrate cutover --plan web-server-01-plan.yaml --shutdown-source
datamigrate cutover --plan db-server-plan.yaml --shutdown-source
```

Each VM has:
- Its own plan file
- Its own state DB (`/tmp/datamigrate/<vm-name>/state.db`)
- Its own Volume Group on Nutanix (iSCSI mode)
- Its own snapshots on VMware

## Running from any machine

This tool does **not** need to run on a VMware host or inside the VM being migrated. It can run from:

- Your laptop
- A jump server
- Any machine with network access to both vCenter (port 443) and Nutanix Prism Central (port 9440)

For iSCSI transport, the machine also needs iSCSI access to the Nutanix Data Services IP (port 3260).

## Development

```bash
# Run tests
make test

# Run tests with coverage
make test-cover

# Build
make build

# Lint (requires golangci-lint)
make lint
```

## Project structure

```
datamigrate/
├── cmd/datamigrate/main.go           # Entry point
├── internal/
│   ├── cli/                          # Cobra CLI commands
│   ├── config/                       # Config + plan loading, creds files
│   ├── state/                        # BoltDB state persistence + journal
│   ├── vmware/                       # govmomi client, discovery, snapshot, CBT
│   ├── transport/                    # NBD reader, VDDK placeholder
│   ├── blockio/                      # Block reader/writer interfaces, pipeline, iSCSI writer, qcow2 writer
│   ├── nutanix/                      # Prism Central client, images, VMs, volume groups
│   ├── migration/                    # Orchestrator, full sync, incremental sync, cutover
│   └── util/                         # Logging, retry, size formatting
├── configs/
│   ├── example.yaml                  # Example config
│   ├── vmware.creds.example          # Example VMware credentials file
│   └── nutanix.creds.example         # Example Nutanix credentials file
├── Makefile
└── README.md
```



# helpful commands
# One-time setup from your Mac
  ```ssh-keygen -t ed25519 -f ~/.ssh/migration-host                                                           
  ssh-copy-id -i ~/.ssh/migration-host ubuntuadmin@migration-host                                          

  Then add to ~/.ssh/config:
  Host migration-host
      User ubuntuadmin
      IdentityFile ~/.ssh/migration-host

  Now scp and ssh will never ask for a password.

  Quick alternative (if you don't want keys): use sshpass:
  # Install
  brew install hudochenkov/sshpass/sshpass

  # Use with password file
  echo 'yourpassword' > ~/.migration-pass && chmod 600 ~/.migration-pass
  sshpass -f ~/.migration-pass scp bin/datamigrate-linux-amd64 ubuntuadmin@migration-host:~/datamigrate


  sshpass -f ~/.migration-pass scp bin/datamigrate-linux-amd64 ubuntuadmin@migration-host:~/datamigrate

```