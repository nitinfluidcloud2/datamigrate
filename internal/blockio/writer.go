package blockio

import "context"

// BlockWriter writes blocks to a target.
type BlockWriter interface {
	// WriteBlock writes a single block of data.
	WriteBlock(ctx context.Context, block BlockData) error
	// Finalize completes the write operation (flush, close, convert).
	Finalize() error
}
