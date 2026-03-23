# Datamigrate: Phase Diagrams

## Master Overview — All Phases

```
┌─────────────────────────────────────────────────────────────────────────────────────────┐
│                              DATAMIGRATE — PHASE OVERVIEW                               │
│                                                                                         │
│  Phase 1          Phase 2          Phase 3          Phase 4                              │
│  ┌──────────┐     ┌──────────┐     ┌──────────┐     ┌──────────┐                        │
│  │ Project  │     │ Snapshot │     │  Block   │     │  qcow2   │                        │
│  │ Skeleton │────►│    +     │────►│ Reading  │────►│ Writer + │                        │
│  │    +     │     │   CBT    │     │ via NBD  │     │  Local   │                        │
│  │ VMware   │     │          │     │          │     │ Staging  │                        │
│  │ Discover │     │          │     │          │     │          │                        │
│  └──────────┘     └──────────┘     └──────────┘     └──────────┘                        │
│       │                │                │                │                               │
│       ▼                ▼                ▼                ▼                               │
│  CLI scaffold     State store      Pipeline         Raw→qcow2                           │
│  Config load      BoltDB           Concurrency      qemu-img                            │
│  govmomi          Snapshots        NFC lease         Sparse files                       │
│  VM discovery     CBT queries      Block reader      Compression                       │
│                                                                                         │
│  Phase 5          Phase 6          Phase 7          Phase 8                              │
│  ┌──────────┐     ┌──────────┐     ┌──────────┐     ┌──────────┐                        │
│  │ Nutanix  │     │Migration │     │Increment │     │  VDDK    │                        │
│  │ Client + │────►│Orchestr- │────►│  Sync +  │────►│Transport │                        │
│  │  Image   │     │  ator    │     │ Cutover  │     │    +     │                        │
│  │ Upload   │     │          │     │          │     │ Polish   │                        │
│  │          │     │          │     │          │     │          │                        │
│  └──────────┘     └──────────┘     └──────────┘     └──────────┘                        │
│       │                │                │                │                               │
│       ▼                ▼                ▼                ▼                               │
│  Prism API        State machine    Delta sync        CGo VDDK                           │
│  Image CRUD       Full sync T0     CBT fallback      SAN/HotAdd                        │
│  VM create        Progress/ETA     Final cutover     Multi-disk                         │
│  Subnet list      Block journal    Cleanup           Progress bars                      │
│                                                                                         │
│  ✅ Done           ✅ Done          ✅ Done           ⬜ TODO                             │
└─────────────────────────────────────────────────────────────────────────────────────────┘
```

---

## Phase 1: Project Skeleton + VMware Discovery

```
┌──────────────────────────────────────────────────────────────────────────┐
│  PHASE 1: PROJECT SKELETON + VMWARE DISCOVERY                          │
├──────────────────────────────────────────────────────────────────────────┤
│                                                                        │
│  ┌─────────────────┐                                                   │
│  │   go mod init   │                                                   │
│  │   + Cobra CLI   │                                                   │
│  │   + Viper cfg   │                                                   │
│  └────────┬────────┘                                                   │
│           │                                                            │
│           ▼                                                            │
│  ┌─────────────────┐      ┌──────────────────────────────────────────┐ │
│  │  Config Loader  │      │  configs/example.yaml                    │ │
│  │                 │◄─────│                                          │ │
│  │  YAML + env     │      │  source:                                 │ │
│  │  vars merged    │      │    vcenter: "vcenter.example.com"        │ │
│  └────────┬────────┘      │    username: "admin@vsphere.local"       │ │
│           │               │  target:                                 │ │
│           ▼               │    prism_central: "prism.example.com"    │ │
│  ┌─────────────────┐      │  staging:                                │ │
│  │  VMware Client  │      │    directory: "/tmp/datamigrate"         │ │
│  │                 │      └──────────────────────────────────────────┘ │
│  │  govmomi.New-   │                                                   │
│  │  Client()       │                                                   │
│  └────────┬────────┘                                                   │
│           │                                                            │
│           ▼                                                            │
│  ┌─────────────────┐      ┌──────────────────────────────────────────┐ │
│  │  VM Discovery   │─────►│  Output:                                 │ │
│  │                 │      │                                          │ │
│  │  find.Finder    │      │  NAME        MOREF   POWER  CPUs  DISKS │ │
│  │  + property     │      │  web-srv-01  vm-42   on     4     2     │ │
│  │  collector      │      │  db-srv-01   vm-43   on     8     4     │ │
│  └─────────────────┘      │  app-srv-02  vm-44   off    2     1     │ │
│                           └──────────────────────────────────────────┘ │
│                                                                        │
│  Files:                                                                │
│  ├── cmd/datamigrate/main.go                                           │
│  ├── internal/cli/root.go, discover.go                                 │
│  ├── internal/config/config.go, mapping.go                             │
│  ├── internal/vmware/client.go, discovery.go                           │
│  └── internal/util/logging.go, retry.go, size.go                      │
│                                                                        │
│  Tests: govmomi/simulator — DiscoverVMs, FindVM                        │
└──────────────────────────────────────────────────────────────────────────┘
```

---

## Phase 2: Snapshot + CBT

```
┌──────────────────────────────────────────────────────────────────────────┐
│  PHASE 2: SNAPSHOT + CBT (Changed Block Tracking)                      │
├──────────────────────────────────────────────────────────────────────────┤
│                                                                        │
│              Source VM (running on ESXi)                                │
│              ┌──────────────────────┐                                  │
│              │   web-server-01      │                                  │
│              │                      │                                  │
│              │  ┌────────────────┐  │                                  │
│              │  │ disk-flat.vmdk │  │                                  │
│              │  │ (200 GB)       │  │                                  │
│              │  └───────┬────────┘  │                                  │
│              └──────────┼───────────┘                                  │
│                         │                                              │
│           ┌─────────────┼──────────────┐                               │
│           ▼             ▼              ▼                               │
│  ┌──────────────┐  ┌──────────┐  ┌───────────────┐                    │
│  │ 1. Enable    │  │ 2. Snap- │  │ 3. Query CBT  │                    │
│  │    CBT       │  │    shot  │  │               │                    │
│  │              │  │          │  │ changeId="*"  │                    │
│  │ Reconfigure  │  │ Creates  │  │ → all blocks  │                    │
│  │ VM_Task(     │  │ point-in │  │               │                    │
│  │  changeTrk   │  │ -time    │  │ changeId="52" │                    │
│  │  =true)      │  │ freeze   │  │ → only deltas │                    │
│  └──────────────┘  └──────────┘  └───────┬───────┘                    │
│                                          │                             │
│                                          ▼                             │
│                                 ┌─────────────────┐                    │
│                                 │  CBT Response    │                    │
│                                 │                  │                    │
│                                 │  Changed areas:  │                    │
│                                 │  (0, 65536)      │                    │
│                                 │  (131072, 32768) │                    │
│                                 │  (524288, 65536) │                    │
│                                 │  ...             │                    │
│                                 │                  │                    │
│                                 │  New changeId:   │                    │
│                                 │  "52:aa:bb:..."  │                    │
│                                 └────────┬────────┘                    │
│                                          │                             │
│                                          ▼                             │
│                                 ┌─────────────────┐                    │
│                                 │  BoltDB State    │                    │
│                                 │                  │                    │
│                                 │  plan: "web-01"  │                    │
│                                 │  status: SYNCING │                    │
│                                 │  changeId: saved │                    │
│                                 │  sync_count: 1   │                    │
│                                 └─────────────────┘                    │
│                                                                        │
│  Files:                                                                │
│  ├── internal/vmware/snapshot.go  (CreateSnapshot, RemoveAllSnapshots) │
│  ├── internal/vmware/cbt.go       (EnableCBT, QueryChangedBlocks)     │
│  ├── internal/state/store.go      (BoltDB CRUD)                       │
│  └── internal/state/migration.go  (MigrationState, DiskState structs) │
│                                                                        │
│  Key API: methods.QueryChangedDiskAreas()                              │
│  Key note: changeId from VirtualDiskFlatVer2BackingInfo, NOT response  │
└──────────────────────────────────────────────────────────────────────────┘
```

---

## Phase 3: Block Reading via NBD

```
┌──────────────────────────────────────────────────────────────────────────┐
│  PHASE 3: BLOCK READING VIA NBD (NFC Lease)                           │
├──────────────────────────────────────────────────────────────────────────┤
│                                                                        │
│  CBT tells us WHICH blocks to read. Now we READ them.                  │
│                                                                        │
│  ┌──────────────┐         ┌───────────────┐        ┌──────────────┐    │
│  │   ESXi Host  │         │  NFC Lease     │        │  datamigrate │    │
│  │              │         │  (Export)      │        │  machine     │    │
│  │  ┌────────┐  │  HTTPS  │               │ Blocks │              │    │
│  │  │ VMDK   │──┼────────►│  URL per disk │───────►│  NBDReader   │    │
│  │  │ blocks │  │         │               │        │              │    │
│  │  └────────┘  │         │  Lease.Wait() │        │  ReadBlocks()│    │
│  └──────────────┘         └───────────────┘        └──────┬───────┘    │
│                                                           │            │
│                                                           ▼            │
│  ┌─────────────────────────────────────────────────────────────────┐    │
│  │                    TRANSFER PIPELINE                            │    │
│  │                                                                 │    │
│  │  ┌──────────┐    ┌──────────┐    ┌──────────┐    ┌──────────┐  │    │
│  │  │ Worker 1 │    │ Worker 2 │    │ Worker 3 │    │ Worker 4 │  │    │
│  │  │ Read →   │    │ Read →   │    │ Read →   │    │ Read →   │  │    │
│  │  │ Write    │    │ Write    │    │ Write    │    │ Write    │  │    │
│  │  └──────────┘    └──────────┘    └──────────┘    └──────────┘  │    │
│  │       │               │               │               │        │    │
│  │       └───────────────┼───────────────┼───────────────┘        │    │
│  │                       ▼               ▼                        │    │
│  │              ┌──────────────────────────────┐                  │    │
│  │              │  dataCh (buffered channel)   │                  │    │
│  │              │  BlockData {                 │                  │    │
│  │              │    DiskKey, Offset,          │                  │    │
│  │              │    Length, Data []byte        │                  │    │
│  │              │  }                           │                  │    │
│  │              └──────────────────────────────┘                  │    │
│  │                                                                 │    │
│  │  Configurable concurrency (default: 4 workers)                  │    │
│  └─────────────────────────────────────────────────────────────────┘    │
│                                                                        │
│  Interfaces:                                                           │
│  ┌─────────────────────────────────────────────────────┐               │
│  │  BlockReader                                        │               │
│  │    ReadBlocks(ctx, []Extent) (<-chan BlockData, err) │               │
│  │    ReadExtent(ctx, Extent) (BlockData, err)         │               │
│  │    Close() error                                    │               │
│  └─────────────────────────────────────────────────────┘               │
│                                                                        │
│  Files:                                                                │
│  ├── internal/blockio/types.go    (BlockExtent, BlockData)             │
│  ├── internal/blockio/reader.go   (BlockReader interface)              │
│  ├── internal/blockio/pipeline.go (concurrent pipeline)                │
│  ├── internal/transport/nbd.go    (NBDReader via NFC)                  │
│  └── internal/vmware/reader.go    (DiskReader wrapper)                 │
└──────────────────────────────────────────────────────────────────────────┘
```

---

## Phase 4: qcow2 Writer + Local Staging

```
┌──────────────────────────────────────────────────────────────────────────┐
│  PHASE 4: QCOW2 WRITER + LOCAL STAGING                                │
├──────────────────────────────────────────────────────────────────────────┤
│                                                                        │
│  Pipeline output                                                       │
│       │                                                                │
│       ▼                                                                │
│  ┌─────────────────────────────────────────────────────────────────┐    │
│  │  Qcow2Writer                                                    │    │
│  │                                                                 │    │
│  │  Step 1: Create sparse raw file                                 │    │
│  │  ┌──────────────────────────────────────────────────┐           │    │
│  │  │  os.Create("disk-2000.raw")                      │           │    │
│  │  │  file.Truncate(200 GB)  ← sparse, 0 actual bytes│           │    │
│  │  └──────────────────────────────────────────────────┘           │    │
│  │                                                                 │    │
│  │  Step 2: Write blocks at exact offsets                          │    │
│  │  ┌──────────────────────────────────────────────────┐           │    │
│  │  │  file.WriteAt(block.Data, block.Offset)          │           │    │
│  │  │                                                  │           │    │
│  │  │  Raw file (200 GB logical, ~50 GB actual):       │           │    │
│  │  │  ┌───┬───┬───┬───┬───┬───┬───┬───┬───┬───┐      │           │    │
│  │  │  │█░░│███│░░░│██░│░░░│░░░│███│░░░│█░░│░░░│      │           │    │
│  │  │  └───┴───┴───┴───┴───┴───┴───┴───┴───┴───┘      │           │    │
│  │  │  █ = written blocks   ░ = sparse (zero, no disk) │           │    │
│  │  └──────────────────────────────────────────────────┘           │    │
│  │                                                                 │    │
│  │  Step 3: Finalize — convert raw → qcow2                        │    │
│  │  ┌──────────────────────────────────────────────────┐           │    │
│  │  │  qemu-img convert -f raw -O qcow2 -c             │           │    │
│  │  │    disk-2000.raw disk-2000.qcow2                  │           │    │
│  │  │                                                   │           │    │
│  │  │  -c = compressed output                           │           │    │
│  │  │  200 GB raw → ~20 GB qcow2 (typical)             │           │    │
│  │  └──────────────────────────────────────────────────┘           │    │
│  └─────────────────────────────────────────────────────────────────┘    │
│                                                                        │
│  Staging directory layout:                                             │
│  ┌──────────────────────────────────────────┐                          │
│  │  /tmp/datamigrate/                       │                          │
│  │  └── web-server-01-migration/            │                          │
│  │      ├── disk-2000.raw    (sparse, big)  │                          │
│  │      └── disk-2000.qcow2 (compressed)   │                          │
│  └──────────────────────────────────────────┘                          │
│                                                                        │
│  Interface:                                                            │
│  ┌──────────────────────────────────────────┐                          │
│  │  BlockWriter                             │                          │
│  │    WriteBlock(ctx, BlockData) error       │                          │
│  │    Finalize() error                      │                          │
│  └──────────────────────────────────────────┘                          │
│                                                                        │
│  Files:                                                                │
│  ├── internal/blockio/qcow2.go  (Qcow2Writer)                         │
│  └── internal/blockio/writer.go (BlockWriter interface)                │
└──────────────────────────────────────────────────────────────────────────┘
```

---

## Phase 5: Nutanix Client + Image Upload

```
┌──────────────────────────────────────────────────────────────────────────┐
│  PHASE 5: NUTANIX CLIENT + IMAGE UPLOAD                                │
├──────────────────────────────────────────────────────────────────────────┤
│                                                                        │
│  qcow2 file ready on local disk                                        │
│       │                                                                │
│       ▼                                                                │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │  Nutanix Client (Prism Central v3 REST API, port 9440)          │   │
│  │                                                                  │   │
│  │  ┌────────────┐  ┌──────────────┐  ┌─────────────┐              │   │
│  │  │ POST       │  │ PUT          │  │ GET         │              │   │
│  │  │ /images    │  │ /images/{id} │  │ /tasks/{id} │              │   │
│  │  │            │  │ /file        │  │             │              │   │
│  │  │ Create     │  │              │  │ Poll until  │              │   │
│  │  │ image      │  │ Upload       │  │ SUCCEEDED   │              │   │
│  │  │ entry      │  │ qcow2 body   │  │ or FAILED   │              │   │
│  │  └─────┬──────┘  └──────┬───────┘  └──────┬──────┘              │   │
│  │        │                │                  │                      │   │
│  │        ▼                ▼                  ▼                      │   │
│  │   image UUID       upload OK          task complete               │   │
│  └──────────────────────────────────────────────────────────────────┘   │
│                                                                        │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │  Additional Nutanix APIs                                         │   │
│  │                                                                  │   │
│  │  ┌───────────────────┐    ┌───────────────────┐                  │   │
│  │  │ POST              │    │ POST              │                  │   │
│  │  │ /subnets/list     │    │ /storage_          │                  │   │
│  │  │                   │    │  containers/list   │                  │   │
│  │  │ List networks     │    │                   │                  │   │
│  │  │ for NIC mapping   │    │ List containers   │                  │   │
│  │  └───────────────────┘    │ for storage map   │                  │   │
│  │                           └───────────────────┘                  │   │
│  └──────────────────────────────────────────────────────────────────┘   │
│                                                                        │
│  Validate command:                                                     │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │  $ datamigrate validate --config myconfig.yaml                   │   │
│  │                                                                  │   │
│  │  Validating VMware vCenter connection... OK                      │   │
│  │  Validating Nutanix Prism Central connection... OK               │   │
│  │                                                                  │   │
│  │  All validations passed.                                         │   │
│  └──────────────────────────────────────────────────────────────────┘   │
│                                                                        │
│  Files:                                                                │
│  ├── internal/nutanix/client.go   (HTTP client, auth, task poll)       │
│  ├── internal/nutanix/image.go    (CreateImage, UploadImage)           │
│  ├── internal/nutanix/vm.go       (CreateVM, PowerOnVM)                │
│  ├── internal/nutanix/network.go  (ListSubnets, ListContainers)        │
│  └── internal/cli/validate.go     (validate command)                   │
└──────────────────────────────────────────────────────────────────────────┘
```

---

## Phase 6: Migration Orchestrator (Full Sync T0)

```
┌──────────────────────────────────────────────────────────────────────────┐
│  PHASE 6: MIGRATION ORCHESTRATOR — FULL SYNC (T0)                      │
├──────────────────────────────────────────────────────────────────────────┤
│                                                                        │
│  State Machine:                                                        │
│  ┌────────┐    ┌───────────┐    ┌─────────┐    ┌──────────────┐        │
│  │CREATED │───►│ FULL_SYNC │───►│SYNCING  │◄──►│CUTOVER_READY │        │
│  └────────┘    └───────────┘    └─────────┘    └──────────────┘        │
│       │             │               │                │                  │
│       └─────────────┴───────────────┴────────────────┘                  │
│                          FAILED                                        │
│                                                                        │
│  T0 Full Sync Flow:                                                    │
│                                                                        │
│  ┌──────────┐    ┌───────────┐    ┌──────────┐    ┌──────────────┐     │
│  │ 1. Init  │    │ 2. Enable │    │ 3. Snap- │    │ 4. CBT Query │     │
│  │          │    │    CBT    │    │    shot   │    │  changeId="*"│     │
│  │ Find VM  │───►│           │───►│          │───►│              │     │
│  │ Enum     │    │ Reconfig  │    │ T0 snap  │    │ ALL blocks   │     │
│  │ disks    │    │ VM        │    │          │    │ returned     │     │
│  └──────────┘    └───────────┘    └──────────┘    └──────┬───────┘     │
│                                                          │              │
│       ┌──────────────────────────────────────────────────┘              │
│       ▼                                                                │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │  5. PER-DISK LOOP                                                │   │
│  │                                                                  │   │
│  │  For each disk in VM:                                            │   │
│  │  ┌──────────┐   ┌──────────┐   ┌──────────┐   ┌──────────────┐ │   │
│  │  │ Open     │   │ Create   │   │ Pipeline │   │ Finalize     │ │   │
│  │  │ Disk     │──►│ Qcow2    │──►│ Read →   │──►│ raw→qcow2   │ │   │
│  │  │ Reader   │   │ Writer   │   │ Write    │   │ qemu-img     │ │   │
│  │  │ (NBD)    │   │ (raw)    │   │ (4 wkrs) │   │ convert      │ │   │
│  │  └──────────┘   └──────────┘   └──────────┘   └──────┬───────┘ │   │
│  │                                                       │         │   │
│  │       ┌───────────────────────────────────────────────┘         │   │
│  │       ▼                                                         │   │
│  │  ┌──────────┐   ┌──────────┐   ┌──────────────────────────┐    │   │
│  │  │ Save     │   │ Upload   │   │ Save image UUID +        │    │   │
│  │  │ changeId │──►│ qcow2 to │──►│ changeId to BoltDB       │    │   │
│  │  │ locally  │   │ Nutanix  │   │                          │    │   │
│  │  └──────────┘   └──────────┘   └──────────────────────────┘    │   │
│  └──────────────────────────────────────────────────────────────────┘   │
│       │                                                                │
│       ▼                                                                │
│  ┌──────────┐    ┌──────────┐                                          │
│  │ 6. Remove│    │ 7. State │                                          │
│  │ Snapshot │───►│ → SYNC-  │                                          │
│  │          │    │   ING    │                                          │
│  └──────────┘    └──────────┘                                          │
│                                                                        │
│  Progress Tracker:                                                     │
│  ┌──────────────────────────────────────────┐                          │
│  │  47.3% (23.65 GB / 50.00 GB)            │                          │
│  │  @ 125.40 MB/s  ETA: 3m28s              │                          │
│  └──────────────────────────────────────────┘                          │
│                                                                        │
│  Files:                                                                │
│  ├── internal/migration/orchestrator.go (state machine, transitions)   │
│  ├── internal/migration/full_sync.go    (T0 end-to-end)                │
│  ├── internal/migration/progress.go     (tracking, ETA, rate)          │
│  ├── internal/state/journal.go          (block journal)                │
│  ├── internal/cli/plan.go              (plan create/show commands)     │
│  └── internal/cli/migrate.go           (migrate start/status commands) │
└──────────────────────────────────────────────────────────────────────────┘
```

---

## Phase 7: Incremental Sync + Cutover

```
┌──────────────────────────────────────────────────────────────────────────┐
│  PHASE 7: INCREMENTAL SYNC (T1..TN) + CUTOVER                         │
├──────────────────────────────────────────────────────────────────────────┤
│                                                                        │
│  INCREMENTAL SYNC (T1..TN):                                           │
│                                                                        │
│  ┌──────────┐    ┌───────────────┐    ┌─────────────────────────┐      │
│  │ 1. Snap  │    │ 2. CBT Query  │    │ 3. Response             │      │
│  │          │    │               │    │                         │      │
│  │ New      │───►│ changeId =    │───►│ Changed: 847 extents   │      │
│  │ snapshot │    │ "52:aa:bb..." │    │ Total:   2.3 GB        │      │
│  │ TN       │    │ (from last)   │    │ (vs 200 GB full disk)  │      │
│  └──────────┘    └───────────────┘    └──────────┬──────────────┘      │
│                                                   │                     │
│                                                   ▼                     │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │  4. PATCH EXISTING RAW FILE                                      │   │
│  │                                                                  │   │
│  │  Before T1:                                                      │   │
│  │  ┌───┬───┬───┬───┬───┬───┬───┬───┬───┬───┐                      │   │
│  │  │ A │ B │ C │ D │ E │ F │ G │ H │ I │ J │  ← T0 blocks       │   │
│  │  └───┴───┴───┴───┴───┴───┴───┴───┴───┴───┘                      │   │
│  │                                                                  │   │
│  │  CBT says blocks B, D, G changed:                                │   │
│  │  ┌───┬───┬───┬───┬───┬───┬───┬───┬───┬───┐                      │   │
│  │  │ A │B' │ C │D' │ E │ F │G' │ H │ I │ J │  ← after T1 patch  │   │
│  │  └───┴─▲─┴───┴─▲─┴───┴───┴─▲─┴───┴───┴───┘                     │   │
│  │        │       │           │                                     │   │
│  │        └───────┴───────────┘                                     │   │
│  │        Only these 3 blocks read from VMware + written to raw     │   │
│  └──────────────────────────────────────────────────────────────────┘   │
│                                                                        │
│  ┌──────────┐    ┌──────────┐    ┌──────────┐    ┌──────────┐          │
│  │ 5. Re-   │    │ 6. Re-   │    │ 7. Remove│    │ 8. State │          │
│  │ convert  │───►│ upload   │───►│ snapshot │───►│ → CUTOVER│          │
│  │ raw →    │    │ qcow2 to │    │          │    │ _READY   │          │
│  │ qcow2    │    │ Nutanix  │    │          │    │          │          │
│  └──────────┘    └──────────┘    └──────────┘    └──────────┘          │
│                                                                        │
│  Efficiency over time:                                                 │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │  T0  ████████████████████████████████████  100 GB  (full disk)  │   │
│  │  T1  ████████                              5 GB   (daily Δ)    │   │
│  │  T2  ████                                  500 MB (hourly Δ)   │   │
│  │  T3  ██                                    50 MB  (minutes Δ)  │   │
│  │  TN  █                                     5 MB   (final Δ)    │   │
│  └──────────────────────────────────────────────────────────────────┘   │
│                                                                        │
│  CBT Fallback:                                                         │
│  ┌──────────────────────────────────────────┐                          │
│  │  If changeId is invalidated (rare):      │                          │
│  │    → Fall back to changeId="*"           │                          │
│  │    → Full sync for that disk only        │                          │
│  │    → Continue incremental from new ID    │                          │
│  └──────────────────────────────────────────┘                          │
│                                                                        │
├──────────────────────────────────────────────────────────────────────────┤
│                                                                        │
│  CUTOVER FLOW:                                                         │
│                                                                        │
│  Source VM                          Nutanix AHV                        │
│  ┌──────────┐                       ┌──────────────┐                   │
│  │ Running  │   ── Step 1 ──────►   │ Final Δ sync │                   │
│  │          │   (last incremental)  │ uploaded     │                   │
│  │          │                       └──────────────┘                   │
│  │          │                                                          │
│  │          │   ── Step 2 ──────►   (power off source)                 │
│  │ Shutdown │                                                          │
│  │   ░░░░   │                                                          │
│  │          │   ── Step 3 ──────►   ┌──────────────┐                   │
│  │          │   (post-shutdown Δ)   │ Last blocks  │                   │
│  │          │                       │ uploaded     │                   │
│  │          │                       └──────────────┘                   │
│  │  OFF     │                                                          │
│  │          │   ── Step 4 ──────►   ┌──────────────┐                   │
│  └──────────┘   (create VM)         │ VM Created   │                   │
│                                     │ CPU/RAM/NIC  │                   │
│                                     │ disks mapped │                   │
│                                     └──────┬───────┘                   │
│                                            │                           │
│                     Step 5 ───────────►    │                           │
│                     (power on)             ▼                           │
│                                     ┌──────────────┐                   │
│                                     │  RUNNING     │                   │
│                                     │  on Nutanix  │                   │
│                                     │  AHV         │                   │
│                                     └──────────────┘                   │
│                                                                        │
│  Downtime: only Steps 2→5 = typically 2-10 minutes                     │
│                                                                        │
│  VM Mapping at cutover:                                                │
│  ┌──────────────────────┬────────────────────────┐                     │
│  │  VMware              │  Nutanix AHV            │                     │
│  ├──────────────────────┼────────────────────────┤                     │
│  │  4 vCPUs             │  4 vCPUs (NumSockets=4)│                     │
│  │  8 GB RAM            │  8192 MiB              │                     │
│  │  disk-2000 (image)   │  SCSI:0 from image UUID│                     │
│  │  disk-2001 (image)   │  SCSI:1 from image UUID│                     │
│  │  "VM Network"        │  subnet-uuid-abc       │                     │
│  │  "Storage Network"   │  subnet-uuid-def       │                     │
│  └──────────────────────┴────────────────────────┘                     │
│                                                                        │
│  Cleanup command:                                                      │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │  $ datamigrate cleanup --plan web-server-01-plan.yaml            │   │
│  │                                                                  │   │
│  │  Removing VMware snapshots... OK                                 │   │
│  │  Removing staging files in /tmp/datamigrate/web-01-migration... OK│   │
│  │  Removing migration state... OK                                  │   │
│  │                                                                  │   │
│  │  Cleanup complete.                                               │   │
│  └──────────────────────────────────────────────────────────────────┘   │
│                                                                        │
│  Files:                                                                │
│  ├── internal/migration/incremental_sync.go (T1..TN delta sync)        │
│  ├── internal/migration/cutover.go          (final sync + VM create)   │
│  ├── internal/cli/cutover.go                (cutover command)          │
│  └── internal/cli/cleanup.go                (cleanup command)          │
└──────────────────────────────────────────────────────────────────────────┘
```

---

## Phase 8: VDDK Transport + Polish (TODO)

```
┌──────────────────────────────────────────────────────────────────────────┐
│  PHASE 8: VDDK TRANSPORT + POLISH  (⬜ TODO)                           │
├──────────────────────────────────────────────────────────────────────────┤
│                                                                        │
│  CURRENT (Phase 3):              TARGET (Phase 8):                     │
│                                                                        │
│  ┌──────────────┐                ┌──────────────┐                      │
│  │  NBD/NFC     │                │  VDDK        │                      │
│  │  (Pure Go)   │                │  (CGo)       │                      │
│  │              │                │              │                      │
│  │  ESXi ─HTTPS─►  datamigrate  │  ESXi ─SAN──►  datamigrate          │
│  │              │                │       ─HotAdd►                      │
│  │  ~50 MB/s    │                │  ~500 MB/s   │                      │
│  │  Network     │                │  Direct LUN  │                      │
│  │  bound       │                │  or mount    │                      │
│  └──────────────┘                └──────────────┘                      │
│                                                                        │
│  Transport mode selection:                                             │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │  ┌─────────┐                                                     │   │
│  │  │ Config  │──► transport: "nbd"  → NBDReader (current, pure Go)│   │
│  │  │         │──► transport: "vddk" → VDDKReader (Phase 8, CGo)   │   │
│  │  └─────────┘                                                     │   │
│  │                                                                  │   │
│  │  Both implement BlockReader interface — pipeline doesn't change  │   │
│  └──────────────────────────────────────────────────────────────────┘   │
│                                                                        │
│  Other Phase 8 improvements:                                           │
│                                                                        │
│  ┌─────────────────────┐  ┌─────────────────────┐                      │
│  │ Multi-disk parallel │  │ Progress bars        │                      │
│  │                     │  │                     │                      │
│  │ disk-2000 ████░░░░  │  │ [████████░░░░] 65%  │                      │
│  │ disk-2001 ██░░░░░░  │  │ 32.5 GB / 50 GB    │                      │
│  │ disk-2002 ░░░░░░░░  │  │ ETA: 2m15s         │                      │
│  │ (concurrent)        │  │ (progressbar/v3)    │                      │
│  └─────────────────────┘  └─────────────────────┘                      │
│                                                                        │
│  ┌─────────────────────┐  ┌─────────────────────┐                      │
│  │ Per-extent retry    │  │ Integration tests   │                      │
│  │                     │  │                     │                      │
│  │ Block read fail?    │  │ Real vCenter +      │                      │
│  │ → Retry 3x with    │  │ Real Nutanix        │                      │
│  │   exp backoff       │  │ End-to-end verify   │                      │
│  │ → Skip + log if     │  │ qemu-img compare    │                      │
│  │   still fails       │  │                     │                      │
│  └─────────────────────┘  └─────────────────────┘                      │
│                                                                        │
│  Files (to be created):                                                │
│  └── internal/transport/vddk.go  (CGo VDDK bindings)                   │
└──────────────────────────────────────────────────────────────────────────┘
```

---

## End-to-End Data Flow (All Phases Combined)

```
┌─────────────────────────────────────────────────────────────────────────────────────────┐
│  END-TO-END: VMware to Nutanix AHV Migration                                           │
│                                                                                         │
│  VMware vCenter                 datamigrate Machine              Nutanix Prism Central  │
│  ┌──────────────┐               ┌──────────────────┐             ┌──────────────────┐   │
│  │              │               │                  │             │                  │   │
│  │  ┌────────┐  │  Phase 1      │  ┌────────────┐  │             │                  │   │
│  │  │  VMs   │──┼──Discover────►│  │ VM List    │  │             │                  │   │
│  │  └────────┘  │               │  └────────────┘  │             │                  │   │
│  │              │               │        │         │             │                  │   │
│  │              │               │        ▼         │             │                  │   │
│  │              │               │  ┌────────────┐  │             │                  │   │
│  │              │               │  │ Plan YAML  │  │             │                  │   │
│  │              │               │  └────────────┘  │             │                  │   │
│  │              │               │        │         │  Phase 5     │                  │   │
│  │  ┌────────┐  │  Phase 2      │        ▼         │  Validate   │  ┌────────────┐  │   │
│  │  │Snapshot│◄─┼──Snapshot─────│  ┌────────────┐  │────────────►│  │ Connection │  │   │
│  │  │  +CBT  │──┼──CBT Query──►│  │ Extents    │  │             │  │ OK         │  │   │
│  │  └────────┘  │               │  └─────┬──────┘  │             │  └────────────┘  │   │
│  │              │               │        │         │             │                  │   │
│  │  ┌────────┐  │  Phase 3      │        ▼         │             │                  │   │
│  │  │ Disk   │──┼──Blocks──────►│  ┌────────────┐  │             │                  │   │
│  │  │ blocks │  │  (NBD/NFC)    │  │ Pipeline   │  │             │                  │   │
│  │  └────────┘  │               │  │ 4 workers  │  │             │                  │   │
│  │              │               │  └─────┬──────┘  │             │                  │   │
│  │              │               │        │         │             │                  │   │
│  │              │               │        ▼ Phase 4 │             │                  │   │
│  │              │               │  ┌────────────┐  │             │                  │   │
│  │              │               │  │ disk.raw   │  │             │                  │   │
│  │              │               │  │    ▼       │  │             │                  │   │
│  │              │               │  │ disk.qcow2 │  │  Phase 5    │  ┌────────────┐  │   │
│  │              │               │  └─────┬──────┘  │─Upload─────►│  │ Image      │  │   │
│  │              │               │        │         │             │  │ (qcow2)    │  │   │
│  │              │               │        │         │             │  └────────────┘  │   │
│  │              │               │        ▼         │             │                  │   │
│  │              │               │  ┌────────────┐  │             │                  │   │
│  │              │               │  │ state.db   │  │             │                  │   │
│  │              │               │  │ (BoltDB)   │  │             │                  │   │
│  │              │               │  │ changeId   │  │             │                  │   │
│  │              │               │  │ sync count │  │             │                  │   │
│  │              │               │  └────────────┘  │             │                  │   │
│  │              │               │                  │             │                  │   │
│  │  ┌────────┐  │  Phase 7      │  (Repeat T1..TN) │             │  ┌────────────┐  │   │
│  │  │  Δ CBT │──┼──Δ blocks───►│  patch raw       │─Re-upload──►│  │ Updated    │  │   │
│  │  └────────┘  │               │  re-convert      │             │  │ Image      │  │   │
│  │              │               │                  │             │  └────────────┘  │   │
│  │              │               │                  │             │                  │   │
│  │  ┌────────┐  │  Phase 7      │                  │             │  ┌────────────┐  │   │
│  │  │ VM OFF │◄─┼──Shutdown────│  Final Δ sync    │─────────────►│  │ Create VM  │  │   │
│  │  │        │  │  (cutover)    │                  │             │  │ Power ON   │  │   │
│  │  └────────┘  │               │                  │             │  │ ✓ RUNNING  │  │   │
│  │              │               │                  │             │  └────────────┘  │   │
│  └──────────────┘               └──────────────────┘             └──────────────────┘   │
│                                                                                         │
│  ◄──── Source stays running throughout ────►◄─ 2-10 min ─►◄── Target running ──►        │
│                                              downtime                                   │
└─────────────────────────────────────────────────────────────────────────────────────────┘
```

---

## Timeline View

```
Time ──────────────────────────────────────────────────────────────────────────────►

Phase 1   Phase 2      Phase 3+4         Phase 5     Phase 6          Phase 7
discover  snapshot     read+write        upload      orchestrate      sync+cutover
  │         │             │                │             │                │
  ▼         ▼             ▼                ▼             ▼                ▼

  ┌───┐   ┌───┐   ┌───────────────┐   ┌───────┐   ┌───────────┐   ┌─────────────┐
  │ D │   │S+C│   │  T0 Full Copy │   │Upload │   │ State mgmt│   │ T1 Δ  T2 Δ  │
  │ i │   │ B │   │  Block by     │   │qcow2  │   │ Track IDs │   │  ██    █     │
  │ s │   │ T │   │  block read   │   │to     │   │ Journal   │   │             │
  │ c │   │   │   │  + write raw  │   │Nutanix│   │ Progress  │   │ ...    TN Δ │
  │ o │   │   │   │  + convert    │   │       │   │           │   │         █   │
  │ v │   │   │   │               │   │       │   │           │   │             │
  │ e │   │   │   │ ██████████████│   │███████│   │           │   │  Cutover:   │
  │ r │   │   │   │               │   │       │   │           │   │  Shutdown   │
  │   │   │   │   │               │   │       │   │           │   │  Final Δ    │
  │   │   │   │   │               │   │       │   │           │   │  Create VM  │
  │   │   │   │   │               │   │       │   │           │   │  Power ON   │
  └───┘   └───┘   └───────────────┘   └───────┘   └───────────┘   └─────────────┘

Source VM:  ████████████████████████████████████████████████████████████░░░ (off)
Target VM:                                                              ████████ (on)
                                                                      ▲
                                                                  cutover
                                                               (2-10 min)
```

---

## iSCSI Transport vs Image Transport — Network Efficiency

```
┌─────────────────────────────────────────────────────────────────────────────────────────┐
│  TRANSPORT MODE COMPARISON                                                              │
│                                                                                         │
│  iSCSI TRANSPORT (default — --transport iscsi)                                          │
│  ════════════════════════════════════════════                                            │
│                                                                                         │
│  VMware vCenter              datamigrate               Nutanix                          │
│  ┌──────────┐                ┌──────────┐              ┌──────────────────┐              │
│  │          │  CBT blocks    │          │  iSCSI       │  Volume Group    │              │
│  │  T0:     │ ──100 GB────► │ Pass-    │ ──100 GB───► │  ┌────────────┐  │              │
│  │  all     │                │ through  │  WriteAt()   │  │ Disk (LUN) │  │              │
│  │  blocks  │                │ (no      │              │  │            │  │              │
│  │          │  CBT Δ blocks  │  local   │  iSCSI       │  │ Blocks     │  │              │
│  │  T1:     │ ──5 GB──────► │  staging │ ──5 GB─────► │  │ patched    │  │              │
│  │  delta   │                │  needed) │  WriteAt()   │  │ in-place   │  │              │
│  │          │  CBT Δ blocks  │          │  iSCSI       │  │            │  │              │
│  │  T2:     │ ──500 MB────► │          │ ──500 MB───► │  │            │  │              │
│  │  delta   │                │          │  WriteAt()   │  └────────────┘  │              │
│  └──────────┘                └──────────┘              │        │         │              │
│                                                        │        ▼         │              │
│                                                        │  Attach to VM   │              │
│                                                        │  at cutover     │              │
│                                                        └──────────────────┘              │
│                                                                                         │
│  Network transfer: T0=100GB + T1=5GB + T2=500MB + ... = ~106 GB total                  │
│  Local disk used: ZERO (no staging files)                                               │
│                                                                                         │
├─────────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                         │
│  IMAGE TRANSPORT (legacy — --transport image)                                           │
│  ════════════════════════════════════════════                                            │
│                                                                                         │
│  VMware vCenter              datamigrate               Nutanix                          │
│  ┌──────────┐                ┌──────────────┐          ┌──────────────────┐              │
│  │          │  CBT blocks    │ Local disk:  │  Upload  │                  │              │
│  │  T0:     │ ──100 GB────► │ disk.raw     │ ─30 GB─► │  Image (qcow2)  │              │
│  │  all     │                │   ↓ convert  │  full    │                  │              │
│  │  blocks  │                │ disk.qcow2   │  file    │                  │              │
│  │          │  CBT Δ blocks  │              │  Upload  │                  │              │
│  │  T1:     │ ──5 GB──────► │ patch raw    │ ─30 GB─► │  Re-upload      │              │
│  │  delta   │                │   ↓ convert  │  FULL    │  entire qcow2   │              │
│  │          │  CBT Δ blocks  │              │  file    │                  │              │
│  │  T2:     │ ──500 MB────► │ patch raw    │ ─30 GB─► │  Re-upload      │              │
│  │  delta   │                │   ↓ convert  │  AGAIN   │  entire qcow2   │              │
│  └──────────┘                └──────────────┘          └──────────────────┘              │
│                                                                                         │
│  Network transfer: T0=30GB + T1=30GB + T2=30GB + ... = 300 GB total  ← 3x more!        │
│  Local disk used: ~200 GB raw + ~30 GB qcow2                                            │
│                                                                                         │
├─────────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                         │
│  SIDE-BY-SIDE: 10 syncs of a 200 GB disk (50 GB used, 30 GB qcow2)                     │
│                                                                                         │
│  Sync    VMware Read    iSCSI Transfer    Image Transfer    Savings                     │
│  ──────  ───────────    ──────────────    ──────────────    ────────                     │
│  T0      100 GB         100 GB            30 GB             -70 GB *                    │
│  T1        5 GB           5 GB            30 GB             +25 GB                      │
│  T2      500 MB         500 MB            30 GB             +29.5 GB                    │
│  T3      200 MB         200 MB            30 GB             +29.8 GB                    │
│  T4      100 MB         100 MB            30 GB             +29.9 GB                    │
│  T5       80 MB          80 MB            30 GB             +29.9 GB                    │
│  T6       50 MB          50 MB            30 GB             +30.0 GB                    │
│  T7       30 MB          30 MB            30 GB             +30.0 GB                    │
│  T8       20 MB          20 MB            30 GB             +30.0 GB                    │
│  T9       10 MB          10 MB            30 GB             +30.0 GB                    │
│  ──────  ───────────    ──────────────    ──────────────    ────────                     │
│  TOTAL   ~106 GB        ~106 GB           300 GB            194 GB saved                │
│                                                                                         │
│  * T0 iSCSI writes raw blocks (100 GB) vs image uploads compressed qcow2 (30 GB)       │
│    But from T1 onward, iSCSI wins dramatically.                                         │
│                                                                                         │
└─────────────────────────────────────────────────────────────────────────────────────────┘
```

---

## iSCSI Volume Group Flow — Detailed

```
┌──────────────────────────────────────────────────────────────────────────┐
│  iSCSI VOLUME GROUP LIFECYCLE                                          │
├──────────────────────────────────────────────────────────────────────────┤
│                                                                        │
│  SETUP (during migrate start):                                         │
│                                                                        │
│  ┌──────────────┐    ┌──────────────────────┐    ┌──────────────────┐   │
│  │ 1. Create VG │    │ 2. Add disks to VG   │    │ 3. Get iSCSI    │   │
│  │              │    │                      │    │    portal info   │   │
│  │ POST /v3/    │───►│ disk-0: 200 GB       │───►│                  │   │
│  │ volume_groups│    │ disk-1: 100 GB       │    │ Target IQN       │   │
│  │              │    │                      │    │ Portal IP:3260   │   │
│  └──────────────┘    └──────────────────────┘    └────────┬─────────┘   │
│                                                           │             │
│  CONNECT (from datamigrate machine):                      │             │
│                                                           ▼             │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │  iscsiadm -m discovery -t sendtargets -p 10.0.0.100:3260       │   │
│  │  iscsiadm -m node -T iqn.2010-06.com...:vg-name --login        │   │
│  │  → /dev/sdb appears (200 GB LUN)                                │   │
│  │  → /dev/sdc appears (100 GB LUN)                                │   │
│  └──────────────────────────────────────────────────────────────────┘   │
│                                                                        │
│  WRITE BLOCKS (T0 and every T1..TN):                                   │
│                                                                        │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │  file, _ := os.OpenFile("/dev/sdb", O_RDWR|O_SYNC, 0)          │   │
│  │                                                                  │   │
│  │  // Write each changed block at exact offset                     │   │
│  │  file.WriteAt(blockData, offset)   // e.g., offset=8192, 4KB   │   │
│  │  file.WriteAt(blockData, offset)   // e.g., offset=1048576     │   │
│  │  file.WriteAt(blockData, offset)   // Only changed blocks!     │   │
│  │                                                                  │   │
│  │  file.Sync()                                                     │   │
│  └──────────────────────────────────────────────────────────────────┘   │
│                                                                        │
│  CUTOVER:                                                              │
│                                                                        │
│  ┌──────────────┐    ┌──────────────────┐    ┌──────────────────────┐  │
│  │ 1. Final Δ   │    │ 2. Disconnect    │    │ 3. Attach VG to VM  │  │
│  │    sync      │    │    iSCSI from    │    │                      │  │
│  │    (few MB)  │───►│    datamigrate   │───►│  PUT /v3/volume_     │  │
│  │              │    │    machine       │    │  groups/{id}         │  │
│  │              │    │                  │    │  attachment: vm-uuid │  │
│  └──────────────┘    └──────────────────┘    └──────────┬───────────┘  │
│                                                         │              │
│                                                         ▼              │
│                                              ┌──────────────────────┐  │
│                                              │ 4. Create VM         │  │
│                                              │    (CPU/RAM/NICs)    │  │
│                                              │    Disks from VG     │  │
│                                              │                      │  │
│                                              │ 5. Power ON          │  │
│                                              │    VM boots on AHV   │  │
│                                              └──────────────────────┘  │
│                                                                        │
│  Files:                                                                │
│  ├── internal/nutanix/volume_group.go  (Create/Get/Delete/Attach VG)  │
│  ├── internal/blockio/iscsi.go         (ISCSIWriter: connect, write)  │
│  └── internal/migration/full_sync.go   (transport mode selection)      │
└──────────────────────────────────────────────────────────────────────────┘
```

## Data Path Comparison — Local Disk vs Datastore vs iSCSI

```
OPTION A: Image Transport + Local Disk (what image mode does)
──────────────────────────────────────────────────────────────
  VMware                    Migration Machine              Nutanix
  ┌──────┐   blocks        ┌──────────────┐   upload     ┌──────────┐
  │ ESXi │ ═══════════════►│ /staging/    │ ════════════►│ Image    │
  │      │                 │  disk.raw    │   30 GB      │ Service  │
  │      │                 │  disk.qcow2  │   EACH TIME  │          │
  └──────┘                 └──────────────┘              └──────────┘
                            ▲
                            │ Needs local disk space
                            │ Double I/O: write + read
                            │ qemu-img convert CPU cost

OPTION B: Image Transport + VMware Datastore (NOT recommended)
──────────────────────────────────────────────────────────────
  VMware                                                   Nutanix
  ┌──────┐   blocks        ┌──────────────┐   upload     ┌──────────┐
  │ ESXi │ ═══════════════►│ Datastore    │ ════════════►│ Image    │
  │      │                 │ [VMFS/NFS]   │   30 GB      │ Service  │
  │      │                 │ disk.raw     │   EACH TIME  │          │
  └──────┘                 └──────────────┘              └──────────┘
                            ▲
                            │ Consumes production storage
                            │ Competes with VM IOPS
                            │ Still re-uploads full file each sync
                            │ Needs ESXi file manager API access

OPTION C: iSCSI Transport (DEFAULT — no intermediate storage)
──────────────────────────────────────────────────────────────
  VMware                    Migration Machine              Nutanix
  ┌──────┐   blocks        ┌──────────────┐   iSCSI      ┌──────────┐
  │ ESXi │ ═══════════════►│ Passthrough  │ ════════════►│ Volume   │
  │      │                 │ (no disk I/O)│   DELTA ONLY │ Group    │
  │      │                 │              │              │          │
  └──────┘                 └──────────────┘              └──────────┘
                            ▲
                            │ Zero local disk usage
                            │ Only changed blocks cross the wire
                            │ Data already on Nutanix (RF2/RF3)
                            │ No qemu-img, no conversion

  Network transfer comparison (100 GB VM, 10 sync cycles):
  ┌──────────────────┬────────────────────┬──────────────────┐
  │ Option A (local) │ Option B (dstore)  │ Option C (iSCSI) │
  ├──────────────────┼────────────────────┼──────────────────┤
  │ T0:  100 GB      │ T0:  100 GB        │ T0:  100 GB      │
  │ T1:   30 GB      │ T1:   30 GB        │ T1:    5 GB      │
  │ T2:   30 GB      │ T2:   30 GB        │ T2:    2 GB      │
  │ ...              │ ...                │ ...              │
  │ T10:  30 GB      │ T10:  30 GB        │ T10: 500 MB      │
  ├──────────────────┼────────────────────┼──────────────────┤
  │ Total: ~400 GB   │ Total: ~400 GB     │ Total: ~110 GB   │
  └──────────────────┴────────────────────┴──────────────────┘
```

---

## Where Do T0, T1..TN Go? (Simple View)

```
═══════════════════════════════════════════════════════════════════════
  IMAGE TRANSPORT — Step by step, in plain English
═══════════════════════════════════════════════════════════════════════

  T0 (Full Copy):
  ┌──────────┐       ┌──────────────────┐       ┌───────────────────┐
  │ VMware   │ copy  │ Your Machine     │upload │ Nutanix Images    │
  │ VM Disk  │──────►│ disk.raw (100GB) │──────►│ disk.qcow2 (30GB) │
  │ (100 GB) │ all   │ disk.qcow2(30GB) │ full  │                   │
  └──────────┘ blocks└──────────────────┘ file  └───────────────────┘
                                                  ✅ Can boot VM now!

  T1 (Delta):
  ┌──────────┐       ┌──────────────────┐       ┌───────────────────┐
  │ VMware   │ copy  │ Your Machine     │upload │ Nutanix Images    │
  │ Changed  │──────►│ Patch disk.raw   │──────►│ REPLACE qcow2     │
  │ blocks   │ 5 GB  │ (overwrite 5 GB  │ 30 GB │ (still 30 GB)     │
  │ only     │       │  in 100 GB file) │ FULL! │                   │
  └──────────┘       └──────────────────┘       └───────────────────┘
                      raw file is now                ✅ Can boot VM!
                      T0 + T1 merged                 Disk is complete

  T2 (Delta):
  ┌──────────┐       ┌──────────────────┐       ┌───────────────────┐
  │ VMware   │ copy  │ Your Machine     │upload │ Nutanix Images    │
  │ Changed  │──────►│ Patch disk.raw   │──────►│ REPLACE qcow2     │
  │ blocks   │500 MB │ (overwrite 500MB │ 30 GB │ (still 30 GB)     │
  │ only     │       │  in 100 GB file) │ FULL! │                   │
  └──────────┘       └──────────────────┘       └───────────────────┘
                      raw file is now                ✅ Can boot VM!
                      T0 + T1 + T2 merged            Disk is complete

  KEY POINTS:
  • Delta is NOT stored separately — it's merged into the raw file
  • Nutanix Images always has ONE complete qcow2 — never delta files
  • You re-upload the FULL 30 GB qcow2 every time (wasteful but simple)
  • You can create a VM from the image and boot it at ANY stage

═══════════════════════════════════════════════════════════════════════
  iSCSI TRANSPORT (default) — Step by step, in plain English
═══════════════════════════════════════════════════════════════════════

  T0 (Full Copy):
  ┌──────────┐              ┌───────────┐            ┌─────────────────┐
  │ VMware   │  read all    │ Your      │  iSCSI     │ Nutanix Volume  │
  │ VM Disk  │─────────────►│ Machine   │───────────►│ Group           │
  │ (100 GB) │   blocks     │ (no files │  write at  │ ┌─────────────┐ │
  │          │              │  on disk) │  exact     │ │ Disk 100 GB │ │
  └──────────┘              └───────────┘  offsets   │ │ [■■■■■■■■■] │ │
                                                      │ │ complete    │ │
                                                      │ └─────────────┘ │
                                                      └─────────────────┘
                                                        ✅ Can boot VM!

  T1 (Delta):
  ┌──────────┐              ┌───────────┐            ┌─────────────────┐
  │ VMware   │  read 5 GB   │ Your      │  iSCSI     │ Nutanix Volume  │
  │ Changed  │─────────────►│ Machine   │───────────►│ Group           │
  │ blocks   │  delta only  │ (just a   │  write     │ ┌─────────────┐ │
  │          │              │  pipe)    │  5 GB at   │ │ [■■░■■░■■■] │ │
  └──────────┘              └───────────┘  offsets   │ │  ↑   ↑       │ │
                                                      │ │  changed    │ │
                                                      │ │  blocks     │ │
                                                      │ └─────────────┘ │
                                                      └─────────────────┘
                                                        ✅ Can boot VM!
                                                        Disk is complete
                                                        (old + new merged)

  T2 (Delta):
  ┌──────────┐              ┌───────────┐            ┌─────────────────┐
  │ VMware   │  read 500MB  │ Your      │  iSCSI     │ Nutanix Volume  │
  │ Changed  │─────────────►│ Machine   │───────────►│ Group           │
  │ blocks   │  delta only  │ (just a   │  500 MB    │ ┌─────────────┐ │
  │          │              │  pipe)    │  at exact  │ │ [■■■■░■■■■] │ │
  └──────────┘              └───────────┘  offsets   │ │      ↑       │ │
                                                      │ │   changed   │ │
                                                      │ └─────────────┘ │
                                                      └─────────────────┘
                                                        ✅ Can boot VM!

  KEY POINTS:
  • No files on your machine — blocks pass through like a pipe
  • No raw file, no qcow2, no conversion
  • Only changed blocks go over the network (5 GB, 500 MB, etc.)
  • Writing at offset X overwrites what was there — that IS the merge
  • Volume Group disk is always complete and bootable
  • At cutover: detach iSCSI → attach VG to VM → power on

═══════════════════════════════════════════════════════════════════════
  HOW TO TEST-BOOT AT ANY STAGE
═══════════════════════════════════════════════════════════════════════

  Image mode:
  ┌──────────────────┐     ┌──────────────────┐     ┌────────────────┐
  │ Prism Central    │     │ Create VM        │     │ Power On       │
  │ → Images         │────►│ → Add Disk       │────►│ → VM boots!    │
  │ → Find qcow2    │     │ → Clone from     │     │ → Test it      │
  │                  │     │   Image          │     │ → Delete when  │
  │                  │     │ → Set CPU/RAM    │     │   done testing │
  └──────────────────┘     └──────────────────┘     └────────────────┘

  iSCSI mode:
  ┌──────────────────┐     ┌──────────────────┐     ┌────────────────┐
  │ Pause migration  │     │ Attach VG to     │     │ Power On       │
  │ (disconnect      │────►│ a test VM in     │────►│ → VM boots!    │
  │  iSCSI from your │     │ Prism Central    │     │ → Test it      │
  │  machine)        │     │                  │     │ → Detach VG    │
  │                  │     │                  │     │ → Resume sync  │
  └──────────────────┘     └──────────────────┘     └────────────────┘
```
