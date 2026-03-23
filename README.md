# datamigrate

VMware vSphere to Nutanix AHV VM migration tool using block-level replication with near-zero downtime.

## How it works

1. Takes a VMware snapshot and reads disk blocks
2. Writes blocks to Nutanix — either directly via iSCSI Volume Groups or as qcow2 images
3. Subsequent syncs transfer only changed blocks (CBT delta)
4. Final cutover: last delta sync, shutdown source, create VM on Nutanix AHV, power on

The disk on Nutanix is always a complete, bootable disk after every sync. You can test-boot at any stage.

## Transport Modes

The tool supports three transport modes that control how data moves from VMware to Nutanix:

### `--transport iscsi` (production, Linux only)

The fastest and most efficient mode. Reads raw flat disk bytes from the VMware datastore via HTTPS and writes them directly to a Nutanix Volume Group via iSCSI. **No local disk, no conversion, no temp files.** Incremental syncs write only CBT delta blocks.

```
VMware datastore ──HTTPS GET──▶ RawDiskReader (flat VMDK = raw sector bytes)
                                      │
                                64 MB chunks via pipeline (4 parallel workers)
                                      │
                                ISCSIWriter.WriteAt(offset, data)
                                      │
                                /dev/sdX ──iSCSI──▶ Nutanix Volume Group
```

**Requires:** Linux with `open-iscsi` (provides `iscsiadm`)

**Where data lives:** Nutanix **Volume Group** — disks stay there across all syncs. At cutover, a VM is created directly from the VG disks (no copy needed).

**Incremental syncs:** CBT identifies changed blocks → only those blocks are read from VMware and written to the same VG disk at their exact offsets. The VG disk is always a complete, bootable disk.

### `--transport stream` (testing, Mac/Linux)

Downloads the disk via NFC export (streamOptimized VMDK), converts to qcow2 locally with `qemu-img`, then uploads to the Nutanix image store. Works from any OS including macOS.

```
VMware snapshot ──NFC export──▶ streamOptimized VMDK (compressed, sparse-aware)
                                      │
                                Save to temp file (~10% of disk size)
                                      │
                                qemu-img convert -f vmdk -O qcow2
                                      │
                                Upload qcow2 ──HTTP PUT──▶ Nutanix Image Store
```

**Requires:** `qemu-img` installed, local disk space for VMDK + qcow2 temp files

**Where data lives:** Nutanix **Image Store** — each sync creates a new image. You must manually create a VM from the image in Prism Central.

**Incremental syncs:** Not supported — each run creates a full new image.

### `--transport image` (legacy fallback)

Writes blocks to a local raw file, converts to qcow2, then uploads. Similar to stream but uses the block pipeline instead of NFC.

**Requires:** `qemu-img`, local disk space equal to ~1.5x VM disk size

### Comparison

| | iSCSI (production) | Stream (testing) | Image (legacy) |
|---|---|---|---|
| **OS** | Linux only | Any (Mac/Linux) | Any |
| **Read method** | Datastore HTTPS (raw flat bytes) | NFC export (streamOptimized VMDK) | NFC export |
| **Write method** | iSCSI WriteAt to Volume Group | HTTP PUT to Image Store | Local qcow2 → HTTP PUT |
| **Local disk** | None | VMDK + qcow2 temp files | Raw + qcow2 staging |
| **Dependencies** | open-iscsi | qemu-img | qemu-img |
| **Incremental** | Yes (CBT deltas) | No (full each time) | No |
| **Speed** | Fastest (direct block I/O) | Moderate | Slowest |
| **Nutanix target** | Volume Group | Image Store | Image Store |
| **VM creation** | From VG disks (no copy) | Manual from image | Manual from image |

## Migration Lifecycle

### Full picture: T0 → T1..TN → Cutover

```
T0 (Full Sync)           T1..TN (Incremental)         Cutover
─────────────────        ─────────────────────         ────────────────────
migrate start            migrate sync (repeat)         migrate cutover

┌─────────────┐          ┌─────────────┐               ┌─────────────┐
│ VMware VM   │          │ VMware VM   │               │ VMware VM   │
│ (running)   │          │ (running)   │               │ (shut down) │
│             │          │             │               │             │
│ Snapshot T0 │          │ Snapshot TN │               │ Final snap  │
└──────┬──────┘          └──────┬──────┘               └──────┬──────┘
       │ read all                │ read CBT delta              │ last delta
       │ blocks                  │ blocks only                 │
       ▼                         ▼                             ▼
┌─────────────┐          ┌─────────────┐               ┌─────────────┐
│ Nutanix VG  │          │ Nutanix VG  │               │ Nutanix VM  │
│ (full copy) │          │ (updated)   │               │ (powered on)│
│             │          │             │               │ from VG     │
└─────────────┘          └─────────────┘               └─────────────┘

Source VM: RUNNING        Source VM: RUNNING             Source VM: OFF
Downtime:  ZERO           Downtime:  ZERO                Downtime: 2-10 min
```

### Where does data live on Nutanix?

**iSCSI transport:**
- **Volume Group** named `datamigrate-<vm-name>` with one disk per source VM disk
- The VG persists across all syncs — T0 creates it, T1..TN update it in-place
- At cutover, a VM is created and the VG disks become the VM's disks
- You can **test-boot** at any point by creating a VM from the VG in Prism (clone the disk first so syncs can continue)

**Stream/Image transport:**
- **Image Store** — each run creates a new image named `<vm-name>-disk-<key>-<datetime>`
- You must manually create a VM from the image in Prism Central
- No incremental capability — each run is a full upload

### Testing a migration before cutover

After T0 or any sync, you can verify the disk is bootable:

**iSCSI transport (Volume Group):**
1. In Prism Central: **VMs → Create VM**
2. Add disk: **Clone from Volume Group** → select `datamigrate-<vm-name>`
3. Set boot type: **Legacy BIOS** (for older VMs like RHEL 6) or **UEFI** (for modern VMs)
4. Power on and verify
5. **Delete the test VM** before running the next `migrate sync` (or the VG disk will be locked)

**Stream transport (Image):**
1. In Prism Central: **VMs → Create VM**
2. Add disk: **Clone from Image** → select the latest `<vm-name>-disk-<key>-<datetime>` image
3. Set boot type appropriately
4. Power on and verify

## Prerequisites

### Where to run

The tool can run from **any machine** with network access to both VMware and Nutanix. For best performance, run it from a **Linux VM inside the VMware datacenter** — this keeps the heavy block transfer on the fast datacenter network (10/25 GbE) instead of going over WAN/VPN.

```
Recommended setup:

  ┌─────────────────────────── VMware Datacenter ───────────────────────────┐
  │                                                                         │
  │  ┌──────────────┐     fast (10/25 GbE)      ┌───────────────────────┐  │
  │  │ Linux VM     │ ─────────────────────────► │ vCenter API (443)    │  │
  │  │ running      │                            └───────────────────────┘  │
  │  │ datamigrate  │                                                       │
  │  │              │     fast (10/25 GbE)      ┌───────────────────────┐  │
  │  │              │ ─────────────────────────► │ Nutanix Prism (9440) │  │
  │  │              │                            │ iSCSI Data Svc (3260)│  │
  │  └──────────────┘                            └───────────────────────┘  │
  │                                                                         │
  └─────────────────────────────────────────────────────────────────────────┘
```

A small Linux VM works fine — 2 vCPUs, 4 GB RAM. The tool streams blocks through memory; it doesn't need much disk (zero disk for iSCSI transport).

### System requirements

| Requirement | iSCSI transport | Stream transport | Image transport |
|---|---|---|---|
| **OS** | Linux (RHEL/CentOS/Ubuntu) | Linux or macOS | Linux or macOS |
| **CPU** | 2+ vCPUs | 2+ vCPUs | 2+ vCPUs |
| **RAM** | 512 MB per VM | 512 MB per VM | 512 MB per VM |
| **Disk** | ~1 GB (binary + state DB) | VMDK + qcow2 temp (~0.5x disk) | Raw + qcow2 (~1.5x disk) |
| **Dependencies** | open-iscsi | qemu-img | qemu-img |
| **Go** | 1.24+ (build only) | 1.24+ (build only) | 1.24+ (build only) |

### Memory usage and performance

**RAM is NOT a bottleneck.** The tool streams blocks through a small in-memory buffer — it does NOT load the entire disk into RAM.

How the pipeline works:
```
VMware ESXi                    Memory buffer                    Nutanix
┌──────────┐    read block    ┌────────────────┐  write block  ┌──────────┐
│ Disk     │ ────────────────►│ Channel buffer │ ─────────────►│ Volume   │
│ (100 GB) │                  │ (16 slots)     │   via iSCSI   │ Group    │
│          │   1 block at     │ + 4 workers    │               │          │
│          │   a time         │                │               │          │
└──────────┘                  └────────────────┘               └──────────┘
                               ↑
                               Only 20 blocks in memory at any time
                               20 × 1 MB = ~20 MB
```

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

### Why streaming is faster than disk-first

You might think: "write to local disk first, then transfer to Nutanix — wouldn't that be faster?" **No — streaming is faster:**

```
STREAMING (what iSCSI transport does):
  Read and write happen IN PARALLEL — while block N writes to Nutanix,
  block N+1 is already being read from VMware.

  VMware read:   ████████████████████████████   13 min
  Nutanix write: ░████████████████████████████  13 min (starts 1 block later)
                                          Total: ~13 min

DISK-FIRST (write locally, then transfer):
  Read everything to local disk FIRST, then read it back and send to Nutanix.

  VMware read:    ████████████████████████████              13 min
  Write to SSD:   ████████████████████████████               2 min
  Read from SSD:                                ████████████ 2 min
  Nutanix write:                                █████████████████████████████ 13 min
                                                                       Total: ~30 min
```

Streaming overlaps read and write, disk-first does them sequentially. Streaming also avoids:
- 2x local disk I/O (write + read back)
- Needing enough disk space for the entire VM
- SSD wear from temporary data

The only scenario where disk-first wins is if the VMware-to-machine link is much faster than machine-to-Nutanix (e.g., reading from local datastore SSD but writing to Nutanix over a slow WAN link). But if you're running from a VM inside the same datacenter (recommended), both links are equally fast.

### Disk sizing

**iSCSI transport (default):** Blocks stream directly to Nutanix — no local staging files. You only need space for the binary (~16 MB), BoltDB state file (~1 MB per VM), and logs. **~1 GB total is plenty**, even for 50+ parallel VMs.

**Image transport:** Each VM being migrated needs local disk for a raw file + qcow2 file. Formula:

```
Disk per VM  =  VM disk size (raw)  +  compressed qcow2 (~30-50% of raw)
             ≈  1.5x the VM's disk size
```

| VMs in parallel | Avg VM disk | Disk needed (image transport) |
|---|---|---|
| 1 VM | 100 GB | ~150 GB |
| 1 VM | 500 GB | ~750 GB |
| 3 VMs | 100 GB each | ~450 GB |
| 5 VMs | 200 GB each | ~1.5 TB |
| 10 VMs | 100 GB each | ~1.5 TB |

**Recommendation:** Use iSCSI transport (default) to avoid disk requirements entirely. If you must use image transport, provision a data disk mounted at `/tmp/datamigrate` with enough space for all parallel VMs.

```bash
# Example: mount a 500 GB disk for image transport staging
sudo mkfs.xfs /dev/sdb
sudo mkdir -p /tmp/datamigrate
sudo mount /dev/sdb /tmp/datamigrate
```

### Nutanix prerequisites

The tool uses the Nutanix Prism Central v3 API. A few things must be configured on the Nutanix side before you start.

#### Required setup (one-time, by Nutanix admin)

**1. iSCSI Data Services IP must be configured**

The tool creates Volume Groups and connects to them via iSCSI. This requires a Data Services IP on the Nutanix cluster.

```
Prism Element → Cluster Details → iSCSI Data Services IP
```

If this is not set, the tool will fail with: `cluster has no data services IP configured`

To configure:
- Log in to **Prism Element** (not Prism Central)
- Go to **Settings** (gear icon) → **Cluster Details**
- Set **iSCSI Data Services IP** to a free IP on the storage network
- This IP must be reachable from the machine running datamigrate (port 3260)

**2. Prism Central API access**

The tool needs a user account on Prism Central with permissions to:

| API Operation | Prism Central Permission |
|---|---|
| Create/delete Volume Groups | Storage Admin or Cluster Admin |
| Create/upload Images | Image Admin or Cluster Admin |
| Create/power on VMs | VM Admin or Cluster Admin |
| List subnets/containers | Viewer (any role) |
| List clusters | Viewer (any role) |

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

| Resource | Created when | Transport | Named | Cleaned up |
|---|---|---|---|---|
| **Volume Group** | `migrate start` (T0) | iSCSI | `datamigrate-<vm-name>` | `cleanup` or manually |
| **VG Disks** | `migrate start` (T0) | iSCSI | One LUN per VM disk, sized to match | With the Volume Group |
| **Image** | `migrate start` (T0) | stream/image | `<vm-name>-disk-<N>-<datetime>` | Manually via Prism |
| **Target VM** | `cutover` | all | Same as source VM name | Manually if unwanted |

#### Nutanix checklist

```
[ ] iSCSI Data Services IP configured on the cluster
[ ] Prism Central user account with Cluster Admin (or equivalent) role
[ ] Cluster UUID noted
[ ] Target subnet UUID(s) noted for network mapping
[ ] Storage container UUID noted for storage mapping (optional)
[ ] Port 9440 accessible from datamigrate machine to Prism Central
[ ] Port 3260 accessible from datamigrate machine to Data Services IP (iSCSI transport)
[ ] Enough storage capacity on Nutanix for the migrated VM disks
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