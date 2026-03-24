package repository

import (
	"context"
	"os"
	"sync"

	"github.com/nitinmore/datamigrate/internal/blockio"
)

// RawFileWriter implements blockio.BlockWriter by writing blocks into a local
// raw disk image file. The file is created as a sparse file using Truncate so
// that unwritten regions consume no real disk space until touched.
//
// WriteBlock is safe for concurrent use from multiple goroutines (the pipeline
// uses up to 4 concurrent workers).
type RawFileWriter struct {
	mu           sync.Mutex
	file         *os.File
	path         string
	bytesWritten int64
}

// Compile-time check that RawFileWriter satisfies the BlockWriter interface.
var _ blockio.BlockWriter = (*RawFileWriter)(nil)

// NewRawFileWriter creates (or truncates) the file at path and sets its size
// to capacity bytes, creating a sparse file.  The caller is responsible for
// calling Close() when done.
func NewRawFileWriter(path string, capacity int64) (*RawFileWriter, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}

	// Establish sparse file at the target capacity without pre-allocating blocks.
	if err := f.Truncate(capacity); err != nil {
		f.Close()
		return nil, err
	}

	return &RawFileWriter{
		file: f,
		path: path,
	}, nil
}

// WriteBlock writes block.Data at block.Offset within the file.
// It is safe to call concurrently from multiple goroutines.
func (w *RawFileWriter) WriteBlock(_ context.Context, block blockio.BlockData) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err := w.file.WriteAt(block.Data, block.Offset)
	w.bytesWritten += int64(n)
	return err
}

// Finalize flushes all pending writes to the underlying storage.
func (w *RawFileWriter) Finalize() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.file.Sync()
}

// Close closes the underlying file.  Finalize should be called before Close
// to ensure all data is flushed.
func (w *RawFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.file.Close()
}

// Path returns the path of the raw image file.
func (w *RawFileWriter) Path() string {
	return w.path
}

// BytesWritten returns the total number of bytes successfully written across
// all WriteBlock calls.
func (w *RawFileWriter) BytesWritten() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.bytesWritten
}
