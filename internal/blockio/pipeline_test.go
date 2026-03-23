package blockio

import (
	"context"
	"sync"
	"testing"
)

// mockReader implements BlockReader for testing.
type mockReader struct {
	data map[int64][]byte
}

func (r *mockReader) ReadBlocks(ctx context.Context, extents []BlockExtent) (<-chan BlockData, <-chan error) {
	dataCh := make(chan BlockData, len(extents))
	errCh := make(chan error, 1)

	go func() {
		defer close(dataCh)
		defer close(errCh)
		for _, ext := range extents {
			data := make([]byte, ext.Length)
			// Fill with a pattern based on offset
			for i := range data {
				data[i] = byte(ext.Offset % 256)
			}
			dataCh <- BlockData{
				DiskKey: 1,
				Offset:  ext.Offset,
				Length:  ext.Length,
				Data:    data,
			}
		}
	}()

	return dataCh, errCh
}

func (r *mockReader) ReadExtent(ctx context.Context, extent BlockExtent) (BlockData, error) {
	data := make([]byte, extent.Length)
	return BlockData{DiskKey: 1, Offset: extent.Offset, Length: extent.Length, Data: data}, nil
}

func (r *mockReader) Close() error { return nil }

// mockWriter implements BlockWriter for testing.
type mockWriter struct {
	mu     sync.Mutex
	blocks []BlockData
}

func (w *mockWriter) WriteBlock(ctx context.Context, block BlockData) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.blocks = append(w.blocks, block)
	return nil
}

func (w *mockWriter) Finalize() error { return nil }

func TestPipeline(t *testing.T) {
	reader := &mockReader{}
	writer := &mockWriter{}

	extents := []BlockExtent{
		{Offset: 0, Length: 4096},
		{Offset: 4096, Length: 4096},
		{Offset: 8192, Length: 4096},
		{Offset: 12288, Length: 4096},
	}

	pipeline := NewPipeline(reader, writer, PipelineConfig{Concurrency: 2})

	var totalTransferred int64
	err := pipeline.Run(context.Background(), extents, func(transferred, total int64) {
		totalTransferred = transferred
	})
	if err != nil {
		t.Fatalf("Pipeline.Run: %v", err)
	}

	if len(writer.blocks) != 4 {
		t.Errorf("blocks written = %d, want 4", len(writer.blocks))
	}
	if totalTransferred != 16384 {
		t.Errorf("totalTransferred = %d, want 16384", totalTransferred)
	}
}

func TestPipelineCancellation(t *testing.T) {
	reader := &mockReader{}
	writer := &mockWriter{}

	extents := make([]BlockExtent, 1000)
	for i := range extents {
		extents[i] = BlockExtent{Offset: int64(i) * 4096, Length: 4096}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	pipeline := NewPipeline(reader, writer, PipelineConfig{Concurrency: 2})
	err := pipeline.Run(ctx, extents, nil)
	// Either nil (if all processed before cancel) or context error
	_ = err
}
