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
| VMware ESXi hosts | 902 (NFC) | Disk reading via NFC export |
| Nutanix Prism Central | 9440 (HTTPS) | All commands |

### Software dependencies

- `qemu-img` — converts VMDK to raw and raw to qcow2
- `go` 1.24+ — only needed to build the binary; not needed at runtime

### Linux setup (RHEL/CentOS/Rocky)

```bash
# Install qemu-img
sudo yum install -y qemu-img

# Install Go (build only — skip if using pre-built binary)
sudo yum install -y golang

# Build datamigrate
git clone <repo-url> && cd datamigrate
make build

# Copy binary to PATH
sudo cp bin/datamigrate /usr/local/bin/
```

### Linux setup (Ubuntu/Debian)

```bash
# Install qemu-img
sudo apt-get update
sudo apt-get install -y qemu-utils

# Install Go (build only — skip if using pre-built binary)
sudo apt-get install -y golang-go

# Build datamigrate
git clone <repo-url> && cd datamigrate
make build

# Copy binary to PATH
sudo cp bin/datamigrate /usr/local/bin/
```

### macOS setup

```bash
# Install qemu-img
brew install qemu

# Build datamigrate
git clone <repo-url> && cd datamigrate
make build
```

### Verify setup

```bash
# Check qemu-img
qemu-img --version

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

## Project Structure

```
datamigrate/
├── cmd/datamigrate/main.go           # Entry point
├── internal/
│   ├── cli/                          # Cobra CLI commands (discover, plan, migrate, createvm, cleanup)
│   ├── config/                       # Config + plan loading, creds files
│   ├── state/                        # BoltDB state persistence + journal
│   ├── vmware/                       # govmomi client, discovery, snapshot, CBT, NFC readers
│   ├── repository/                   # Repository transport: VMDK parser, raw file writer, qemu-img wrapper
│   ├── blockio/                      # Block reader/writer interfaces, pipeline
│   ├── nutanix/                      # Prism Central client: images, VMs
│   ├── migration/                    # Orchestrator, full sync, incremental sync
│   └── util/                         # Logging, retry, size formatting
├── configs/                          # Plan YAML files and credential templates
├── docs/                             # Design docs, comparison tables
├── Makefile
└── README.md
```

## CLI Commands

```bash
datamigrate discover    --vcenter <url> --username <user>        # List VMs on vCenter
datamigrate plan create --vm <name> --transport repository       # Create migration plan
datamigrate plan show   <plan-file>                              # Show plan details
datamigrate migrate start  --plan <plan.yaml>                    # Full sync (T0)
datamigrate migrate sync   --plan <plan.yaml>                    # Incremental sync (T1..TN)
datamigrate migrate status --plan <plan.yaml>                    # Show progress
datamigrate createvm       --plan <plan.yaml> --boot-type UEFI --power-on  # Create VM from image
datamigrate cleanup        --plan <plan.yaml>                    # Remove snapshots/artifacts
```

