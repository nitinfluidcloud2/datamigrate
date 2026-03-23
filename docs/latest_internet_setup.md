# NSX-T Internet Access Setup

## Background
- OVH VMware environment lost internet when old IP block `51.81.235.216/28` disappeared from OVH account
- OVH assigned new IP block: `15.204.37.80/28` with VIP `15.204.37.81`
- Date resolved: 2026-03-20

## Environment
- **NSX-T Manager**: `nsxt.pcc-147-135-35-91.ovh.us`
- **T0 Gateway**: `ovh-T0-1171`
- **New Public IP Block**: `15.204.37.80/28` (usable: `.81` – `.94`, broadcast: `.95`)
- **VIP**: `15.204.37.81`
- **VM Network**: `vlan-network-11` (vrack VLAN), gateway `10.10.11.1`

## What OVH Configured (no action needed)
- T0 uplinks updated to new IP block
- HA VIP set to `15.204.37.81`
- Static route: `0.0.0.0/0` → next hop `15.204.37.94`

## IP Allocation
| IP | Purpose |
|---|---|
| `15.204.37.80` | Network address |
| `15.204.37.81` | VIP (HA, configured by OVH) |
| `15.204.37.82` | T0 Uplink 1 (configured by OVH) |
| `15.204.37.83` | T0 Uplink 2 (configured by OVH) |
| `15.204.37.84` | Windows VM SNAT/DNAT |
| `15.204.37.85` | ubuntu-vm SNAT/DNAT |
| `15.204.37.86` – `.94` | Available |
| `15.204.37.95` | Broadcast |

## NAT Rules on T0 Gateway (`ovh-T0-1171`)

### NAT Priority Convention
- **DNAT**: priority `0` (higher priority, evaluated first)
- **SNAT**: priority `2` (lower priority)

### Windows VM (`10.10.11.4`) — WORKING
| Rule Name | Action | Source | Destination | Translated IP | Port | Priority |
|---|---|---|---|---|---|---|
| NitinWindowsSNAT | SNAT | `10.10.11.4` | Any | `15.204.37.84` | Not Set | 2 |
| NitinWindowsDNAT | DNAT | Any | `15.204.37.84` | `10.10.11.4` | 3389 (RDP) | 0 |

- **TCP/IP on VM**: IP `10.10.11.4`, Subnet `255.255.255.0`, Gateway `10.10.11.1`, DNS `8.8.8.8` / `8.8.4.4`

### ubuntu-vm (`10.10.11.20`) — WORKING
| Rule Name | Action | Source | Destination | Translated IP | Port | Priority |
|---|---|---|---|---|---|---|
| 10_10_20_SNAT | SNAT | `10.10.11.20` | Any | `15.204.37.85` | Not Set | 2 |
| UbuntuSSH_DNAT | DNAT | Desktop IP (restricted) | `15.204.37.85` | `10.10.11.20` | 22 (SSH) | 0 |

- **Netplan** (`/etc/netplan/00-installer-config.yaml`):
  ```yaml
  network:
    renderer: networkd
    ethernets:
      ens32:
        addresses:
          - 10.10.11.20/24
        routes:
          - to: default
            via: 10.10.11.1
        nameservers:
          addresses:
            - 8.8.8.8
            - 8.8.4.4
    version: 2
  ```
- **SSH access**: `ssh ubuntuadmin@15.204.37.85` (source IP restricted in DNAT rule for security)
- **Security note**: SSH DNAT source IP restricted to desktop public IP to prevent brute-force/DDoS

### web-vm (`10.10.11.10`) — TBD
- NAT rules not yet configured
- Suggested: SNAT → `15.204.37.86`, DNAT for HTTP/HTTPS if needed

## Verification Steps
After configuring NAT for any VM:
1. From VM console: `ping 8.8.8.8`
2. From VM console: `curl ifconfig.me` (should show the SNAT translated IP)
3. For inbound access (RDP/SSH): test from external machine

## Steps to Add NAT for a New VM
1. **Networking → Tier-0 Gateways → Edit `ovh-T0-1171` → NAT**
2. Add **DNAT** rule (priority 0): External IP + port → Internal VM IP + port
3. Add **SNAT** rule (priority 2): Internal VM IP → External IP
4. Save and test

## Old Configuration (for reference)
- Old block: `51.81.235.216/28`
  - `.217` / `.218` = T0 uplinks
  - `.219` = SNAT/DNAT for VMs
