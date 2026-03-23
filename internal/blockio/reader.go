package blockio

import "context"

// BlockReader reads blocks from a source disk.
type BlockReader interface {
	// ReadBlocks reads the specified extents concurrently.
	ReadBlocks(ctx context.Context, extents []BlockExtent) (<-chan BlockData, <-chan error)
	// ReadExtent reads a single extent.
	ReadExtent(ctx context.Context, extent BlockExtent) (BlockData, error)
	// Close releases resources.
	Close() error
}
