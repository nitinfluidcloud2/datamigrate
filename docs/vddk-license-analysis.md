# VDDK License Analysis (Post-Broadcom Acquisition)

**Date:** 2026-03-24
**Source:** Broadcom EULA at https://docs.broadcom.com/doc/end-user-agreement-english

## Summary

**VDDK cannot be used freely for commercial migration tools.** Broadcom has tightened control significantly since acquiring VMware in 2024.

## What is Allowed

| Use Case | Allowed? | Condition |
|---|---|---|
| Backup | Yes | Must be VMware TAP partner |
| Restore | Yes | Must be VMware TAP partner |
| Migration | Yes | Must be VMware TAP partner |
| Replication | Yes | Must be VMware TAP partner |

## What is NOT Allowed

| Use Case | Allowed? |
|---|---|
| Open-source migration tool | NO |
| Bundle VDDK in your binary | NO (unless explicit license approval) |
| Use without partner agreement | NO (legal risk) |
| General disk manipulation | NO |
| Competing hypervisor runtime usage | NO |
| Reverse engineering VMware formats | NO |

## Redistribution Restrictions

- Cannot bundle VDDK inside your product freely
- Cannot ship it like an open-source dependency
- Options:
  - Customer downloads VDDK separately (BYOVDDK pattern)
  - Distribute under explicit Broadcom license approval

## Post-Broadcom Changes (2024+)

| Before (VMware era) | After (Broadcom era) |
|---|---|
| Download VDDK freely from VMware site | Access is gated behind partner portal |
| Many tools used it semi-openly | Legal enforcement stricter |
| Developer-friendly ecosystem | Enterprise-only ecosystem push |

## How Big Vendors Use It

Veeam, Zerto, and similar tools:
1. Are official VMware Technology Alliance Partners (TAP)
2. Use VDDK APIs: NFC, HotAdd, SAN mode
3. Implement CBT-based incremental copy + continuous sync

## Impact on datamigrate

**VDDK is not viable for datamigrate** as a freely distributable migration tool:
- We are not a VMware TAP partner
- We cannot bundle VDDK libraries
- Even BYOVDDK pattern has legal gray areas for commercial use

**Decision:** Pure Go approach (NFC + VMDK parser) is the correct path:
- Zero Broadcom dependency
- No licensing risk
- Fully portable and distributable
- VDDK can be added as optional Phase 3 IF we become a TAP partner in the future
