# OVH Nutanix Networking Guide

## Overview

This document explains how OVH networking (vRack, IPFO, public IPs) connects to Nutanix AHV and VMware vSphere clusters, and how to troubleshoot internet connectivity issues.

## Architecture Diagram

```
                          +---------------------+
                          |      INTERNET        |
                          +----------+-----------+
                                     |
                          +----------+-----------+
                          |   OVH BACKBONE /      |
                          |   EDGE NETWORK        |
                          |  (routing, anti-DDoS,  |
                          |   DNS 213.186.33.99)   |
                          +----------+-----------+
                                     |
                 +-------------------+-------------------+
                 |                   |                   |
        +--------+--------+  +------+-------+  +--------+--------+
        |  Public IPs     |  |  Public IPs  |  |  Public IP      |
        | 15.204.37.80/28 |  | 15.204.x.x  |  | 51.81.192.212/30|
        | (VMware IPFO)   |  | (Dedicated   |  | (Nutanix IPFO)  |
        | -> pcc-147-135  |  |  Servers)    |  | -> must be in   |
        +--------+--------+  +------+-------+  |    vRack!       |
                 |                   |          +--------+--------+
                 |                   |                   |
    =============+===================+===================+============
    |            |    vRack pn-2014805 (Layer 2 switch)  |           |
    |            |                   |                   |           |
    |  +---------+------------------+|  +----------------+--------+ |
    |  |  VMware vSphere            ||  |  Nutanix AHV Cluster    | |
    |  |  pcc-147-135-35-91         ||  |  cluster-1919 ("fc")    | |
    |  |                            ||  |                         | |
    |  |  +------+ +------+        ||  |  +------++------++-----+| |
    |  |  |ESXi 1| |ESXi 2|  ...  ||  |  |Node 1||Node 2||Node3|| |
    |  |  |      | |      |       ||  |  |.0.1  ||.0.2  ||.0.3 || |
    |  |  +--+---+ +--+---+       ||  |  +--+---++--+---++--+--+| |
    |  |     |  vSwitch|           ||  |     |  vs0  |       |   | |
    |  |  +--+--------+--+        ||  |  +--+-------+-------+-+ | |
    |  |  | VM Network   |        ||  |  | 172.16.0.0/22       | | |
    |  |  | 15.204.37.x  |        ||  |  | (CVM + AHV private) | | |
    |  |  +--------------+        ||  |  |                     | | |
    |  |                          ||  |  | Prism Central VIP   | | |
    |  |                          ||  |  | Cluster VIP:        | | |
    |  |                          ||  |  |   172.16.1.99       | | |
    |  +--------------------------+|  |  |                     | | |
    |                              |  |  | OVH-IPFO-Uplink-   | | |
    |                              |  |  | Subnet:            | | |
    |                              |  |  |  51.81.192.212/30  | | |
    |                              |  |  |  GW: 51.81.192.214 | | |
    |                              |  |  +---------------------+ | |
    |   Dedicated Servers          |  |                         | |
    |   +--------++--------++-----+|  +-------------------------+ |
    |   |ns1027  ||ns1029  ||ns102||                               |
    |   |233     ||300     ||9318 ||    Load Balancer               |
    |   +--------++--------++-----+|   +------------------+        |
    |                              |   |loadbalancer-970f |        |
    |                              |   +------------------+        |
    ===============================================================
```

## Key Components

### vRack (Virtual Rack)

The vRack is OVH's Layer 2 private network technology. It acts as a virtual switch connecting all your services:

- **pn-2014805**: The primary vRack containing the Nutanix nodes (dedicated servers), load balancer, and public IP blocks
- **pn-2016218**: Secondary vRack (should have IP block added if used)

All services that need to communicate at Layer 2 must be in the **same vRack**.

### IPFO (IP Failover / Additional IP)

IPFO blocks are public IP ranges that can be routed to services within a vRack:

| IP Block | Service | Purpose |
|----------|---------|---------|
| `51.81.192.212/30` | Nutanix cluster `fc` | Public internet access for CVMs |
| `15.204.37.80/28` | VMware `pcc-147-135-35-91` | Public internet access for ESXi/VMs |

### Nutanix Network Layout

| Component | IP / Subnet |
|-----------|------------|
| AHV Node 1 | 172.16.0.1 |
| AHV Node 2 | 172.16.0.2 |
| AHV Node 3 | 172.16.0.3 |
| Cluster VIP | 172.16.1.99 |
| Data Services IP (iSCSI) | 172.16.3.254 |
| Internal Subnet | 172.16.0.0/22 |
| CVM Internal Subnet | 192.168.5.0/25 |
| IPFO Uplink Subnet | 51.81.192.212/30 |
| IPFO Gateway | 51.81.192.214 |
| DNS Server | 213.186.33.99 (OVH) |
| NTP Server | ntp0.ovh.net |

### Traffic Flow (Internet Access)

1. **Outbound**: CVM -> 172.16.x.x -> IPFO Uplink (51.81.192.212/30) -> vRack -> OVH Backbone -> Internet
2. **Inbound**: Internet -> OVH Backbone -> routes IPFO block into vRack -> Nutanix nodes -> CVM

The IPFO block **must be added to the vRack** for this routing to work.

## Troubleshooting

### Symptom: "Unable to resolve the host" when downloading images via URL in Prism

**Root Cause**: The IPFO block `51.81.192.212/30` is not added to vRack `pn-2014805`.

**Diagnosis**:
1. Check OVH Manager -> Network -> IP -> find `51.81.192.212/30`
2. If the "Service" column shows `-`, the IP block is unattached
3. Check OVH Manager -> Network -> vRack private network -> `pn-2014805`
4. If the IP block is in "Your eligible services" (left) instead of "Your vRack" (right), it's not connected

**Fix**:
1. Go to OVH Manager -> Network -> vRack private network -> `pn-2014805`
2. Select `51.81.192.212/30` from "Your eligible services"
3. Click "Add >" to move it into the vRack
4. Wait for the operation to complete
5. Test by uploading an image via URL in Prism Central

### Symptom: OVH Anti-DDoS blocks iSCSI connections

**Root Cause**: OVH's anti-DDoS layer blocks repeated TCP connections to port 3260 from external IPs.

**Workaround**: Run the migration tool from a VM inside the Nutanix cluster, so iSCSI traffic stays within the private network (172.16.x.x).

### Useful API Commands for Network Debugging

```bash
# Check DNS name servers on the cluster
curl -sk -u "$NUTANIX_USERNAME:$NUTANIX_PASSWORD" \
  "https://$NUTANIX_ENDPOINT:$NUTANIX_PORT/api/nutanix/v1/cluster/name_servers"

# Check NTP servers
curl -sk -u "$NUTANIX_USERNAME:$NUTANIX_PASSWORD" \
  "https://$NUTANIX_ENDPOINT:$NUTANIX_PORT/api/nutanix/v1/cluster/ntp_servers"

# Get cluster network info
curl -sk -u "$NUTANIX_USERNAME:$NUTANIX_PASSWORD" \
  "https://$NUTANIX_ENDPOINT:$NUTANIX_PORT/api/nutanix/v1/cluster"

# List registered clusters (PE + PC)
curl -sk -u "$NUTANIX_USERNAME:$NUTANIX_PASSWORD" \
  "https://$NUTANIX_ENDPOINT:$NUTANIX_PORT/api/nutanix/v3/clusters/list" \
  -X POST -H "Content-Type: application/json" -d '{"kind":"cluster","length":25}'
```

### OVH Manager Locations

| What to Check | Where in OVH Manager |
|---------------|---------------------|
| IP block assignment | Bare Metal Cloud -> Network -> IP |
| vRack contents | Bare Metal Cloud -> Network -> vRack private network -> pn-2014805 |
| Nutanix cluster info | Bare Metal Cloud -> Nutanix -> fc |
| Firewall rules | Bare Metal Cloud -> Network -> IP -> (select IP) -> Firewall |
| Network Security | Bare Metal Cloud -> Network -> Network Security Dashboard |

## VMware vs Nutanix: Networking Differences

| Aspect | VMware (pcc-147-135-35-91) | Nutanix (cluster-1919) |
|--------|---------------------------|----------------------|
| IP Routing | IPFO directly attached to Dedicated Cloud service | IPFO must be in same vRack as Nutanix nodes |
| Public IPs | 15.204.37.80/28 | 51.81.192.212/30 |
| Private Network | Managed by OVH Dedicated Cloud | 172.16.0.0/22 on vSwitch vs0 |
| Internet Access | Managed automatically by OVH | Requires IPFO in vRack + correct gateway config |
| Nodes | ESXi hosts (OVH managed) | Bare metal dedicated servers in vRack |

## Incident Log

### 2026-03-17: Internet connectivity lost on Nutanix cluster

- **Symptom**: Prism Central image download via URL fails with "Unable to resolve the host cloud-images.ubuntu.com"
- **Root Cause**: IPFO block `51.81.192.212/30` was not added to vRack `pn-2014805`
- **Impact**: CVMs cannot reach internet (no DNS resolution, no HTTP)
- **Resolution**: Added IPFO block `51.81.192.212/30` to vRack `pn-2014805` via OVH Manager
- **Verified**: Image download via URL succeeded immediately after vRack fix (294MB in ~25s)
- **Note**: OVH also discontinued VMware internet connectivity for this account around the same time
