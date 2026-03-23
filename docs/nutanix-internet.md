# Internet Access for VMs on OVH Nutanix AHV Cluster

## Environment

| Component | Value |
|---|---|
| Cluster | cluster-1919.nutanix.ovh.us |
| Hypervisor | Nutanix AHV |
| vRack | pn-2014805 |
| IPFO Block | 51.81.192.212/30 |
| IPFO Gateway | 51.81.192.214 |
| IPFO Usable IP | 51.81.192.213 (only 1 — likely used by cluster) |
| Internal Network | 172.16.0.0/22 on vSwitch vs0 |
| AHV Nodes | 172.16.0.1, .2, .3 |
| Cluster VIP | 172.16.1.99 |
| CVM Subnet | 192.168.5.0/25 |
| Data Services IP | 172.16.3.254 |
| OVH DNS | 213.186.33.99 |

---

## Current State

- **CVMs have internet** — can download images via URL in Prism Central
- **VMs on infra_pb6bh (VLAN 1) do NOT have internet** — no gateway, no NAT, no route to IPFO
- **OVH anti-DDoS** blocks repeated connections to non-standard ports (3260, 2049, 22) on public IP 147.135.96.30
- **Port 9440 (Prism HTTPS)** is always stable and not blocked
- OVH support investigated but said it may be a Nutanix setting

---

## Why VMs Have No Internet

The `infra_pb6bh` subnet (VLAN 1, vs0):
- Has no CIDR configured
- Has no gateway configured
- Has no connection to the IPFO uplink (VLAN 0)
- It's a flat L2 network with no L3 routing

VMs on this subnet can reach other devices on `172.16.0.0/22` (like the data services IP and Prism VIP) but have no path to the internet.

The IPFO uplink (`OVH-IPFO-Uplink-Subnet`, VLAN 0, vs0) has "External Connectivity: Yes" but the `/30` block only provides 1 usable IP (`.213`), which is likely consumed by cluster infrastructure.

---

## How CVMs Get Internet

CVMs use the OVH-IPFO-Uplink-Subnet (VLAN 0, vs0):
- CVM traffic routes through `51.81.192.212/30` to OVH gateway `51.81.192.214`
- They likely use `51.81.192.213` as source IP
- This path is built into the Nutanix cluster deployment by OVH

**To understand the exact CVM routing path**, SSH to a CVM and run:
```bash
ip route show
ip addr show
curl ifconfig.me
```

---

## Options to Fix VM Internet Access

### Option A: Nutanix Flow Networking (VPC + SNAT) — RECOMMENDED

Nutanix Flow Virtual Networking provides NAT/SNAT for VMs using a single public IP.

**Steps:**

1. **Check if Flow Networking is available:**
   - Prism Central → Settings → Flow Virtual Networking
   - Requires AOS 6.x+ and Prism Central 2022.x+
   - Must be included in your OVH Nutanix license (included in AHV Pro/Ultimate)

2. **Create an External Subnet (if OVH-IPFO-Uplink-Subnet doesn't work):**
   - Prism Central → Networking & Security → Subnets → Create Subnet
   - Type: External
   - VLAN ID: 0
   - Network IP/Prefix: 51.81.192.212/30
   - Gateway: 51.81.192.214
   - IP Pool: 51.81.192.213

3. **Create a VPC:**
   - Prism Central → Networking & Security → VPCs → Create VPC
   - Name: `vpc-migration`
   - External Connectivity: Select the external subnet
   - **Enable SNAT** — this is the key setting
   - VPC uses `51.81.192.213` for outbound NAT

4. **Create an Overlay Subnet inside the VPC:**
   - Subnets → Create Subnet
   - Type: Overlay
   - VPC: `vpc-migration`
   - Network IP/Prefix: `10.0.0.0/24`
   - Gateway: `10.0.0.1`
   - Enable DHCP or set IP pool

5. **Attach VM NIC to the Overlay Subnet:**
   - VM gets private IP (e.g., `10.0.0.x`)
   - Outbound traffic is SNATted through `51.81.192.213`

**Pros:** Clean, scalable, no extra IPs needed.
**Cons:** Requires Flow Networking license. `.213` may conflict with CVM usage.

---

### Option B: Request Additional IPFO Block from OVH — BEST LONG-TERM

The `/30` block is too small. Get a larger block.

1. **Order additional IPs:**
   - OVH Manager → Bare Metal Cloud → IP → Order Additional IPs
   - Minimum /29 (8 IPs, 5 usable) or /28 (16 IPs, 13 usable)
   - Associate with your Nutanix cluster / vRack `pn-2014805`

2. **Create new external subnet in Prism with the new block**

3. **Options with new IPs:**
   - Assign public IPs directly to VMs
   - Use Flow SNAT with a dedicated IP (keep `.213` for cluster)
   - Use Floating IPs for inbound access

**OVH Additional IP cost:** ~$2-5/month per IP

**Important:** OVH uses MAC-based routing for additional IPs. You must generate a virtual MAC in OVH Manager before assigning an additional IP to a VM.

---

### Option C: NAT Router VM — WORKAROUND (No Extra Cost)

Use one Linux VM as a NAT gateway for other VMs.

1. **Create a "router" VM with 2 NICs:**
   - NIC 1: `OVH-IPFO-Uplink-Subnet` (VLAN 0) → IP `51.81.192.213`
   - NIC 2: `infra_pb6bh` (VLAN 1) → IP `172.16.0.100/22`

2. **On the router VM (Linux), enable NAT:**
   ```bash
   # Enable IP forwarding
   echo 1 > /proc/sys/net/ipv4/ip_forward
   echo "net.ipv4.ip_forward = 1" >> /etc/sysctl.conf
   sysctl -p

   # Configure NAT (masquerade) — eth0 is the public NIC
   iptables -t nat -A POSTROUTING -o eth0 -j MASQUERADE
   iptables -A FORWARD -i eth1 -o eth0 -j ACCEPT
   iptables -A FORWARD -i eth0 -o eth1 -m state --state RELATED,ESTABLISHED -j ACCEPT

   # Save rules
   iptables-save > /etc/iptables.rules
   ```

3. **On other VMs (on infra_pb6bh):**
   ```bash
   ip route add default via 172.16.0.100
   echo "nameserver 213.186.33.99" > /etc/resolv.conf
   ```

**Risk:** Assigning `.213` to a VM may break CVM internet access. Verify CVM routing first.

---

### Option D: Direct VM on IPFO Subnet — SIMPLE BUT RISKY

Attach VM directly to `OVH-IPFO-Uplink-Subnet` with the public IP.

1. Connect VM NIC to OVH-IPFO-Uplink-Subnet (VLAN 0, vs0)
2. Configure VM networking:
   ```
   IP: 51.81.192.213
   Netmask: 255.255.255.252 (/30)
   Gateway: 51.81.192.214
   DNS: 213.186.33.99
   ```

**Problem:** Only 1 usable IP. If `.213` is already used by cluster, this WILL break things.

---

## Diagnostic Steps (Do First)

Before making changes, understand how things currently work:

### 1. SSH into a CVM
From RHEL7 VM on infra_pb6bh (172.16.0.50):
```bash
# Try SSH to CVM IPs (on 192.168.5.x or 172.16.x.x)
ssh nutanix@192.168.5.1
ssh nutanix@192.168.5.2
ssh nutanix@192.168.5.3
# Default CVM password: nutanix/4u (or check OVH setup email)
```

On the CVM:
```bash
# Check routing — how does CVM reach internet?
ip route show
ip addr show

# What public IP does CVM use?
curl -s ifconfig.me

# Trace the path
traceroute 8.8.8.8
```

### 2. SSH into AHV host
From RHEL7 VM:
```bash
ssh root@172.16.0.1
# Check OVS bridge configuration
ovs-vsctl show
ip route show
ip addr show
```

### 3. Check Prism Central settings
- Prism Central → **Settings → Flow Virtual Networking** — is it enabled?
- Prism Central → **Networking & Security → VPCs** — any existing VPCs?
- Prism Central → **Networking & Security → Subnets** → click OVH-IPFO-Uplink-Subnet → check details

### 4. Check what IP the cluster uses for internet
```bash
# From ubuntu-vm, check what IP Prism Central connects from
curl -sk -u "admin2:~4Kn6F1-w~fDFYFe" \
  "https://cluster-1919.nutanix.ovh.us:9440/api/nutanix/v3/clusters/list" \
  -X POST -H "Content-Type: application/json" \
  -d '{"kind":"cluster","length":25}' | python3 -m json.tool | grep -i "ip\|address\|external"
```

---

## Recommended Action Plan

### Priority 1: Diagnostics (No Risk)
1. From RHEL7 VM (`172.16.0.50`), try SSH to CVM IPs to understand routing
2. Check if Flow Networking is available in Prism Central
3. Determine if `.213` is used by CVMs

### Priority 2: Quick Fix
- **If Flow Networking is available:** Create VPC + SNAT (Option A)
- **If `.213` is free:** Try router VM approach (Option C)
- **If neither works:** Order additional IPs from OVH (Option B)

### Priority 3: Long-term
- Order /28 IPFO block from OVH (~13 usable IPs)
- Set up proper Flow Networking with VPCs
- Assign floating IPs to VMs that need inbound access

---

## API Investigation (2026-03-21)

### OVH-IPFO-Uplink-Subnet Status from API
```
subnet_ip: 51.81.192.212
prefix_length: 30
default_gateway_ip: 51.81.192.214
pool: 51.81.192.213 - 51.81.192.213 (single IP)
num_free_ips: 0
num_assigned_ips: 3 (3 CVMs sharing 1 IP!)
external_connectivity_state: DISABLED  ← KEY FINDING
is_external: true
enable_nat: true
```

### Key Findings
1. **Zero free IPs** — `.213` is consumed by 3 CVM assignments
2. **External connectivity is DISABLED** — despite `is_external: true` and `enable_nat: true`, the actual state is DISABLED. This likely explains why VPC SNAT never worked.
3. **3 assignments for 1 IP** — the 3 AHV nodes/CVMs all reference this single IP

### TODO for Monday
1. **Investigate `external_connectivity_state: DISABLED`** — try enabling it in Prism Central. This alone might fix SNAT.
2. **Order /29 IPFO block from OVH** (~$5/month, 5 usable IPs) — mandatory with current /30
3. After getting new IPs: create new external subnet → update VPC SNAT IP → test VM internet

---

## Key Warnings

- **Do NOT reassign `51.81.192.213`** without confirming how CVMs use it — you could lose cluster management access
- **OVH virtual MAC required**: When assigning additional IPs to VMs, generate a virtual MAC in OVH Manager first. OVH routes additional IPs based on MAC addresses.
- **VLAN 0 vs VLAN 1**: Different L2 domains. Traffic doesn't flow between them without a router.
- **Flow Networking licensing**: Verify your OVH package includes it (AHV Pro/Ultimate).

---

## OVH Documentation References

- OVH Nutanix Network Configuration guide
- OVH Nutanix Advanced Network Configuration (Flow Networking, VPC, NAT)
- OVH "Using IP addresses in a vRack" guide
- OVH Additional IP ordering and MAC generation
- Nutanix Flow Virtual Networking admin guide
- Nutanix Volumes (iSCSI) best practices — same L2 requirement
