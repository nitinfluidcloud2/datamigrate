# Nutanix VM Internet — Root Cause & Justification for More IPs

## Current IPFO Block: `51.81.192.212/30`

| IP | Status | Used By |
|---|---|---|
| `51.81.192.212` | Network address | Unusable |
| `51.81.192.213` | **Only usable IP** | Claimed by 3 CVMs (API shows `num_assigned_ips: 3`, `num_free_ips: 0`) |
| `51.81.192.214` | OVH Gateway | Unusable (OVH-owned) |
| `51.81.192.215` | Broadcast | Unusable |

## What's Broken

- **VPC SNAT configured correctly** — `ovh-external-vpc` with SNAT IP `51.81.192.213`, NAT enabled, external subnet linked
- **But `external_connectivity_state: DISABLED`** in the API — Nutanix can't activate SNAT because the IP pool is exhausted (0 free)
- **VMs on overlay subnets get no internet** — confirmed by testing ping to `8.8.8.8` from RHEL7 VM on `OVH-External-Subnet`
- **OVH support investigated** — said it may be a Nutanix setting, couldn't resolve

## Evidence

### API Response (OVH-IPFO-Uplink-Subnet)
```json
{
  "subnet_ip": "51.81.192.212",
  "prefix_length": 30,
  "default_gateway_ip": "51.81.192.214",
  "pool_list": [{ "range": "51.81.192.213 51.81.192.213" }],
  "external_connectivity_state": "DISABLED",
  "is_external": true,
  "enable_nat": true,
  "ip_usage_stats": {
    "num_free_ips": 0,
    "num_assigned_ips": 3,
    "num_total_ips": 1
  }
}
```

### Tests Performed (2026-03-21)
1. VM on `OVH-External-Subnet` (overlay, inside `ovh-external-vpc`) → `ping 8.8.8.8` → no response
2. VM on `infra_pb6bh` (VLAN 1, vs0) → can reach `172.16.3.254:3260` (iSCSI) ✅ but no internet ❌
3. OVH anti-DDoS blocks repeated connections to ports 3260, 2049, 22 on public IP `147.135.96.30` from VMware side
4. Port 9440 (Prism HTTPS) is always stable and unblocked

## What We Need

- **Minimum: 1 additional IP** (`/32`) — dedicated SNAT IP for VPC, separate from CVM usage (~$2/month)
- **Recommended: /29 block** (5 usable IPs) — SNAT + floating IPs for VMs needing inbound access (~$10/month)
- Associate new IPs with vRack `pn-2014805`

## Why It Matters

We need a VM on Nutanix with **both**:
1. **iSCSI access** to `172.16.3.254:3260` — requires `infra_pb6bh` subnet (same L2 as data services IP)
2. **Internet/VMware access** — to reach vCenter API and download tools

Without internet on Nutanix VMs, we cannot test or run iSCSI-based VM migrations from VMware to Nutanix. The datamigrate tool needs connectivity to both VMware (source) and Nutanix iSCSI (target) simultaneously.

## Action Items

1. **Order additional IPFO IPs from OVH** — OVH Manager → Bare Metal Cloud → IP → Order Additional IPs
2. **Create new external subnet** in Prism with the new IP block
3. **Update VPC SNAT** to use a new dedicated IP (keep `.213` for cluster/CVMs)
4. **Test VM internet** — attach VM to overlay subnet in VPC, verify outbound connectivity
5. **Deploy datamigrate** on dual-NIC VM (infra + internet) and test iSCSI end-to-end
