package repository_test

import (
	"context"
	"os"
	"testing"

	"github.com/nitinmore/datamigrate/internal/blockio"
	"github.com/nitinmore/datamigrate/internal/repository"
)

// TestRawFileWriter_ImplementsBlockWriter is a compile-time check that
// RawFileWriter satisfies the blockio.BlockWriter interface.
var _ blockio.BlockWriter = (*repository.RawFileWriter)(nil)

func TestRawFileWriter_WriteBlock(t *testing.T) {
	// Create a temp file path (don't create the file — NewRawFileWriter creates it).
	tmpFile, err := os.CreateTemp(t.TempDir(), "rawfile_writer_test_*.img")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	path := tmpFile.Name()
	tmpFile.Close()
	os.Remove(path)

	const capacity = 4096

	w, err := repository.NewRawFileWriter(path, capacity)
	if err != nil {
		t.Fatalf("NewRawFileWriter: %v", err)
	}
	defer w.Close()

	ctx := context.Background()

	// Write "AAAA" at offset 0.
	block0 := blockio.BlockData{
		DiskKey: 0,
		Offset:  0,
		Length:  4,
		Data:    []byte("AAAA"),
	}
	if err := w.WriteBlock(ctx, block0); err != nil {
		t.Fatalf("WriteBlock at offset 0: %v", err)
	}

	// Write "BBBB" at offset 512.
	block512 := blockio.BlockData{
		DiskKey: 0,
		Offset:  512,
		Length:  4,
		Data:    []byte("BBBB"),
	}
	if err := w.WriteBlock(ctx, block512); err != nil {
		t.Fatalf("WriteBlock at offset 512: %v", err)
	}

	// Finalize flushes.
	if err := w.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	// Read the file back and verify contents.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if len(data) != capacity {
		t.Fatalf("expected file size %d, got %d", capacity, len(data))
	}

	// Verify "AAAA" at offset 0.
	if string(data[0:4]) != "AAAA" {
		t.Errorf("offset 0: expected AAAA, got %q", data[0:4])
	}

	// Verify zeros between offset 4 and 511 (sparse region).
	for i := 4; i < 512; i++ {
		if data[i] != 0 {
			t.Errorf("offset %d: expected 0x00, got 0x%02x", i, data[i])
			break
		}
	}

	// Verify "BBBB" at offset 512.
	if string(data[512:516]) != "BBBB" {
		t.Errorf("offset 512: expected BBBB, got %q", data[512:516])
	}

	// Verify BytesWritten reflects total bytes written across both blocks.
	if w.BytesWritten() != 8 {
		t.Errorf("BytesWritten: expected 8, got %d", w.BytesWritten())
	}

	// Verify Path() returns the correct path.
	if w.Path() != path {
		t.Errorf("Path: expected %q, got %q", path, w.Path())
	}
}
