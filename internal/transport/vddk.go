package transport

// VDDK transport is a Phase 8 optimization.
// It requires CGo bindings to the VMware VDDK library for SAN/HotAdd transport modes.
// For now, we use the pure Go NBD transport via NFC lease.

// TODO(phase8): Implement VDDK transport for improved performance
// - CGo bindings to libvixDiskLib
// - SAN transport mode for direct LUN access
// - HotAdd transport mode for ESXi-local access
