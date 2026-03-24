package repository

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/nitinmore/datamigrate/internal/blockio"
)

// T1Config holds configuration for an incremental sync via repository transport.
type T1Config struct {
	StagingDir     string
	DiskKey        int32
	Capacity       int64
	RawPath        string              // path to existing repository raw file from T0
	ChangedExtents []blockio.BlockExtent // CBT-reported changed extents
}

// RunT1 performs an incremental sync:
// 1. NFC export → save VMDK to temp file
// 2. qemu-img convert VMDK → temp raw file
// 3. Read only changed extents from temp raw
// 4. Patch those extents into the repository raw file
// 5. Convert patched raw → qcow2
// Returns bytes patched.
func RunT1(ctx context.Context, nfcStream io.Reader, cfg T1Config) (int64, error) {
	if err := CheckQemuImg(); err != nil {
		return 0, err
	}

	if err := os.MkdirAll(cfg.StagingDir, 0755); err != nil {
		return 0, fmt.Errorf("creating staging dir: %w", err)
	}

	tempVMDK := filepath.Join(cfg.StagingDir, fmt.Sprintf("disk-%d-t1.vmdk", cfg.DiskKey))
	tempRaw := filepath.Join(cfg.StagingDir, fmt.Sprintf("disk-%d-t1.raw", cfg.DiskKey))
	qcow2Path := filepath.Join(cfg.StagingDir, fmt.Sprintf("disk-%d.qcow2", cfg.DiskKey))

	// Step 1: Save NFC stream to temp VMDK
	log.Info().Str("vmdk", tempVMDK).Msg("T1: saving NFC stream to temp VMDK")
	start := time.Now()
	written, err := saveStreamToFile(ctx, nfcStream, tempVMDK, cfg.Capacity)
	if err != nil {
		return 0, fmt.Errorf("saving NFC stream: %w", err)
	}
	log.Info().
		Int64("bytes_mb", written/(1024*1024)).
		Str("elapsed", time.Since(start).Truncate(time.Second).String()).
		Msg("T1: VMDK stream saved")

	// Step 2: Convert temp VMDK → temp raw
	log.Info().Msg("T1: converting temp VMDK → temp raw")
	start = time.Now()
	if err := ConvertVMDKToRaw(tempVMDK, tempRaw); err != nil {
		return 0, fmt.Errorf("converting temp VMDK to raw: %w", err)
	}
	log.Info().Str("elapsed", time.Since(start).Truncate(time.Second).String()).Msg("T1: temp raw ready")

	// Step 3: Read changed extents from temp raw, patch into repository raw
	log.Info().
		Int("extents", len(cfg.ChangedExtents)).
		Str("repo_raw", cfg.RawPath).
		Msg("T1: patching changed extents into repository")

	start = time.Now()
	patched, err := patchExtents(cfg.RawPath, tempRaw, cfg.ChangedExtents)
	if err != nil {
		return 0, fmt.Errorf("patching extents: %w", err)
	}
	log.Info().
		Int64("patched_mb", patched/(1024*1024)).
		Str("elapsed", time.Since(start).Truncate(time.Second).String()).
		Msg("T1: repository patched")

	// Step 4: Convert patched raw → qcow2
	log.Info().Msg("T1: converting patched raw → qcow2")
	start = time.Now()
	if err := ConvertRawToQcow2(cfg.RawPath, qcow2Path); err != nil {
		return 0, fmt.Errorf("converting raw to qcow2: %w", err)
	}
	log.Info().Str("elapsed", time.Since(start).Truncate(time.Second).String()).Msg("T1: qcow2 ready")

	// Step 5: Cleanup temp files
	os.Remove(tempVMDK)
	os.Remove(tempRaw)

	return patched, nil
}

// patchExtents reads changed extents from srcRaw and writes them to dstRaw at the same offsets.
func patchExtents(dstRaw, srcRaw string, extents []blockio.BlockExtent) (int64, error) {
	src, err := os.Open(srcRaw)
	if err != nil {
		return 0, fmt.Errorf("opening source raw: %w", err)
	}
	defer src.Close()

	dst, err := os.OpenFile(dstRaw, os.O_RDWR, 0644)
	if err != nil {
		return 0, fmt.Errorf("opening repository raw: %w", err)
	}
	defer dst.Close()

	var totalPatched int64
	buf := make([]byte, 1024*1024) // 1 MB buffer

	for i, ext := range extents {
		remaining := ext.Length
		offset := ext.Offset

		for remaining > 0 {
			readSize := int64(len(buf))
			if readSize > remaining {
				readSize = remaining
			}

			n, err := src.ReadAt(buf[:readSize], offset)
			if err != nil && err != io.EOF {
				return totalPatched, fmt.Errorf("reading extent %d at offset %d: %w", i, offset, err)
			}
			if n == 0 {
				break
			}

			if _, err := dst.WriteAt(buf[:n], offset); err != nil {
				return totalPatched, fmt.Errorf("writing extent %d at offset %d: %w", i, offset, err)
			}

			totalPatched += int64(n)
			offset += int64(n)
			remaining -= int64(n)
		}

		if (i+1)%100 == 0 {
			log.Info().
				Int("extent", i+1).
				Int("total_extents", len(extents)).
				Int64("patched_mb", totalPatched/(1024*1024)).
				Msg("T1: patch progress")
		}
	}

	if err := dst.Sync(); err != nil {
		return totalPatched, fmt.Errorf("syncing repository: %w", err)
	}

	return totalPatched, nil
}
