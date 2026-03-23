package blockio

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"runtime"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// StreamUploader is the interface for uploading a stream to a remote target.
type StreamUploader interface {
	// CreateImage creates an image entry and returns its UUID.
	CreateImage(ctx context.Context, name string, sizeBytes int64) (string, error)
	// UploadImageStream uploads raw disk data from a reader to an image.
	UploadImageStream(ctx context.Context, imageUUID string, reader io.Reader, size int64) error
	// UploadImageStreamGzip uploads gzip-compressed disk data from a reader.
	UploadImageStreamGzip(ctx context.Context, imageUUID string, reader io.Reader) error
}

// streamWriteBufSize is the size of the buffered writer wrapping the pipe.
// A 128 MB buffer decouples block-level writes from HTTP/TLS send cadence,
// allowing block production to run well ahead of network consumption.
// Larger buffer = fewer flushes = bigger TCP writes = better throughput.
const streamWriteBufSize = 128 * 1024 * 1024

// StreamWriter writes blocks directly into an HTTP upload stream.
// No local disk is needed — blocks are piped directly to the upload.
// Blocks MUST arrive in order since HTTP streams are sequential.
// When UseGzip is true, data is gzip-compressed before sending,
// dramatically reducing wire bytes for sparse/zero-heavy disks.
type StreamWriter struct {
	uploader  StreamUploader
	name      string
	capacity  int64
	imageUUID string
	UseGzip   bool // enable gzip compression on the upload stream

	pipeWriter *io.PipeWriter
	pipeReader *io.PipeReader
	bufWriter  *bufio.Writer  // 128 MB buffer sitting between block writes and compression
	gzipWriter *gzip.Writer   // nil when gzip disabled
	uploadErr  chan error

	mu          sync.Mutex
	written     int64 // actual block data written
	streamPos   int64 // current position in the stream (includes zero-fill)
	blockCount  int64 // number of blocks written
	startTime   time.Time
	lastLogTime time.Time

	zeroBuf []byte // reusable buffer for zero-fill (allocated once)
}

// NewStreamWriter creates a writer that streams blocks directly to Nutanix.
// Gzip is disabled by default — Nutanix Prism Central image API requires
// raw bytes with Content-Length. Set UseGzip=true only if your target supports it.
func NewStreamWriter(uploader StreamUploader, name string, capacity int64) *StreamWriter {
	return &StreamWriter{
		uploader: uploader,
		name:     name,
		capacity: capacity,
		UseGzip:  false,
		zeroBuf:  make([]byte, 1024*1024), // 1 MB zero buffer, allocated once
	}
}

// Start creates the image and begins the upload in the background.
func (w *StreamWriter) Start(ctx context.Context) error {
	uuid, err := w.uploader.CreateImage(ctx, w.name, w.capacity)
	if err != nil {
		return fmt.Errorf("creating image: %w", err)
	}
	w.imageUUID = uuid
	w.startTime = time.Now()
	w.lastLogTime = w.startTime

	w.pipeReader, w.pipeWriter = io.Pipe()

	// Build the write chain:
	//   block writes → bufWriter (128 MB) → [gzipWriter] → pipeWriter → HTTP body
	var downstream io.Writer = w.pipeWriter
	if w.UseGzip {
		// Best speed: prioritize throughput over compression ratio.
		// For sparse disks, even fast gzip gives 10-100x compression on zeros.
		gz, err := gzip.NewWriterLevel(w.pipeWriter, gzip.BestSpeed)
		if err != nil {
			return fmt.Errorf("creating gzip writer: %w", err)
		}
		w.gzipWriter = gz
		downstream = gz
	}
	w.bufWriter = bufio.NewWriterSize(downstream, streamWriteBufSize)
	w.uploadErr = make(chan error, 1)

	go func() {
		log.Info().
			Str("image_uuid", uuid).
			Int64("capacity_mb", w.capacity/(1024*1024)).
			Int("write_buf_mb", streamWriteBufSize/(1024*1024)).
			Bool("gzip", w.UseGzip).
			Msg("upload goroutine started")

		var uploadErr error
		if w.UseGzip {
			// Gzip: compressed stream, size unknown → chunked transfer encoding
			uploadErr = w.uploader.UploadImageStreamGzip(ctx, uuid, w.pipeReader)
		} else {
			// Raw: known size → Content-Length header
			uploadErr = w.uploader.UploadImageStream(ctx, uuid, w.pipeReader, w.capacity)
		}

		if uploadErr != nil {
			log.Error().Err(uploadErr).
				Str("image_uuid", uuid).
				Int64("bytes_streamed", w.streamPos).
				Bool("gzip", w.UseGzip).
				Msg("upload goroutine failed")
		} else {
			log.Info().
				Str("image_uuid", uuid).
				Bool("gzip", w.UseGzip).
				Msg("upload goroutine completed successfully")
		}
		w.uploadErr <- uploadErr
	}()

	log.Info().
		Str("image_uuid", uuid).
		Int64("capacity_mb", w.capacity/(1024*1024)).
		Int("write_buf_mb", streamWriteBufSize/(1024*1024)).
		Bool("gzip", w.UseGzip).
		Msg("streaming upload started")

	return nil
}

// WriteBlock writes a block into the upload stream.
func (w *StreamWriter) WriteBlock(ctx context.Context, block BlockData) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Fill gap with zeros if block starts after current position
	if block.Offset > w.streamPos {
		gap := block.Offset - w.streamPos
		log.Debug().
			Int64("gap_bytes", gap).
			Int64("from", w.streamPos).
			Int64("to", block.Offset).
			Msg("filling zero gap")
		if err := w.fillZeros(gap); err != nil {
			return fmt.Errorf("filling gap at offset %d: %w", w.streamPos, err)
		}
	}

	// Write the actual data into the buffered writer
	n, err := w.bufWriter.Write(block.Data)
	if err != nil {
		log.Error().Err(err).
			Int64("offset", block.Offset).
			Int64("length", block.Length).
			Int64("stream_pos", w.streamPos).
			Msg("buffered write failed")
		return fmt.Errorf("writing block at offset %d: %w", block.Offset, err)
	}
	if int64(n) != block.Length {
		return fmt.Errorf("short write: %d of %d", n, block.Length)
	}

	w.streamPos += block.Length
	w.written += block.Length
	w.blockCount++

	// Log progress every 30 seconds
	now := time.Now()
	if now.Sub(w.lastLogTime) >= 30*time.Second {
		elapsed := now.Sub(w.startTime).Seconds()
		rateMBs := float64(w.streamPos) / (1024 * 1024) / elapsed
		pct := float64(w.streamPos) / float64(w.capacity) * 100

		var memStats runtime.MemStats
		runtime.ReadMemStats(&memStats)

		log.Info().
			Str("image_uuid", w.imageUUID).
			Int64("blocks_written", w.blockCount).
			Int64("streamed_mb", w.streamPos/(1024*1024)).
			Int64("capacity_mb", w.capacity/(1024*1024)).
			Float64("progress_pct", pct).
			Float64("rate_mb_s", rateMBs).
			Int("buf_buffered_mb", w.bufWriter.Buffered()/(1024*1024)).
			Int("buf_available_mb", w.bufWriter.Available()/(1024*1024)).
			Uint64("heap_mb", memStats.Alloc/(1024*1024)).
			Uint64("sys_mb", memStats.Sys/(1024*1024)).
			Msg("stream writer progress")
		w.lastLogTime = now
	}

	return nil
}

// Finalize flushes the buffer, pads the stream to full capacity, and closes the pipe.
func (w *StreamWriter) Finalize() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	remaining := w.capacity - w.streamPos
	log.Info().
		Int64("remaining_mb", remaining/(1024*1024)).
		Int64("stream_pos_mb", w.streamPos/(1024*1024)).
		Bool("gzip", w.UseGzip).
		Msg("finalizing stream — padding with zeros")

	// Pad remaining space with zeros to reach full capacity
	if remaining > 0 {
		if err := w.fillZeros(remaining); err != nil {
			w.pipeWriter.CloseWithError(err)
			return fmt.Errorf("padding stream: %w", err)
		}
	}

	// Flush the bufio buffer into gzip (or pipe directly)
	if err := w.bufWriter.Flush(); err != nil {
		w.pipeWriter.CloseWithError(err)
		return fmt.Errorf("flushing buffer: %w", err)
	}

	// Close gzip stream to write the gzip footer (required!)
	if w.gzipWriter != nil {
		if err := w.gzipWriter.Close(); err != nil {
			w.pipeWriter.CloseWithError(err)
			return fmt.Errorf("closing gzip stream: %w", err)
		}
	}

	w.pipeWriter.Close()

	log.Info().Msg("pipe closed, waiting for upload to finish")

	// Wait for upload to finish
	if err := <-w.uploadErr; err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}

	elapsed := time.Since(w.startTime)
	avgRate := float64(w.streamPos) / (1024 * 1024) / elapsed.Seconds()
	log.Info().
		Int64("data_written_mb", w.written/(1024*1024)).
		Int64("total_streamed_mb", w.streamPos/(1024*1024)).
		Int64("blocks", w.blockCount).
		Float64("avg_rate_mb_s", avgRate).
		Bool("gzip", w.UseGzip).
		Str("elapsed", elapsed.Truncate(time.Second).String()).
		Str("image_uuid", w.imageUUID).
		Msg("streaming upload complete")

	return nil
}

// ImageUUID returns the UUID of the created image.
func (w *StreamWriter) ImageUUID() string {
	return w.imageUUID
}

// fillZeros writes n bytes of zeros to the buffered writer.
// Reuses w.zeroBuf to avoid allocations.
func (w *StreamWriter) fillZeros(n int64) error {
	for n > 0 {
		size := int64(len(w.zeroBuf))
		if size > n {
			size = n
		}
		written, err := w.bufWriter.Write(w.zeroBuf[:size])
		if err != nil {
			return err
		}
		w.streamPos += int64(written)
		n -= int64(written)
	}
	return nil
}

// Ensure StreamWriter implements BlockWriter.
var _ BlockWriter = (*StreamWriter)(nil)
