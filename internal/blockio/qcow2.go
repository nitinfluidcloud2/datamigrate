package blockio

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/rs/zerolog/log"
)

// Qcow2Writer writes blocks to a raw image file and converts to qcow2.
type Qcow2Writer struct {
	rawPath    string
	qcow2Path  string
	file       *os.File
	capacity   int64
	mu         sync.Mutex
	written    int64
}

// NewQcow2Writer creates a writer that stages blocks in a raw file.
func NewQcow2Writer(stagingDir string, diskKey int32, capacityBytes int64) (*Qcow2Writer, error) {
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		return nil, fmt.Errorf("creating staging dir: %w", err)
	}

	rawPath := filepath.Join(stagingDir, fmt.Sprintf("disk-%d.raw", diskKey))
	qcow2Path := filepath.Join(stagingDir, fmt.Sprintf("disk-%d.qcow2", diskKey))

	// Create or open the raw file with the full capacity
	f, err := os.OpenFile(rawPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("creating raw file: %w", err)
	}

	// Truncate to full capacity (creates sparse file)
	if err := f.Truncate(capacityBytes); err != nil {
		f.Close()
		return nil, fmt.Errorf("truncating raw file: %w", err)
	}

	log.Info().
		Str("raw_path", rawPath).
		Int64("capacity", capacityBytes).
		Msg("created raw disk image")

	return &Qcow2Writer{
		rawPath:   rawPath,
		qcow2Path: qcow2Path,
		file:      f,
		capacity:  capacityBytes,
	}, nil
}

// WriteBlock writes a block of data at the specified offset.
func (w *Qcow2Writer) WriteBlock(ctx context.Context, block BlockData) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if _, err := w.file.WriteAt(block.Data, block.Offset); err != nil {
		return fmt.Errorf("writing block at offset %d: %w", block.Offset, err)
	}

	w.written += block.Length
	return nil
}

// Finalize flushes the raw file and converts it to qcow2.
func (w *Qcow2Writer) Finalize() error {
	if err := w.file.Sync(); err != nil {
		return fmt.Errorf("syncing raw file: %w", err)
	}
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("closing raw file: %w", err)
	}

	log.Info().
		Str("raw", w.rawPath).
		Str("qcow2", w.qcow2Path).
		Msg("converting raw to qcow2")

	cmd := exec.Command("qemu-img", "convert", "-f", "raw", "-O", "qcow2", "-c", w.rawPath, w.qcow2Path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("converting to qcow2: %w: %s", err, string(output))
	}

	log.Info().Str("path", w.qcow2Path).Msg("qcow2 conversion complete")
	return nil
}

// RawPath returns the path to the raw disk image.
func (w *Qcow2Writer) RawPath() string {
	return w.rawPath
}

// Qcow2Path returns the path to the qcow2 disk image.
func (w *Qcow2Writer) Qcow2Path() string {
	return w.qcow2Path
}

// BytesWritten returns the total bytes written.
func (w *Qcow2Writer) BytesWritten() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.written
}

// Ensure Qcow2Writer implements BlockWriter.
var _ BlockWriter = (*Qcow2Writer)(nil)
