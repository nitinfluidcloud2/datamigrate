package repository

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"testing"
)

// buildSparseHeader creates a valid 512-byte streamOptimized VMDK header.
func buildSparseHeader(capacity, grainSize, overHead uint64) []byte {
	hdr := SparseHeader{
		MagicNumber: vmdkMagic,
		Version:     3,
		Capacity:    capacity,
		GrainSize:   grainSize,
		OverHead:    overHead,
	}
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, &hdr); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// compressData zlib-compresses data and returns the compressed bytes.
func compressData(data []byte) []byte {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

// writeGrainMarker writes a grain marker (LBA + compressedSize + compressedData)
// into buf.
func writeGrainMarker(buf *bytes.Buffer, lba uint64, compressed []byte) {
	binary.Write(buf, binary.LittleEndian, lba)
	binary.Write(buf, binary.LittleEndian, uint32(len(compressed)))
	buf.Write(compressed)
	// Add padding to align to sector boundary.
	total := grainMarkerHeaderSize + len(compressed)
	remainder := total % sectorSize
	if remainder != 0 {
		pad := sectorSize - remainder
		buf.Write(make([]byte, pad))
	}
}

// writeEOSMarker writes an end-of-stream marker (LBA=0, size=0).
func writeEOSMarker(buf *bytes.Buffer) {
	binary.Write(buf, binary.LittleEndian, uint64(0))
	binary.Write(buf, binary.LittleEndian, uint32(0))
}

// writeMetadataMarker writes a metadata marker (LBA=0, size>0, dummy data).
func writeMetadataMarker(buf *bytes.Buffer, dataSize uint32) {
	binary.Write(buf, binary.LittleEndian, uint64(0))
	binary.Write(buf, binary.LittleEndian, dataSize)
	buf.Write(make([]byte, dataSize))
}

func TestParseSparseHeader(t *testing.T) {
	hdrBytes := buildSparseHeader(2048, 128, 1)
	hdr, err := ParseSparseHeader(bytes.NewReader(hdrBytes))
	if err != nil {
		t.Fatalf("ParseSparseHeader: %v", err)
	}
	if hdr.MagicNumber != vmdkMagic {
		t.Errorf("magic = 0x%08X, want 0x%08X", hdr.MagicNumber, vmdkMagic)
	}
	if hdr.Capacity != 2048 {
		t.Errorf("capacity = %d, want 2048", hdr.Capacity)
	}
	if hdr.GrainSize != 128 {
		t.Errorf("grainSize = %d, want 128", hdr.GrainSize)
	}
}

func TestParseSparseHeaderInvalidMagic(t *testing.T) {
	hdrBytes := buildSparseHeader(2048, 128, 1)
	// Corrupt magic.
	hdrBytes[0] = 0xFF
	_, err := ParseSparseHeader(bytes.NewReader(hdrBytes))
	if err == nil {
		t.Fatal("expected error for invalid magic, got nil")
	}
}

func TestParseGrainMarker(t *testing.T) {
	// Build a minimal VMDK stream: header (1 sector overhead) + 1 data grain + EOS.
	grainSizeSectors := uint64(128)
	grainSizeBytes := int(grainSizeSectors * sectorSize) // 65536

	// Create test data: a grain filled with 0xAB.
	grainData := make([]byte, grainSizeBytes)
	for i := range grainData {
		grainData[i] = 0xAB
	}
	compressed := compressData(grainData)

	var stream bytes.Buffer
	// Header with OverHead=1 (just the header, no descriptor).
	stream.Write(buildSparseHeader(2048, grainSizeSectors, 1))

	// Data grain at LBA=256 (byte offset = 256*512 = 131072).
	writeGrainMarker(&stream, 256, compressed)

	// EOS.
	writeEOSMarker(&stream)

	reader, err := NewVMDKStreamReader(&stream)
	if err != nil {
		t.Fatalf("NewVMDKStreamReader: %v", err)
	}

	if reader.GrainSize() != int64(grainSizeBytes) {
		t.Errorf("GrainSize = %d, want %d", reader.GrainSize(), grainSizeBytes)
	}

	grain, err := reader.ReadGrain()
	if err != nil {
		t.Fatalf("ReadGrain: %v", err)
	}
	if grain == nil {
		t.Fatal("expected grain, got nil (EOS)")
	}
	if grain.LBA != 256 {
		t.Errorf("LBA = %d, want 256", grain.LBA)
	}
	if grain.ByteOffset != 256*sectorSize {
		t.Errorf("ByteOffset = %d, want %d", grain.ByteOffset, 256*sectorSize)
	}
	if len(grain.Data) != grainSizeBytes {
		t.Errorf("len(Data) = %d, want %d", len(grain.Data), grainSizeBytes)
	}
	for i, b := range grain.Data {
		if b != 0xAB {
			t.Errorf("Data[%d] = 0x%02X, want 0xAB", i, b)
			break
		}
	}

	// Next read should return nil (EOS).
	grain, err = reader.ReadGrain()
	if err != nil {
		t.Fatalf("ReadGrain after EOS: %v", err)
	}
	if grain != nil {
		t.Errorf("expected nil at EOS, got grain LBA=%d", grain.LBA)
	}
}

func TestParseEndOfStream(t *testing.T) {
	var stream bytes.Buffer
	stream.Write(buildSparseHeader(2048, 128, 1))
	writeEOSMarker(&stream)

	reader, err := NewVMDKStreamReader(&stream)
	if err != nil {
		t.Fatalf("NewVMDKStreamReader: %v", err)
	}

	grain, err := reader.ReadGrain()
	if err != nil {
		t.Fatalf("ReadGrain: %v", err)
	}
	if grain != nil {
		t.Errorf("expected nil at EOS, got grain LBA=%d", grain.LBA)
	}
}

func TestParseSkipMetadataMarker(t *testing.T) {
	grainSizeSectors := uint64(128)
	grainSizeBytes := int(grainSizeSectors * sectorSize)

	grainData := make([]byte, grainSizeBytes)
	for i := range grainData {
		grainData[i] = 0xCD
	}
	compressed := compressData(grainData)

	var stream bytes.Buffer
	stream.Write(buildSparseHeader(4096, grainSizeSectors, 1))

	// Metadata marker (LBA=0, size=1024) — should be skipped.
	writeMetadataMarker(&stream, 1024)

	// Data grain at LBA=128.
	writeGrainMarker(&stream, 128, compressed)

	// Another metadata marker — should be skipped.
	writeMetadataMarker(&stream, 512)

	// EOS.
	writeEOSMarker(&stream)

	reader, err := NewVMDKStreamReader(&stream)
	if err != nil {
		t.Fatalf("NewVMDKStreamReader: %v", err)
	}

	// First ReadGrain should skip metadata and return the data grain.
	grain, err := reader.ReadGrain()
	if err != nil {
		t.Fatalf("ReadGrain: %v", err)
	}
	if grain == nil {
		t.Fatal("expected data grain, got nil")
	}
	if grain.LBA != 128 {
		t.Errorf("LBA = %d, want 128", grain.LBA)
	}
	if grain.ByteOffset != 128*sectorSize {
		t.Errorf("ByteOffset = %d, want %d", grain.ByteOffset, 128*sectorSize)
	}
	for i, b := range grain.Data {
		if b != 0xCD {
			t.Errorf("Data[%d] = 0x%02X, want 0xCD", i, b)
			break
		}
	}

	// Next read should skip second metadata and return nil (EOS).
	grain, err = reader.ReadGrain()
	if err != nil {
		t.Fatalf("ReadGrain after metadata+EOS: %v", err)
	}
	if grain != nil {
		t.Errorf("expected nil at EOS, got grain LBA=%d", grain.LBA)
	}
}

func TestParseMultipleGrains(t *testing.T) {
	grainSizeSectors := uint64(128)
	grainSizeBytes := int(grainSizeSectors * sectorSize)

	var stream bytes.Buffer
	stream.Write(buildSparseHeader(8192, grainSizeSectors, 1))

	// Write 3 grains at different LBAs.
	lbas := []uint64{128, 256, 512}
	patterns := []byte{0x11, 0x22, 0x33}
	for i, lba := range lbas {
		data := make([]byte, grainSizeBytes)
		for j := range data {
			data[j] = patterns[i]
		}
		writeGrainMarker(&stream, lba, compressData(data))
	}
	writeEOSMarker(&stream)

	reader, err := NewVMDKStreamReader(&stream)
	if err != nil {
		t.Fatalf("NewVMDKStreamReader: %v", err)
	}

	for i, wantLBA := range lbas {
		grain, err := reader.ReadGrain()
		if err != nil {
			t.Fatalf("ReadGrain %d: %v", i, err)
		}
		if grain == nil {
			t.Fatalf("grain %d: expected data grain at LBA=%d, got nil", i, wantLBA)
		}
		if grain.LBA != wantLBA {
			t.Errorf("grain %d: LBA = %d, want %d", i, grain.LBA, wantLBA)
		}
		if grain.Data[0] != patterns[i] {
			t.Errorf("grain %d: Data[0] = 0x%02X, want 0x%02X", i, grain.Data[0], patterns[i])
		}
	}

	// EOS.
	grain, err := reader.ReadGrain()
	if err != nil {
		t.Fatalf("ReadGrain after all grains: %v", err)
	}
	if grain != nil {
		t.Errorf("expected nil at EOS, got grain LBA=%d", grain.LBA)
	}
}

func TestParseCapacityAndGrainSize(t *testing.T) {
	var stream bytes.Buffer
	stream.Write(buildSparseHeader(20480, 64, 1))
	writeEOSMarker(&stream)

	reader, err := NewVMDKStreamReader(&stream)
	if err != nil {
		t.Fatalf("NewVMDKStreamReader: %v", err)
	}

	wantCap := int64(20480 * sectorSize)
	if reader.Capacity() != wantCap {
		t.Errorf("Capacity = %d, want %d", reader.Capacity(), wantCap)
	}
	wantGrain := int64(64 * sectorSize)
	if reader.GrainSize() != wantGrain {
		t.Errorf("GrainSize = %d, want %d", reader.GrainSize(), wantGrain)
	}
}
