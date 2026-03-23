# Nutanix VM Internet Connectivity — After New IPFO Block

## Network Diagram

```
                              INTERNET
                                 |
                    +------------+------------+
                    |    OVH BACKBONE         |
                    |    (routing, anti-DDoS)  |
                    +------------+------------+
                                 |
              +------------------+------------------+
              |                                     |
    +---------+---------+             +-------------+-----------+
    | Existing IPFO     |             | NEW IPFO Block          |
    | 51.81.192.212/30  |             | x.x.x.0/29             |
    | GW: .214          |             | GW: x.x.x.6 (OVH)     |
    | Usable: .213 only |             | Usable: .1 .2 .3 .4 .5 |
    +---------+---------+             +-------------+-----------+
              |                                     |
    ==========+=====================================+============
    |                  vRack pn-2014805              |           |
    |                  (Layer 2 switch)              |           |
    |                                                |           |
    |  +---------------------------------------------+--------+ |
    |  |           Nutanix AHV Cluster (vs0)                  | |
    |  |                                                      | |
    |  |  VLAN 0 (untagged)          VLAN 0 (untagged)        | |
    |  |  +---------------------+   +----------------------+  | |
    |  |  | OVH-IPFO-Uplink-    |   | NEW-IPFO-Subnet      |  | |
    |  |  | Subnet              |   | (new external subnet) |  | |
    |  |  | 51.81.192.212/30    |   | x.x.x.0/29           |  | |
    |  |  | Pool: .213          |   | Pool: .1 .2 .3 .4 .5 |  | |
    |  |  +----------+----------+   +----------+-----------+  | |
    |  |             |                         |               | |
    |  |         CVMs use this          VPC SNAT uses this     | |
    |  |         (unchanged)            (NEW - for VMs)        | |
    |  |             |                         |               | |
    |  |  +----------+----------+   +----------+-----------+   | |
    |  |  | CVM 1  CVM 2  CVM 3|   | ovh-external-vpc     |   | |
    |  |  | Internet via .213   |   | SNAT IP: x.x.x.1    |   | |
    |  |  | (no changes here)   |   | (outbound NAT)       |   | |
    |  |  +---------------------+   +----------+-----------+   | |
    |  |                                       |               | |
    |  |                            +----------+-----------+   | |
    |  |                            | OVH-External-Subnet  |   | |
    |  |                            | 172.16.0.0/24        |   | |
    |  |                            | (overlay inside VPC) |   | |
    |  |                            +----------+-----------+   | |
    |  |                                       |               | |
    |  |  VLAN 1                    +----------+-----------+   | |
    |  |  +---------------------+   |                      |   | |
    |  |  | infra_pb6bh         |   |   Migration VM       |   | |
    |  |  | 172.16.0.0/22       |   |   (dual NIC)         |   | |
    |  |  | (CVM/host infra)    |   |                      |   | |
    |  |  |                     |   |   NIC 1: infra_pb6bh |   | |
    |  |  | Data Services IP    +---+   172.16.0.50/22     |   | |
    |  |  | 172.16.3.254:3260   |   |   --> iSCSI access   |   | |
    |  |  |                     |   |                      |   | |
    |  |  +---------------------+   |   NIC 2: OVH-Ext-Sub |   | |
    |  |                            |   172.16.0.60/24     |   | |
    |  |                            |   --> internet (SNAT) |   | |
    |  |                            |   --> vCenter access  |   | |
    |  |                            +----------------------+   | |
    |  +-------------------------------------------------------+ |
    ==============================================================
```

## Traffic Flows

### CVM Internet (unchanged)
```
CVM --> 51.81.192.213 --> OVH GW 51.81.192.214 --> Internet
        (existing IPFO)
        NO CHANGES NEEDED
```

### VM Internet (NEW — via VPC SNAT)
```
Migration VM (172.16.0.60)
    --> OVH-External-Subnet (overlay, inside VPC)
    --> ovh-external-vpc SNAT
    --> x.x.x.1 (new IPFO)
    --> OVH GW x.x.x.6
    --> Internet / vCenter
```

### VM iSCSI (unchanged)
```
Migration VM (172.16.0.50)
    --> infra_pb6bh (VLAN 1, vs0)
    --> 172.16.3.254:3260 (Data Services IP)
    --> Nutanix Volume Group
    SAME L2 -- no routing needed
```

## Migration Tool Data Flow (End State)

```
                    Migration VM on Nutanix
                    (dual NIC: infra + internet)
                           |
              +------------+------------+
              |                         |
         NIC 1 (infra)            NIC 2 (overlay/VPC)
         172.16.0.50/22           172.16.0.60/24
              |                         |
    +---------+---------+    +----------+-----------+
    |                   |    |                      |
    | iSCSI WriteAt     |    | vSphere API (443)    |
    | 172.16.3.254:3260 |    | via SNAT x.x.x.1    |
    | (Nutanix VG)      |    | --> vCenter          |
    |                   |    |                      |
    | Prism API (9440)  |    | Prism API (9440)     |
    | 172.16.1.99       |    | (also reachable)     |
    +-------------------+    +----------------------+

    datamigrate reads blocks        datamigrate writes blocks
    from VMware via NIC 2    -->    to Nutanix VG via NIC 1
```

## Setup Steps (After New IPs Arrive)

```
Step 1: OVH Manager
        - Order /29 IPFO block
        - Add new block to vRack pn-2014805

Step 2: Prism Central - Create New External Subnet
        - Name: "New-IPFO-Subnet"
        - Type: VLAN, VLAN ID: 0, Virtual Switch: vs0
        - Network: x.x.x.0/29
        - Gateway: x.x.x.6 (OVH gateway for the new block)
        - IP Pool: x.x.x.1 - x.x.x.5

Step 3: Prism Central - Update VPC
        - Edit ovh-external-vpc
        - Associate New-IPFO-Subnet as external subnet
        - Set SNAT IP to x.x.x.1 (from new block)
        - Keep or remove old OVH-IPFO-Uplink-Subnet association

Step 4: Test VM Internet
        - Attach VM to OVH-External-Subnet (overlay, inside VPC)
        - ping 8.8.8.8  --> should work via SNAT

Step 5: Create Migration VM (dual NIC)
        ** Cannot mix Basic + Advanced NICs on same VM **
        Option A: Use two Advanced/Overlay NICs
          - NIC 1: New overlay subnet on 172.16.0.0/22 inside VPC
                   (but iSCSI needs same L2 as infra — may not work)
          - NIC 2: OVH-External-Subnet for internet

        Option B: Use infra_pb6bh (Basic) only + route internet via CVM
          - If CVM can forward traffic after new IPs fix routing

        ** NOTE: This Basic vs Advanced NIC conflict needs resolution **
        ** May need to create infra as Advanced subnet or find workaround **

Step 6: Deploy datamigrate binary
        - Build: GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o datamigrate-linux ./cmd/datamigrate
        - Transfer via Prism image API or SCP

Step 7: Test iSCSI end-to-end
        - datamigrate migrate start --plan ubuntu-vm-plan.yaml --transport iscsi
```

## IMPORTANT: NIC Type Conflict

Nutanix does NOT allow mixing Basic (VLAN) and Advanced (Overlay) NICs on the same VM.
- `infra_pb6bh` = Basic (needed for iSCSI, same L2 as 172.16.3.254)
- `OVH-External-Subnet` = Overlay/Advanced (needed for internet via VPC SNAT)

**Possible solutions:**
1. Recreate infra subnet as Advanced/Overlay type (risky — may break CVM infra)
2. Create a new VLAN subnet on vs0 with the new IPFO block (Basic type, with gateway) — then both NICs are Basic type
3. Use a router VM approach — one VM does NAT, other VMs route through it
4. Check if Nutanix allows a Basic VLAN subnet with external connectivity directly (no VPC needed)
