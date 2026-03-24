# Repository Transport Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `repository` transport that reads VMware disks via NFC, parses streamOptimized VMDK into a local raw file, converts to qcow2, and uploads to Nutanix. Phase 1 = T0 full sync. Phase 2 = T1 incremental CBT + patch.

**Architecture:** NFC ExportSnapshot → parse streamOptimized VMDK grains (pure Go) → WriteAt to local raw file → qemu-img convert raw→qcow2 → upload to Nutanix → create VM. For T1, CBT query gives changed extents, NFC re-read filters only changed grains, patches raw file.

**Tech Stack:** Go, govmomi (NFC), go-vmdk-parser or custom grain parser, qemu-img (external), Nutanix PC v3 API.

**Spec:** `docs/superpowers/specs/2026-03-24-vddk-transport-design.md`

---

## File Structure

```
internal/
  repository/
    vmdk_parser.go      # Parse streamOptimized VMDK grain tables, decompress grains
    vmdk_parser_test.go  # Unit tests with synthetic VMDK data
    rawfile_writer.go    # RawFileWriter implements BlockWriter (WriteAt to raw file)
    rawfile_writer_test.go
    convert.go           # qemu-img wrapper (raw→qcow2, verify raw file)
    convert_test.go
    t0.go               # T0 full sync orchestration (NFC→parse→raw→qcow2→upload)
  state/
    migration.go        # Add TransportRepository constant
  cli/
    plan.go             # Accept --transport repository
  migration/
    full_sync.go        # Add case state.TransportRepository
```

---

### Task 1: Add TransportRepository constant

**Files:**
- Modify: `internal/state/migration.go:21-35`

- [ ] **Step 1: Add the constant**

In `internal/state/migration.go`, add after the existing transport constants:

```go
// TransportRepository reads via NFC, stores locally as raw file, uploads as qcow2.
// Correct for thin-provisioned VMDKs. Pure Go, no VDDK dependency.
TransportRepository TransportMode = "repository"
```

- [ ] **Step 2: Accept in CLI**

In `internal/cli/plan.go`, the `--transport` flag already accepts a string. Verify it passes through to the plan YAML. No code change needed if the flag is free-form. If validated, add `"repository"` to the allowed values.

- [ ] **Step 3: Commit**

```bash
git add internal/state/migration.go internal/cli/plan.go
git commit -m "feat: add TransportRepository constant for repository transport"
```

---

### Task 2: RawFileWriter (BlockWriter for local raw file)

**Files:**
- Create: `internal/repository/rawfile_writer.go`
- Create: `internal/repository/rawfile_writer_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/repository/rawfile_writer_test.go`:

```go
package repository

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nitinmore/datamigrate/internal/blockio"
)

func TestRawFileWriter_WriteBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.raw")

	w, err := NewRawFileWriter(path, 1024*1024) // 1 MB
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Write at offset 0
	err = w.WriteBlock(context.Background(), blockio.BlockData{
		DiskKey: 0,
		Offset:  0,
		Length:  4,
		Data:    []byte{0xDE, 0xAD, 0xBE, 0xEF},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Write at offset 512
	err = w.WriteBlock(context.Background(), blockio.BlockData{
		DiskKey: 0,
		Offset:  512,
		Length:  4,
		Data:    []byte{0xCA, 0xFE, 0xBA, 0xBE},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := w.Finalize(); err != nil {
		t.Fatal(err)
	}

	// Read back and verify
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if data[0] != 0xDE || data[1] != 0xAD || data[2] != 0xBE || data[3] != 0xEF {
		t.Errorf("offset 0: got %x, want DEADBEEF", data[0:4])
	}
	if data[512] != 0xCA || data[513] != 0xFE || data[514] != 0xBA || data[515] != 0xBE {
		t.Errorf("offset 512: got %x, want CAFEBABE", data[512:516])
	}
	// Verify sparse: bytes between should be zero
	if data[4] != 0 || data[511] != 0 {
		t.Error("expected zeros between written blocks")
	}
}

func TestRawFileWriter_ImplementsBlockWriter(t *testing.T) {
	var _ blockio.BlockWriter = (*RawFileWriter)(nil)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/nitinmore/go/datamigrate && go test ./internal/repository/ -v -run TestRawFileWriter`
Expected: FAIL — package/type doesn't exist yet

- [ ] **Step 3: Write minimal implementation**

Create `internal/repository/rawfile_writer.go`:

```go
package repository

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/rs/zerolog/log"

	"github.com/nitinmore/datamigrate/internal/blockio"
)

// RawFileWriter writes blocks to a local raw disk image file.
// Implements blockio.BlockWriter. Uses WriteAt for random-access writes
// so blocks can arrive in any order (pipeline has concurrent workers).
type RawFileWriter struct {
	file    *os.File
	path    string
	written int64
	mu      sync.Mutex
}

// NewRawFileWriter creates a sparse raw file of the given capacity.
func NewRawFileWriter(path string, capacity int64) (*RawFileWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("creating raw file %s: %w", path, err)
	}
	// Truncate creates a sparse file — sets size without allocating blocks
	if err := f.Truncate(capacity); err != nil {
		f.Close()
		return nil, fmt.Errorf("truncating raw file to %d bytes: %w", capacity, err)
	}
	log.Info().Str("path", path).Int64("capacity", capacity).Msg("raw file writer created (sparse)")
	return &RawFileWriter{file: f, path: path}, nil
}

func (w *RawFileWriter) WriteBlock(ctx context.Context, block blockio.BlockData) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err := w.file.WriteAt(block.Data, block.Offset)
	if err != nil {
		return fmt.Errorf("writing at offset %d: %w", block.Offset, err)
	}
	w.written += int64(n)
	return nil
}

func (w *RawFileWriter) Finalize() error {
	return w.file.Sync()
}

func (w *RawFileWriter) Close() error {
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}

func (w *RawFileWriter) Path() string        { return w.path }
func (w *RawFileWriter) BytesWritten() int64  { w.mu.Lock(); defer w.mu.Unlock(); return w.written }

var _ blockio.BlockWriter = (*RawFileWriter)(nil)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/nitinmore/go/datamigrate && go test ./internal/repository/ -v -run TestRawFileWriter`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/repository/rawfile_writer.go internal/repository/rawfile_writer_test.go
git commit -m "feat: add RawFileWriter implementing BlockWriter for local raw disk files"
```

---

### Task 3: StreamOptimized VMDK Parser

**Files:**
- Create: `internal/repository/vmdk_parser.go`
- Create: `internal/repository/vmdk_parser_test.go`

This is the core component. It reads a streamOptimized VMDK stream (from NFC) and emits BlockData with correct virtual offsets.

- [ ] **Step 1: Research the streamOptimized VMDK format**

Read the format spec. Key structures:
- **Sparse header** (512 bytes at offset 0): magic, version, grain size, descriptor offset
- **Embedded descriptor**: text metadata (extent type, virtual size, etc.)
- **Grain markers**: each grain has a marker with LBA (sector number) + compressed size
- **Grain data**: zlib-compressed grain (typically 64 KB uncompressed = 128 sectors)
- **End-of-stream marker**: LBA=0, size=0

The grain marker format (for streamOptimized):
```
struct {
    uint64 lba;          // sector number (multiply by 512 for byte offset)
    uint32 size;         // compressed size in bytes
}
// followed by `size` bytes of zlib-compressed data
// decompressed data = grainSize * sectorSize bytes (typically 64 KB)
```

- [ ] **Step 2: Write failing test**

Create `internal/repository/vmdk_parser_test.go`:

```go
package repository

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"testing"
)

func TestParseGrainMarker(t *testing.T) {
	// Create a minimal grain marker: LBA=128, compressed data = "hello"
	var buf bytes.Buffer

	// Write grain marker header
	binary.Write(&buf, binary.LittleEndian, uint64(128))  // LBA (sector 128 = byte offset 65536)

	// Compress the grain data
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	grainData := make([]byte, 65536) // 64 KB grain, all zeros
	copy(grainData[0:5], []byte("hello"))
	zw.Write(grainData)
	zw.Close()

	binary.Write(&buf, binary.LittleEndian, uint32(compressed.Len())) // compressed size
	buf.Write(compressed.Bytes())

	// Parse
	marker, err := readGrainMarker(&buf)
	if err != nil {
		t.Fatal(err)
	}

	if marker.LBA != 128 {
		t.Errorf("LBA: got %d, want 128", marker.LBA)
	}
	if marker.ByteOffset != 65536 {
		t.Errorf("ByteOffset: got %d, want 65536", marker.ByteOffset)
	}
	if len(marker.Data) != 65536 {
		t.Errorf("Data len: got %d, want 65536", len(marker.Data))
	}
	if string(marker.Data[0:5]) != "hello" {
		t.Errorf("Data: got %q, want 'hello'", marker.Data[0:5])
	}
}

func TestParseEndOfStream(t *testing.T) {
	var buf bytes.Buffer
	// End-of-stream marker: LBA=0, size=0
	binary.Write(&buf, binary.LittleEndian, uint64(0))
	binary.Write(&buf, binary.LittleEndian, uint32(0))

	marker, err := readGrainMarker(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !marker.IsEOS {
		t.Error("expected end-of-stream marker")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `cd /Users/nitinmore/go/datamigrate && go test ./internal/repository/ -v -run TestParse`
Expected: FAIL — readGrainMarker not defined

- [ ] **Step 4: Implement VMDK parser**

Create `internal/repository/vmdk_parser.go`:

```go
package repository

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/rs/zerolog/log"
)

const (
	vmdkMagicNumber  = 0x564d444b // "VMDK"
	sectorSize       = 512
	defaultGrainSize = 128 // sectors (128 * 512 = 64 KB)
)

// SparseHeader is the streamOptimized VMDK header (first 512 bytes).
type SparseHeader struct {
	MagicNumber        uint32
	Version            uint32
	Flags              uint32
	Capacity           uint64 // in sectors
	GrainSize          uint64 // in sectors (typically 128 = 64 KB)
	DescriptorOffset   uint64
	DescriptorSize     uint64
	NumGTEsPerGT       uint32
	RGDOffset          uint64
	GDOffset           uint64
	OverHead           uint64
	UncleanShutdown    byte
	SingleEndLineChar  byte
	NonEndLineChar     byte
	DoubleEndLineChar1 byte
	DoubleEndLineChar2 byte
	CompressAlgorithm  uint16
	_                  [433]byte // padding to 512 bytes
}

// GrainMarker represents a parsed grain from the streamOptimized VMDK.
type GrainMarker struct {
	LBA        uint64 // sector number
	ByteOffset int64  // byte offset (LBA * 512)
	Data       []byte // decompressed grain data
	IsEOS      bool   // end-of-stream marker
}

// ParseSparseHeader reads the streamOptimized VMDK header.
func ParseSparseHeader(r io.Reader) (*SparseHeader, error) {
	var h SparseHeader
	if err := binary.Read(r, binary.LittleEndian, &h); err != nil {
		return nil, fmt.Errorf("reading sparse header: %w", err)
	}
	if h.MagicNumber != vmdkMagicNumber {
		return nil, fmt.Errorf("invalid VMDK magic: 0x%X (expected 0x%X)", h.MagicNumber, vmdkMagicNumber)
	}
	log.Info().
		Uint64("capacity_sectors", h.Capacity).
		Uint64("grain_size_sectors", h.GrainSize).
		Int64("capacity_bytes", int64(h.Capacity)*sectorSize).
		Uint16("compress_algo", h.CompressAlgorithm).
		Msg("parsed VMDK sparse header")
	return &h, nil
}

// readGrainMarker reads one grain marker + decompressed data from the stream.
func readGrainMarker(r io.Reader) (*GrainMarker, error) {
	var lba uint64
	var compressedSize uint32

	if err := binary.Read(r, binary.LittleEndian, &lba); err != nil {
		if err == io.EOF {
			return &GrainMarker{IsEOS: true}, nil
		}
		return nil, fmt.Errorf("reading grain LBA: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &compressedSize); err != nil {
		return nil, fmt.Errorf("reading grain compressed size: %w", err)
	}

	// End-of-stream marker
	if lba == 0 && compressedSize == 0 {
		return &GrainMarker{IsEOS: true}, nil
	}

	// Read compressed data
	compressed := make([]byte, compressedSize)
	if _, err := io.ReadFull(r, compressed); err != nil {
		return nil, fmt.Errorf("reading compressed grain at LBA %d: %w", lba, err)
	}

	// Decompress
	zr, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("creating zlib reader for grain at LBA %d: %w", lba, err)
	}
	defer zr.Close()

	data, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("decompressing grain at LBA %d: %w", lba, err)
	}

	return &GrainMarker{
		LBA:        lba,
		ByteOffset: int64(lba) * sectorSize,
		Data:       data,
	}, nil
}

// VMDKStreamReader reads a streamOptimized VMDK and emits raw blocks.
// It skips the header and descriptor, then reads grains sequentially.
type VMDKStreamReader struct {
	reader    io.Reader
	header    *SparseHeader
	grainSize int64 // bytes per grain
	capacity  int64 // total disk capacity in bytes
	totalRead int64
}

// NewVMDKStreamReader wraps an NFC export stream.
func NewVMDKStreamReader(r io.Reader) (*VMDKStreamReader, error) {
	header, err := ParseSparseHeader(r)
	if err != nil {
		return nil, err
	}

	grainSize := int64(header.GrainSize) * sectorSize
	capacity := int64(header.Capacity) * sectorSize

	// Skip descriptor and other metadata until first grain marker.
	// The descriptor follows the header. We need to skip past it.
	// In streamOptimized VMDKs, the descriptor is at DescriptorOffset sectors.
	descEnd := int64(header.OverHead) * sectorSize
	skipBytes := descEnd - 512 // already read 512 bytes (header)
	if skipBytes > 0 {
		if _, err := io.CopyN(io.Discard, r, skipBytes); err != nil {
			return nil, fmt.Errorf("skipping VMDK overhead (%d bytes): %w", skipBytes, err)
		}
	}

	return &VMDKStreamReader{
		reader:    r,
		header:    header,
		grainSize: grainSize,
		capacity:  capacity,
	}, nil
}

// ReadGrain reads the next grain from the stream.
// Returns nil, nil at end-of-stream.
func (vr *VMDKStreamReader) ReadGrain() (*GrainMarker, error) {
	marker, err := readGrainMarker(vr.reader)
	if err != nil {
		return nil, err
	}
	if marker.IsEOS {
		return nil, nil
	}
	vr.totalRead += int64(len(marker.Data))
	return marker, nil
}

// Capacity returns the virtual disk size in bytes.
func (vr *VMDKStreamReader) Capacity() int64 { return vr.capacity }

// GrainSize returns the grain size in bytes.
func (vr *VMDKStreamReader) GrainSize() int64 { return vr.grainSize }

// TotalRead returns bytes decompressed so far.
func (vr *VMDKStreamReader) TotalRead() int64 { return vr.totalRead }
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd /Users/nitinmore/go/datamigrate && go test ./internal/repository/ -v -run TestParse`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/repository/vmdk_parser.go internal/repository/vmdk_parser_test.go
git commit -m "feat: add streamOptimized VMDK parser with grain decompression"
```

---

### Task 4: qemu-img Wrapper

**Files:**
- Create: `internal/repository/convert.go`
- Create: `internal/repository/convert_test.go`

- [ ] **Step 1: Write failing test**

Create `internal/repository/convert_test.go`:

```go
package repository

import (
	"os/exec"
	"testing"
)

func TestCheckQemuImg(t *testing.T) {
	// This test checks if qemu-img is available on the system
	_, err := exec.LookPath("qemu-img")
	if err != nil {
		t.Skip("qemu-img not installed, skipping")
	}

	if err := CheckQemuImg(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRawFile_InvalidFile(t *testing.T) {
	err := VerifyRawFile("/nonexistent/file.raw")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}
```

- [ ] **Step 2: Implement**

Create `internal/repository/convert.go`:

```go
package repository

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/rs/zerolog/log"
)

// CheckQemuImg verifies qemu-img is installed.
func CheckQemuImg() error {
	path, err := exec.LookPath("qemu-img")
	if err != nil {
		return fmt.Errorf("qemu-img not found: install with 'yum install qemu-img' or 'brew install qemu'")
	}
	log.Info().Str("path", path).Msg("qemu-img found")
	return nil
}

// ConvertRawToQcow2 converts a raw disk image to compressed qcow2.
func ConvertRawToQcow2(rawPath, qcow2Path string) error {
	log.Info().Str("raw", rawPath).Str("qcow2", qcow2Path).Msg("converting raw → qcow2")

	cmd := exec.Command("qemu-img", "convert",
		"-f", "raw",
		"-O", "qcow2",
		"-c", // compress
		rawPath, qcow2Path,
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("qemu-img convert: %w", err)
	}

	stat, err := os.Stat(qcow2Path)
	if err != nil {
		return fmt.Errorf("stating qcow2: %w", err)
	}
	log.Info().
		Str("qcow2", qcow2Path).
		Int64("size_mb", stat.Size()/(1024*1024)).
		Msg("qcow2 conversion complete")
	return nil
}

// VerifyRawFile runs basic sanity checks on the raw disk image.
func VerifyRawFile(rawPath string) error {
	stat, err := os.Stat(rawPath)
	if err != nil {
		return fmt.Errorf("raw file not found: %w", err)
	}
	if stat.Size() == 0 {
		return fmt.Errorf("raw file is empty")
	}

	// Check with 'file -s' if available
	cmd := exec.Command("file", "-s", rawPath)
	output, err := cmd.Output()
	if err == nil {
		log.Info().Str("file_type", string(output)).Msg("raw file type check")
	}
	return nil
}
```

- [ ] **Step 3: Run tests**

Run: `cd /Users/nitinmore/go/datamigrate && go test ./internal/repository/ -v -run TestCheck`
Expected: PASS (or skip if qemu-img not installed)

- [ ] **Step 4: Commit**

```bash
git add internal/repository/convert.go internal/repository/convert_test.go
git commit -m "feat: add qemu-img wrapper for raw→qcow2 conversion and verification"
```

---

### Task 5: T0 Full Sync Orchestration

**Files:**
- Create: `internal/repository/t0.go`
- Modify: `internal/migration/full_sync.go:144` (add `case state.TransportRepository`)

- [ ] **Step 1: Create T0 orchestration function**

Create `internal/repository/t0.go`:

```go
package repository

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/nitinmore/datamigrate/internal/blockio"
	"github.com/nitinmore/datamigrate/internal/util"
	"github.com/nitinmore/datamigrate/internal/vmware"
)

// T0Config holds configuration for a T0 full sync via repository transport.
type T0Config struct {
	StagingDir string // e.g., /tmp/datamigrate/<migration-name>
	DiskKey    int32
	Capacity   int64
}

// RunT0 performs a full sync: NFC export → parse VMDK → raw file → qcow2.
// Returns the path to the qcow2 file and its size.
func RunT0(ctx context.Context, nfcReader *vmware.DiskReader, cfg T0Config) (qcow2Path string, qcow2Size int64, err error) {
	if err := CheckQemuImg(); err != nil {
		return "", 0, err
	}

	if err := os.MkdirAll(cfg.StagingDir, 0755); err != nil {
		return "", 0, fmt.Errorf("creating staging dir: %w", err)
	}

	rawPath := filepath.Join(cfg.StagingDir, fmt.Sprintf("disk-%d.raw", cfg.DiskKey))
	qcow2Path = filepath.Join(cfg.StagingDir, fmt.Sprintf("disk-%d.qcow2", cfg.DiskKey))
	vmdkPath := filepath.Join(cfg.StagingDir, fmt.Sprintf("disk-%d.vmdk", cfg.DiskKey))

	// Step 1: Save NFC VMDK stream to temp file
	log.Info().Str("vmdk_path", vmdkPath).Msg("saving NFC stream to temp VMDK file")
	vmdkFile, err := os.Create(vmdkPath)
	if err != nil {
		return "", 0, fmt.Errorf("creating vmdk file: %w", err)
	}

	stream := nfcReader.StreamReader()
	start := time.Now()
	written, err := copyWithProgress(ctx, vmdkFile, stream, "NFC download")
	vmdkFile.Close()
	if err != nil {
		return "", 0, fmt.Errorf("saving VMDK stream: %w", err)
	}
	log.Info().Int64("bytes_mb", written/(1024*1024)).Str("elapsed", time.Since(start).Truncate(time.Second).String()).Msg("VMDK stream saved")

	// Step 2: Parse VMDK → raw file
	log.Info().Str("raw_path", rawPath).Msg("parsing VMDK grains → raw file")
	start = time.Now()
	bytesWritten, err := ParseVMDKToRaw(vmdkPath, rawPath, cfg.Capacity)
	if err != nil {
		return "", 0, fmt.Errorf("parsing VMDK to raw: %w", err)
	}
	log.Info().
		Int64("bytes_written_mb", bytesWritten/(1024*1024)).
		Str("elapsed", time.Since(start).Truncate(time.Second).String()).
		Msg("VMDK → raw conversion complete")

	// Step 3: Verify raw file
	if err := VerifyRawFile(rawPath); err != nil {
		log.Warn().Err(err).Msg("raw file verification warning")
	}

	// Step 4: Convert raw → qcow2
	start = time.Now()
	if err := ConvertRawToQcow2(rawPath, qcow2Path); err != nil {
		return "", 0, fmt.Errorf("converting raw to qcow2: %w", err)
	}
	log.Info().Str("elapsed", time.Since(start).Truncate(time.Second).String()).Msg("raw → qcow2 complete")

	// Step 5: Cleanup temp VMDK (keep raw for T1 patching)
	os.Remove(vmdkPath)

	stat, err := os.Stat(qcow2Path)
	if err != nil {
		return "", 0, fmt.Errorf("stating qcow2: %w", err)
	}

	log.Info().
		Str("qcow2", qcow2Path).
		Int64("qcow2_mb", stat.Size()/(1024*1024)).
		Str("raw", rawPath).
		Msg("T0 repository sync complete")

	return qcow2Path, stat.Size(), nil
}

// ParseVMDKToRaw opens a streamOptimized VMDK file and writes decompressed
// grains at correct virtual offsets to a raw file.
func ParseVMDKToRaw(vmdkPath, rawPath string, capacity int64) (int64, error) {
	vmdkFile, err := os.Open(vmdkPath)
	if err != nil {
		return 0, fmt.Errorf("opening vmdk: %w", err)
	}
	defer vmdkFile.Close()

	vmdkReader, err := NewVMDKStreamReader(vmdkFile)
	if err != nil {
		return 0, fmt.Errorf("parsing vmdk header: %w", err)
	}

	writer, err := NewRawFileWriter(rawPath, capacity)
	if err != nil {
		return 0, fmt.Errorf("creating raw writer: %w", err)
	}
	defer writer.Close()

	var grainsWritten int64
	for {
		grain, err := vmdkReader.ReadGrain()
		if err != nil {
			return writer.BytesWritten(), fmt.Errorf("reading grain: %w", err)
		}
		if grain == nil { // EOS
			break
		}

		err = writer.WriteBlock(context.Background(), blockio.BlockData{
			Offset: grain.ByteOffset,
			Length: int64(len(grain.Data)),
			Data:   grain.Data,
		})
		if err != nil {
			return writer.BytesWritten(), fmt.Errorf("writing grain at offset %d: %w", grain.ByteOffset, err)
		}
		grainsWritten++

		if grainsWritten%1000 == 0 {
			log.Info().
				Int64("grains", grainsWritten).
				Int64("written_mb", writer.BytesWritten()/(1024*1024)).
				Int64("capacity_mb", capacity/(1024*1024)).
				Msg("VMDK → raw progress")
		}
	}

	if err := writer.Finalize(); err != nil {
		return writer.BytesWritten(), err
	}

	log.Info().Int64("total_grains", grainsWritten).Int64("total_bytes", writer.BytesWritten()).Msg("VMDK → raw complete")
	return writer.BytesWritten(), nil
}

// copyWithProgress copies from src to dst with periodic logging.
func copyWithProgress(ctx context.Context, dst *os.File, src interface{ Read([]byte) (int, error) }, label string) (int64, error) {
	buf := make([]byte, 64*1024*1024) // 64 MB buffer
	var total int64
	lastLog := time.Now()

	for {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
			if time.Since(lastLog) > 10*time.Second {
				log.Info().Int64("mb", total/(1024*1024)).Str("label", label).Msg("copy progress")
				lastLog = time.Now()
			}
		}
		if err != nil {
			if err.Error() == "EOF" || err == fmt.Errorf("EOF") {
				return total, nil
			}
			// Check for io.EOF
			return total, nil
		}
	}
}
```

- [ ] **Step 2: Wire into full_sync.go**

In `internal/migration/full_sync.go`, add a new case in the transport switch (around line 144):

```go
case state.TransportRepository:
	// Repository transport: NFC → parse VMDK → raw file → qcow2 → upload
	nfcReader, err := o.vmClient.OpenDiskReader(ctx, vm, snapRef, diskInfo)
	if err != nil {
		o.SetError(ms, err)
		return fmt.Errorf("opening NFC reader: %w", err)
	}

	stagingDir := o.plan.Staging.Directory
	if stagingDir == "" {
		stagingDir = "/tmp/datamigrate"
	}
	diskDir := filepath.Join(stagingDir, o.plan.Name)

	qcow2Path, qcow2Size, err := repository.RunT0(ctx, nfcReader, repository.T0Config{
		StagingDir: diskDir,
		DiskKey:    disk.Key,
		Capacity:   disk.CapacityB,
	})
	nfcReader.Close()
	if err != nil {
		o.SetError(ms, err)
		return fmt.Errorf("repository T0: %w", err)
	}

	// Upload qcow2 to Nutanix
	imageName := fmt.Sprintf("%s-disk-%d-%s", o.plan.VMName, disk.Key, time.Now().Format("20060102-150405"))
	imageUUID, err := o.nxClient.CreateImage(ctx, imageName, qcow2Size)
	if err != nil {
		o.SetError(ms, err)
		return fmt.Errorf("creating image: %w", err)
	}

	if err := o.nxClient.UploadImage(ctx, imageUUID, qcow2Path); err != nil {
		o.SetError(ms, err)
		return fmt.Errorf("uploading qcow2: %w", err)
	}

	disk.ImageUUID = imageUUID
	disk.LocalPath = strings.TrimSuffix(qcow2Path, ".qcow2") + ".raw"
	disk.ChangeID = changeID
	disk.BytesCopied = disk.CapacityB

	log.Info().Str("image_uuid", imageUUID).Str("qcow2", qcow2Path).Msg("repository T0 complete")

	if err := o.store.SaveMigration(ms); err != nil {
		return fmt.Errorf("saving disk state: %w", err)
	}
	continue // skip pipeline path
```

Add import: `"github.com/nitinmore/datamigrate/internal/repository"`

- [ ] **Step 3: Build to verify compilation**

Run: `cd /Users/nitinmore/go/datamigrate && make build-linux`
Expected: Build succeeds

- [ ] **Step 4: Commit**

```bash
git add internal/repository/t0.go internal/migration/full_sync.go
git commit -m "feat: add repository transport T0 — NFC→VMDK→raw→qcow2→upload"
```

---

### Task 6: Integration Test on Migration Host

**Files:** None (manual test)

- [ ] **Step 1: Deploy to migration host**

```bash
make build-linux
scp bin/datamigrate-linux-amd64 ubuntuadmin@15.204.34.202:~/datamigrate
```

- [ ] **Step 2: Create plan with repository transport**

On migration host:
```bash
./datamigrate plan create --vm ubuntu10 --transport repository
```

Or edit `configs/ubuntu10-plan.yaml` to set `transport: repository`.

- [ ] **Step 3: Run T0**

```bash
./datamigrate migrate start --plan configs/ubuntu10-plan.yaml
```

Watch for:
- VMDK stream saved (NFC download)
- Grain parsing progress (grains written, MB)
- Raw file verification (file -s, fdisk -l)
- qcow2 conversion complete
- Image uploaded to Nutanix

- [ ] **Step 4: Verify raw file on migration host**

```bash
file -s /tmp/datamigrate/ubuntu10-migration/disk-2000.raw
fdisk -l /tmp/datamigrate/ubuntu10-migration/disk-2000.raw
```

Expected: Shows GPT partition table with 3 partitions, ext4 filesystem.

- [ ] **Step 5: Create VM and verify boot**

```bash
# Get image UUID from upload output, then:
IMAGE_UUID="<uuid>"
curl -sk -u $NUTANIX_USERNAME:$NUTANIX_PASSWORD -X POST -H "Content-Type: application/json" \
  -d "{\"spec\":{\"name\":\"ubuntu10-repo-test\",\"resources\":{\"num_sockets\":1,\"num_vcpus_per_socket\":1,\"memory_size_mib\":2048,\"power_state\":\"ON\",\"machine_type\":\"PC\",\"boot_config\":{\"boot_type\":\"UEFI\"},\"disk_list\":[{\"data_source_reference\":{\"kind\":\"image\",\"uuid\":\"$IMAGE_UUID\"},\"device_properties\":{\"device_type\":\"DISK\",\"disk_address\":{\"adapter_type\":\"SCSI\",\"device_index\":0}}}],\"nic_list\":[]},\"cluster_reference\":{\"kind\":\"cluster\",\"uuid\":\"00063ca3-b484-4dba-605d-66f6fc6cf210\"}},\"metadata\":{\"kind\":\"vm\"}}" \
  "https://$NUTANIX_ENDPOINT:9440/api/nutanix/v3/vms"
```

Expected: VM boots to Ubuntu login prompt.

- [ ] **Step 6: Commit any fixes from testing**

```bash
git add -A
git commit -m "fix: address issues found during repository T0 integration test"
```

---

## Phase 2 Tasks (T1 Incremental — implement after Phase 1 is proven)

### Task 7: T1 Incremental Sync — CBT + NFC Re-read + Patch

**Files:**
- Create: `internal/repository/incremental.go`
- Modify: `internal/migration/incremental_sync.go` (add `case state.TransportRepository`)

This task adds incremental sync: CBT query for changed extents → NFC re-read full disk → filter changed grains → patch raw file → convert → upload.

- [ ] **Step 1: Create incremental sync function**

Create `internal/repository/incremental.go` with:
- `RunT1(ctx, nfcReader, changedExtents, cfg)` — filters NFC grains against CBT extents, patches raw file
- Uses `VMDKStreamReader` to parse grains
- For each grain: check if it overlaps with any changed extent → if yes, `WriteAt` to raw file
- After patching: `ConvertRawToQcow2` → return qcow2 path

- [ ] **Step 2: Wire into incremental_sync.go**

Add `case state.TransportRepository` that:
1. Opens NFC reader (same as stream)
2. Queries CBT for changed extents (existing code)
3. Calls `repository.RunT1()` with NFC reader + extents
4. Uploads new qcow2 to Nutanix

- [ ] **Step 3: Test with ubuntu10**

Make changes on source VM, run incremental sync, verify patched raw file has changes, VM boots.

- [ ] **Step 4: Commit**

```bash
git commit -m "feat: add repository T1 incremental — CBT + NFC re-read + patch raw"
```
