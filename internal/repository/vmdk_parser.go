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
	// vmdkMagic is the magic number at the start of every sparse/streamOptimized VMDK.
	vmdkMagic uint32 = 0x564D444B // "VMDK"

	// sectorSize is the standard disk sector size.
	sectorSize = 512

	// defaultGrainSizeSectors is the typical grain size in sectors (128 * 512 = 64 KB).
	defaultGrainSizeSectors = 128

	// grainMarkerHeaderSize is the size of the grain marker header (LBA + compressed size).
	grainMarkerHeaderSize = 12 // uint64 + uint32
)

// SparseHeader is the 512-byte header at the start (and footer at the end)
// of a streamOptimized VMDK. All fields are little-endian.
type SparseHeader struct {
	MagicNumber        uint32
	Version            uint32
	Flags              uint32
	Capacity           uint64 // virtual disk size in sectors
	GrainSize          uint64 // grain size in sectors
	DescriptorOffset   uint64 // sector offset of embedded descriptor
	DescriptorSize     uint64 // size of descriptor in sectors
	NumGTEsPerGT       uint32 // grain table entries per grain table
	RgdOffset          uint64 // redundant grain directory offset
	GdOffset           uint64 // grain directory offset
	OverHead           uint64 // overhead in sectors (header + descriptor)
	UncleanShutdown    byte
	SingleEndLineChar  byte
	NonEndLineChar     byte
	DoubleEndLineChar1 byte
	DoubleEndLineChar2 byte
	CompressAlgorithm  uint16
	_                  [433]byte // pad to 512 bytes total
}

// GrainMarker represents a parsed grain from the VMDK stream.
type GrainMarker struct {
	LBA        uint64 // sector number for this grain's virtual offset
	ByteOffset int64  // LBA * sectorSize
	Data       []byte // decompressed grain data
	IsEOS      bool   // true if this is the end-of-stream marker
}

// ParseSparseHeader reads and validates the 512-byte sparse header from r.
func ParseSparseHeader(r io.Reader) (*SparseHeader, error) {
	var hdr SparseHeader
	if err := binary.Read(r, binary.LittleEndian, &hdr); err != nil {
		return nil, fmt.Errorf("vmdk: read sparse header: %w", err)
	}
	if hdr.MagicNumber != vmdkMagic {
		return nil, fmt.Errorf("vmdk: invalid magic 0x%08X, expected 0x%08X", hdr.MagicNumber, vmdkMagic)
	}
	if hdr.GrainSize == 0 {
		hdr.GrainSize = defaultGrainSizeSectors
	}
	log.Debug().
		Uint32("version", hdr.Version).
		Uint64("capacity_sectors", hdr.Capacity).
		Uint64("grain_size_sectors", hdr.GrainSize).
		Uint64("overhead_sectors", hdr.OverHead).
		Msg("vmdk: parsed sparse header")
	return &hdr, nil
}

// VMDKStreamReader wraps an io.Reader that provides a streamOptimized VMDK
// stream (e.g. from NFC ExportSnapshot) and emits decompressed data grains.
type VMDKStreamReader struct {
	r         io.Reader
	header    *SparseHeader
	grainBuf  []byte // reusable buffer for compressed grain data
	grainSize int64  // grain size in bytes
	capacity  int64  // virtual disk capacity in bytes
}

// NewVMDKStreamReader parses the sparse header and skips the descriptor/
// overhead region, leaving the reader positioned at the first grain marker.
func NewVMDKStreamReader(r io.Reader) (*VMDKStreamReader, error) {
	hdr, err := ParseSparseHeader(r)
	if err != nil {
		return nil, err
	}

	// Skip the remaining overhead bytes (descriptor region).
	// OverHead is in sectors; we already consumed one sector (the header).
	if hdr.OverHead > 1 {
		skip := int64(hdr.OverHead-1) * sectorSize
		if _, err := io.CopyN(io.Discard, r, skip); err != nil {
			return nil, fmt.Errorf("vmdk: skip descriptor (%d bytes): %w", skip, err)
		}
		log.Debug().Int64("skipped_bytes", skip).Msg("vmdk: skipped descriptor/overhead")
	}

	grainBytes := int64(hdr.GrainSize) * sectorSize

	return &VMDKStreamReader{
		r:         r,
		header:    hdr,
		grainBuf:  make([]byte, 0, grainBytes),
		grainSize: grainBytes,
		capacity:  int64(hdr.Capacity) * sectorSize,
	}, nil
}

// ReadGrain reads the next data grain from the stream, skipping any metadata
// markers (grain tables, grain directories). Returns nil, nil at end-of-stream.
func (v *VMDKStreamReader) ReadGrain() (*GrainMarker, error) {
	var markerHdr [grainMarkerHeaderSize]byte

	for {
		if _, err := io.ReadFull(v.r, markerHdr[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				// Treat unexpected EOF at marker boundary as end of stream.
				return nil, nil
			}
			return nil, fmt.Errorf("vmdk: read grain marker: %w", err)
		}

		lba := binary.LittleEndian.Uint64(markerHdr[0:8])
		compressedSize := binary.LittleEndian.Uint32(markerHdr[8:12])

		// EOS marker: LBA=0, size=0.
		if lba == 0 && compressedSize == 0 {
			log.Debug().Msg("vmdk: end-of-stream marker")
			return nil, nil
		}

		// Metadata marker: LBA=0, size>0 — skip the data.
		if lba == 0 {
			skip := int64(compressedSize)
			if _, err := io.CopyN(io.Discard, v.r, skip); err != nil {
				return nil, fmt.Errorf("vmdk: skip metadata (%d bytes): %w", skip, err)
			}
			log.Debug().Uint32("size", compressedSize).Msg("vmdk: skipped metadata marker")
			continue
		}

		// Data grain: read compressed data and decompress.
		if cap(v.grainBuf) < int(compressedSize) {
			v.grainBuf = make([]byte, compressedSize)
		}
		buf := v.grainBuf[:compressedSize]
		if _, err := io.ReadFull(v.r, buf); err != nil {
			return nil, fmt.Errorf("vmdk: read compressed grain (%d bytes): %w", compressedSize, err)
		}

		// Decompress zlib data.
		zr, err := zlib.NewReader(bytes.NewReader(buf))
		if err != nil {
			return nil, fmt.Errorf("vmdk: zlib init for LBA %d: %w", lba, err)
		}
		decompressed := make([]byte, v.grainSize)
		n, err := io.ReadFull(zr, decompressed)
		zr.Close()
		if err != nil && err != io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("vmdk: zlib decompress for LBA %d: %w", lba, err)
		}
		// Trim to actual decompressed size (last grain may be smaller).
		decompressed = decompressed[:n]

		// Skip any padding to align to the next sector boundary.
		// Total grain record = 12 + compressedSize; padding aligns to 512.
		totalRead := grainMarkerHeaderSize + int64(compressedSize)
		remainder := totalRead % sectorSize
		if remainder != 0 {
			pad := sectorSize - remainder
			if _, err := io.CopyN(io.Discard, v.r, pad); err != nil {
				// Padding may not exist at end of stream; ignore EOF here.
				if err != io.EOF && err != io.ErrUnexpectedEOF {
					return nil, fmt.Errorf("vmdk: skip padding (%d bytes): %w", pad, err)
				}
			}
		}

		return &GrainMarker{
			LBA:        lba,
			ByteOffset: int64(lba) * sectorSize,
			Data:       decompressed,
		}, nil
	}
}

// Capacity returns the virtual disk size in bytes.
func (v *VMDKStreamReader) Capacity() int64 {
	return v.capacity
}

// GrainSize returns the grain size in bytes.
func (v *VMDKStreamReader) GrainSize() int64 {
	return v.grainSize
}
