package blockio

import (
	"context"
	"fmt"
	"sync"

	"github.com/rs/zerolog/log"

	"github.com/nitinmore/datamigrate/internal/iscsi"
)

// ISCSIWriter writes blocks directly to a Nutanix Volume Group disk via iSCSI.
// Uses a pure Go iSCSI initiator — no kernel modules, no iscsiadm.
// Works on Mac, Linux, Docker — anywhere with TCP access to port 3260.
type ISCSIWriter struct {
	initiator *iscsi.Initiator
	mu        sync.Mutex
	written   int64
	connected bool
	cfg       ISCSIConfig
}

// ISCSIConfig holds iSCSI connection parameters.
type ISCSIConfig struct {
	TargetIQN      string
	PortalIP       string
	PortalPort     int
	MaxWriteBytes  int // max bytes per SCSI WRITE command (0 = use negotiated MaxRecvDataSegmentLength)
}

// NewISCSIWriter creates a writer that writes blocks directly to a Nutanix
// Volume Group disk over iSCSI using a pure Go initiator.
func NewISCSIWriter(cfg ISCSIConfig) (*ISCSIWriter, error) {
	ini := iscsi.NewInitiator(iscsi.Config{
		TargetIQN:     cfg.TargetIQN,
		PortalIP:      cfg.PortalIP,
		PortalPort:    cfg.PortalPort,
		MaxWriteBytes: cfg.MaxWriteBytes,
	})

	return &ISCSIWriter{
		initiator: ini,
		cfg:       cfg,
	}, nil
}

// Connect establishes the iSCSI session (pure Go, no iscsiadm).
func (w *ISCSIWriter) Connect(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.connected {
		return nil
	}

	if err := w.initiator.Connect(); err != nil {
		return fmt.Errorf("iSCSI connect: %w", err)
	}

	w.connected = true
	log.Info().
		Str("target", w.cfg.TargetIQN).
		Str("portal", fmt.Sprintf("%s:%d", w.cfg.PortalIP, w.cfg.PortalPort)).
		Msg("iSCSI connected (pure Go initiator)")

	return nil
}

// WriteBlock writes a block of data at the specified offset directly over iSCSI.
// Only the changed bytes cross the network.
func (w *ISCSIWriter) WriteBlock(ctx context.Context, block BlockData) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.connected {
		return fmt.Errorf("iSCSI not connected")
	}

	if err := w.initiator.WriteAt(block.Data, block.Offset); err != nil {
		return fmt.Errorf("iSCSI write at offset %d: %w", block.Offset, err)
	}

	w.written += block.Length
	return nil
}

// Finalize syncs and logs completion.
func (w *ISCSIWriter) Finalize() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	log.Info().
		Int64("bytes_written", w.written).
		Msg("iSCSI write finalized")

	return nil
}

// Disconnect closes the iSCSI session.
func (w *ISCSIWriter) Disconnect(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.connected {
		if err := w.initiator.Disconnect(); err != nil {
			log.Warn().Err(err).Msg("iSCSI disconnect failed")
		}
		w.connected = false
	}
	return nil
}

// BytesWritten returns the total bytes written over iSCSI.
func (w *ISCSIWriter) BytesWritten() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.written
}

// DevicePath returns a description of the iSCSI target (no /dev/sdX needed).
// Uses the actual portal address (which may differ from config after redirect).
func (w *ISCSIWriter) DevicePath() string {
	if w.initiator != nil {
		return fmt.Sprintf("iscsi://%s/%s", w.initiator.Portal(), w.cfg.TargetIQN)
	}
	return fmt.Sprintf("iscsi://%s:%d/%s", w.cfg.PortalIP, w.cfg.PortalPort, w.cfg.TargetIQN)
}

// Ensure ISCSIWriter implements BlockWriter.
var _ BlockWriter = (*ISCSIWriter)(nil)
