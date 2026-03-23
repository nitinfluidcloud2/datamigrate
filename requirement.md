Requirement:
Create a tool like nutanix move which can migrate VMs from VMware to Nutanix AHV.

Design: 
Should be like veeam backup & replication where the downtime is negligible for migration and cutover.



Here’s a detailed comparison table showing how Nutanix Move translates VMware vSphere objects into Nutanix AHV (Acropolis Hypervisor) equivalents during a VM migration:

 

🧩 Nutanix Move: VMware to AHV Object Mapping Table
VMware vSphere Object

Nutanix AHV Equivalent

Notes / Behavior During Migration

Virtual Machine (VM)

VM on AHV

Core migration object. Nutanix Move converts the VM disk format and metadata.

vCPU / Memory

vCPU / Memory on AHV VM

Retained as-is; same count and allocation unless explicitly changed during migration config.

VMDK (VM Disk File)

AHV Disk Image (qcow2 or raw)

VMDKs are converted to compatible formats (usually qcow2) using Nutanix Move.

VM Network Adapter (vNIC)

vNIC on AHV VM

Each VMware vNIC becomes an AHV NIC; user maps each to a Nutanix AHV network (subnet).

Port Group (vSwitch / DVS)

AHV Network (Subnet)

You must manually map port group to AHV subnet during migration. No auto-mapping.

MAC Address (manual or auto)

MAC Address (auto-assigned by AHV)**

AHV assigns new MACs unless static MAC reuse is configured.

VM Folder

Not applicable (AHV doesn’t use folders)

Folders are not recreated; use categories or projects in Prism Central instead.

Resource Pool

AHV doesn’t support Resource Pools

Use categories, projects, or placement policies instead.

Cluster

AHV Cluster

VMs are migrated into a target AHV cluster.

Datastore (VMFS/NFS)

AHV Storage Container

VMs are written to a Nutanix container (using storage pool). No direct mapping to VMFS/NFS.

Snapshots (vSphere VM Snapshots)

Not migrated

Snapshots are not preserved; migrate only the live or powered-off state.

VM Tools / Guest Customization

Nutanix Guest Tools (NGT)

VMware Tools are removed. You can install NGT post-migration for enhanced guest integration.

Affinity / Anti-affinity Rules

AHV Affinity Rules (manually recreated)

Not auto-migrated. Must be recreated manually in Prism Central.

vApp metadata / IP Pooling

Not applicable

vApp concepts are not used in Nutanix AHV.

Cluster DRS / HA settings

AHV Acropolis Dynamic Scheduler (ADS) / High Availability

You can configure similar behavior using AHV ADS and HA policies post-migration.

 

Important Notes
Manual Mapping Required: Nutanix Move does not auto-map port groups to subnets or datastores to containers. You must define target networks and storage during the migration configuration.

No Live Snapshots: Snapshots in VMware are not migrated. You must flatten or discard them before the Move process.

Guest OS Customization: Nutanix Move supports basic guest customization, but full vCenter customization specs are not transferred.

Unsupported Constructs: NSX-T configurations (DFW, NAT, LB), vDS advanced features, or SRM configurations are not migrated and need manual recreation.

 

VMWare + NSX-T to Nutanix
There is no fully automated tool today — from Nutanix or any third party — that provides end-to-end automatic translation of VMware NSX-T constructs (like Tier-0/Tier-1 gateways, segments, DFW rules, NAT, LB, etc.) into Nutanix networking (like AHV subnets, Flow microsegmentation, policies, gateways, or load balancers).

No: There is no tool that automatically converts NSX-T to Nutanix networking.

You must manually re-architect and recreate the networking and security policies.

🧱 Why No Tool Exists Yet
NSX-T is a feature-rich SDN platform with complex constructs like:

Tier-0 / Tier-1 gateways

Overlay and VLAN segments

Distributed Firewall (DFW) rules with L3/L4/L7 support

Service insertion, NAT/DNAT/SNAT rules

Logical routing

Identity Firewall (User/Group-based rules)

Distributed IDS/IPS

Load balancers (L4 and L7)

By contrast, Nutanix does support network virtualization, but with a more cloud-like or simplified model:

L2 subnets

L3 gateways (Edge VM or Virtual Router)

Flow microsegmentation (DFW-lite equivalent)

SNAT via gateway policies

Basic load balancing (optional, or via external devices)

The two ecosystems differ semantically, architecturally, and operationally.


additional context
VMware → Hyper-V Migration
Deep Technical Understanding of Veeam & Zerto Internals (Block-Level Replication)
1. Purpose of This Document
 

This document explains how VMware → Hyper-V like migration tools actually work internally, focusing on:

Veeam and Zerto

Block-level replication

VMware CBT (Changed Block Tracking)

Snapshot mechanics

Datastore I/O paths

Why VMDK files are never copied

How a single final VHDX is created

This is written for architects and engineers, not marketing.

2. Key Principle (Very Important)
Neither Veeam nor Zerto migrates VMDK files.

They migrate disk blocks, reconstructed into a new disk image.

 

A VMDK is:

A metadata container

Backed by blocks on VMFS / NFS / vSAN

Both tools operate below the file level, at the block address level.

3. VMware Storage Model (Foundation)
A VMware virtual disk is logically:



Virtual Disk (e.g., 200 GB)
---------------------------------
Block 0
Block 1
Block 2
...
Block N
Physically stored as:

disk.vmdk (descriptor)

disk-flat.vmdk (block extents on datastore)

Optional snapshot redo logs

Applications NEVER see these files.

4. What VMware CBT Really Is
CBT (Changed Block Tracking)
CBT is metadata only.

It answers:



Which block ranges changed since checkpoint X?
CBT DOES NOT contain:

Disk data

Delta files

Snapshot contents

CBT provides:



[(offset, length), (offset, length), ...]
Example:



Changed blocks since last run:
  (891, 64KB)
  (23001, 64KB)
  (992341, 128KB)
5. Veeam Internals — Step-by-Step
5.1 Veeam Components
Veeam Backup Server

Veeam Proxy

Veeam Repository

VMware ESXi (CBT provider)

❌ No agent inside guest VM

❌ No kernel modules in guest

6. Veeam Timeline — FIRST FULL BACKUP (T0)
Step 0 — VM & Disk Discovery
Pseudo VMware API calls:



FindByUuid()
RetrieveProperties(VirtualMachine.config.hardware.device)
Purpose:

Identify VM

Identify all virtual disks

Identify datastore backing each disk

Step 1 — Create Snapshot (Consistency Point)
Veeam requests a snapshot:



CreateSnapshot_Task(
  quiesce = optional,
  memory  = false
)
Key points:

Memory is NOT captured

Snapshot is for disk consistency only

VM continues running

VMware creates:

Snapshot metadata

Delta redo logs (internally)

Step 2 — Enable or Validate CBT
 



ReconfigVM_Task(changeTrackingEnabled = true)
CBT initializes block change tracking per disk.

Step 3 — Select Transport Mode
Veeam chooses how to read blocks:

HotAdd

SAN

NBD / NBDSSL

This decides how datastore blocks are accessed, not what is accessed.

Step 4 — Read Disk Blocks (Important)
Veeam does NOT copy disk-flat.vmdk.

Instead:

Pseudo:



OpenVirtualDisk(snapshotDiskRef)
for block in 0..N:
    ReadDiskBlocks(offset, length)
Data path:



Datastore → ESXi → Veeam Proxy
Blocks are streamed sequentially.

Step 5 — Build 
full.vbk
Each block is written into a Veeam container:



VBK.write(block_number, block_data)
full.vbk is:

A logical disk image

Deduplicated

Compressed

Indexed

❌ Not mountable

❌ Not a VMDK

 

Step 6 — Remove Snapshot
 



RemoveSnapshot_Task()
VMware merges redo logs back into base disk.

 

7. Veeam Incremental Backups (T1, T2…)
 

Step 1 — Snapshot Again
Same snapshot flow as T0.

 

Step 2 — Query CBT
 



QueryChangedDiskAreas(diskId, changeId)
Returns changed extents only.

Step 3 — Read ONLY Changed Blocks
 



for extent in changed_extents:
    ReadDiskBlocks(extent.offset, extent.length)
 

Step 4 — Write 
.vib
 



VIB.append(offset, block_data)
.vib contains block patches, not disk files.

 

Step 5 — Remove Snapshot
Same as T0.

8. How Veeam Creates the Final Disk (Restore)
Restore Algorithm
 



Create empty target disk (VMDK or VHDX)
Apply blocks from full.vbk
Apply block patches from vib1, vib2, vibN (in order)
Finalize disk
Result:



ONE clean VHDX
No delta disks. No snapshot chains.

9. Why Veeam Never Copies VMDK Files
 

Reasons:

VMDKs are not atomic files

Snapshot chains are fragile

Block replay is faster

Restore target may not be VMware

Backup efficiency (dedupe/compression)

10. Zerto Internals — Step-by-Step
10.1 Zerto Components
Zerto Virtual Manager (ZVM)

Zerto VRA (Virtual Replication Appliance) per ESXi host

Journal storage

❌ No agent inside guest VM

✔ Kernel-level interception inside ESXi

 

11. Zerto Replication Flow
Step 1 — Base Disk Sync
Zerto performs a full initial copy:



Read datastore blocks → write base disk on target
This creates the conceptual equivalent of DISK[T0].

Step 2 — Continuous Write Interception
Zerto VRA hooks into ESXi I/O path.

 

When VM writes:



Write block X
Zerto does:



Intercept write
Copy block
Send to target
Store in journal
This happens in real time.

No snapshots.

12. Zerto Journal Mechanics
Journal stores:



(time, block_number, block_data)
This allows:

Point-in-time recovery

Replay to any second

Journal is NOT a disk file chain.

13. Zerto Cutover
At cutover:

Pause VM

Replay final journal entries

Finalize disk

Boot VM on target

Output:



ONE merged VHDX
14. Memory Handling (Important Clarification)
Neither Veeam nor Zerto:

 

Migrates live RAM

Preserves memory state

 

 

They provide:

 

Crash-consistent disks

Cold boot on target

15. Why Neither Tool Exposes Delta Disks
 

Exposing:



base.vmdk + delta1.vmdk + delta2.vmdk
Would cause:

Ordering problems

Corruption risk

User-managed merges

Support nightmares

 

So vendors hide deltas and replay blocks internally.

16. Final Comparison
Feature

Veeam

Zerto

Block capture

Snapshot + CBT

Write interception

Snapshot usage

Yes

No

Delta format

.vib

Journal

Guest agents

No

No

Delta disks exposed

No

No

Final output

Single VHDX

Single VHDX

Downtime

Minutes–hours

Seconds

 

17. Final Takeaways (Read This Twice)
CBT is metadata, not data

Datastore is the source of truth

Veeam/Zerto replay blocks, not files

VMDKs are never copied

Final output is always one clean disk

Memory is not migrated

This is why these tools are reliable and fast

18. One-Line Mental Model
 

CBT tells WHAT changed.

ESXi provides the bytes.

Veeam/Zerto replay blocks.

One final disk is written.

 

Veeam Pipeline--



+----------------------+
|   Guest VM (Linux)   |
|  App writes to disk  |
+----------+-----------+
           |
           v
+----------------------+
|     ESXi I/O Path    |
|  (virtual SCSI ctrl) |
+----------+-----------+
           |
           |  Snapshot created (T0)
           |  (redo logs isolate writes)
           v
+----------------------+
|     CBT Metadata     |
|  "These blocks changed" 
+----------+-----------+
           |
           |  QueryChangedDiskAreas()
           v
+----------------------+
|  ESXi Datastore I/O  |
|  VMFS / NFS / vSAN   |
+----------+-----------+
           |
           |  ReadDiskBlocks(offset,len)
           v
+----------------------+
|   Veeam Proxy        |
|  (HotAdd/SAN/NBD)    |
+----------+-----------+
           |
           |  Block stream
           v
+----------------------+
|  Veeam Containers    |
|  full.vbk / inc.vib  |
+----------------------+
           |
           |  Restore / Replay
           v
+----------------------+
|  Final VHDX Disk     |
|  (single merged)     |
+----------------------+
 

Key properties
Snapshot used only for consistency

CBT gives block map, not data

Datastore is source of bytes

Final disk is written once

 

 

Zerto



+----------------------+
|   Guest VM (Linux)   |
|  App writes to disk  |
+----------+-----------+
           |
           v
+------------------------------+
|     ESXi I/O Path            |
|  (write intercepted inline)  |
+----------+-------------------+
           |
           |  Zerto VRA hooks here
           v
+------------------------------+
|  Zerto VRA (on ESXi host)    |
|  Copy block immediately      |
+----------+-------------------+
           |
           |  Stream block
           v
+------------------------------+
|  Zerto Journal (Target)      |
|  (time, block, data)         |
+----------+-------------------+
           |
           |  Replay journal
           v
+------------------------------+
|  Final VHDX Disk             |
|  (already mostly complete)   |
+------------------------------+
 

Key properties
No snapshots

No CBT dependency

Continuous replication

Disk is almost finished before cutover

 

 

 

5.2 DIY — Why It Breaks at Scale
 

What DIY lacks fundamentally:

No reliable block map

No ordered delta replay

No crash-safe merge logic

No journal

 

DIY delta idea (theoretically):



vmdk_T0 + diff_T1 + diff_T2 → merge
 

Problems:

 

VMware snapshots ≠ clean deltas

Ordering matters

Metadata corruption risk

Manual merge failures

Extremely hard to automate safely

 

 

DIY works only when:

 

VM is powered off

Downtime is acceptable

Disk size is small

 

 

 

 

5.3 Veeam — Engineering Strength
 

 

What Veeam gives you:

 

Deterministic block replay

CBT-verified deltas

Snapshot safety

Portable restore targets

 

 

What it trades off:

 

Snapshot overhead

Minutes–hours downtime

CBT fragility (rare but real)

 

 

Engineering sweet spot:

 

Batch migrations

Cost-sensitive environments

Predictable cutover windows

 

 

 

 

5.4 Zerto — Engineering Strength
 

 

What Zerto gives you:

 

Write-path interception

Continuous deltas

No snapshots

Second-level RPO

 

 

What it trades off:

 

Cost

ESXi host footprint (VRA)

Operational overhead

 

 

Engineering sweet spot:

 

Production workloads

Large disks

Near-zero downtime

 

 

 

6. Why Vendors Hide Delta Disks (Critical Insight)
 

If users were given:



base.vmdk
delta1.vmdk
delta2.vmdk
 

They would:

Merge in wrong order

Corrupt filesystems

Lose data

Blame vendor

 

So vendors:

Track deltas internally

Replay blocks algorithmically

Emit one final disk

This is intentional and correct.

 

7. Final Engineering Truth (Read This Twice)
 

DIY operates at file level

Veeam operates at block replay level

Zerto operates at write-stream level

 

That’s why:

DIY is fragile

Veeam is safe

Zerto is fast

 

8. One-Line Mental Model
DIY    → copy files
Veeam  → replay blocks
Zerto  → mirror writes

 

Important timeline



Time ───────────────────────────────────────────────►
Base disk copy:   ██████████████████████████████████▶
Delta capture:        ░░░░░░░░░░░░░░░░░░░░░░░░░░░░░▶
                   (starts early, runs continuously)
Cutover:                                           ▲
                                                   VM boots

                                                   