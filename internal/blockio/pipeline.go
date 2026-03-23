package blockio

import (
	"context"
	"fmt"
	"sync"

	"github.com/rs/zerolog/log"
)

// PipelineConfig configures the concurrent transfer pipeline.
type PipelineConfig struct {
	Concurrency    int // Number of concurrent writer workers
	ChannelBuffer  int // Number of blocks buffered between reader and writers
}

// DefaultPipelineConfig returns sensible defaults for parallel writers (iSCSI, qcow2).
// Memory usage: (ChannelBuffer + Concurrency) × block_size
// With CBT's 1 MB granularity: (16 + 4) × 1 MB = ~20 MB peak memory
func DefaultPipelineConfig() PipelineConfig {
	return PipelineConfig{
		Concurrency:   4,
		ChannelBuffer: 16,
	}
}

// StreamingPipelineConfig returns a config optimized for sequential stream upload.
// Uses a single worker since StreamWriter is inherently sequential (HTTP body),
// and avoids wasting goroutines on mutex contention.
// Large channel buffer (16 × 64 MB = 1 GB) keeps data ready so the network
// never starves waiting for reads.
func StreamingPipelineConfig() PipelineConfig {
	return PipelineConfig{
		Concurrency:   1,   // sequential — no mutex contention
		ChannelBuffer: 16,  // read-ahead: 16 × 64 MB chunks = ~1 GB in-flight
	}
}

// Pipeline orchestrates concurrent read→write block transfers.
type Pipeline struct {
	reader BlockReader
	writer BlockWriter
	config PipelineConfig
}

// NewPipeline creates a new transfer pipeline.
func NewPipeline(reader BlockReader, writer BlockWriter, config PipelineConfig) *Pipeline {
	if config.Concurrency <= 0 {
		config.Concurrency = 4
	}
	if config.ChannelBuffer <= 0 {
		config.ChannelBuffer = 16
	}
	return &Pipeline{
		reader: reader,
		writer: writer,
		config: config,
	}
}

// ProgressFunc is called with progress updates.
type ProgressFunc func(bytesTransferred int64, totalBytes int64)

// Run executes the pipeline: reads all extents and writes them to the target.
func (p *Pipeline) Run(ctx context.Context, extents []BlockExtent, progressFn ProgressFunc) error {
	totalBytes := int64(0)
	for _, ext := range extents {
		totalBytes += ext.Length
	}

	log.Info().
		Int("extents", len(extents)).
		Int64("total_bytes", totalBytes).
		Int64("total_mb", totalBytes/(1024*1024)).
		Int("concurrency", p.config.Concurrency).
		Int("channel_buffer", p.config.ChannelBuffer).
		Msg("starting transfer pipeline")

	dataCh, errCh := p.reader.ReadBlocks(ctx, extents)

	var (
		wg            sync.WaitGroup
		writeErr      error
		writeErrOnce  sync.Once
		transferred   int64
		transferredMu sync.Mutex
		blockCount    int64
	)

	// Start writer workers — note: for StreamWriter, concurrency must be 1
	// because HTTP streams are sequential. The pipeline still uses concurrency
	// for channel draining, but WriteBlock holds a mutex internally.
	for i := 0; i < p.config.Concurrency; i++ {
		wg.Add(1)
		workerID := i
		go func() {
			defer wg.Done()
			log.Debug().Int("worker", workerID).Msg("pipeline worker started")
			var workerBlocks int64
			for block := range dataCh {
				if err := p.writer.WriteBlock(ctx, block); err != nil {
					writeErrOnce.Do(func() {
						writeErr = fmt.Errorf("writing block at offset %d: %w", block.Offset, err)
						log.Error().Err(err).
							Int("worker", workerID).
							Int64("offset", block.Offset).
							Int64("length", block.Length).
							Msg("pipeline write error")
					})
					return
				}

				workerBlocks++
				if progressFn != nil {
					transferredMu.Lock()
					transferred += block.Length
					blockCount++
					current := transferred
					transferredMu.Unlock()
					progressFn(current, totalBytes)
				}
			}
			log.Debug().Int("worker", workerID).Int64("blocks", workerBlocks).Msg("pipeline worker done")
		}()
	}

	// Wait for all writers to complete
	wg.Wait()

	// Check for read errors
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("reading blocks: %w", err)
		}
	default:
	}

	if writeErr != nil {
		return writeErr
	}

	log.Info().
		Int64("bytes", transferred).
		Int64("mb", transferred/(1024*1024)).
		Int64("blocks", blockCount).
		Msg("transfer pipeline complete")
	return nil
}
