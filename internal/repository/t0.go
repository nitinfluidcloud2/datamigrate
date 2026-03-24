package repository

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/nitinmore/datamigrate/internal/blockio"
	"github.com/rs/zerolog/log"
)

// T0Config holds configuration for the T0 full sync via the repository transport.
type T0Config struct {
	StagingDir string
	DiskKey    int32
	Capacity   int64
}

// RunT0 orchestrates the T0 flow: NFC stream -> save to temp VMDK file -> parse
// VMDK -> write raw file -> convert to qcow2. Returns the qcow2 path and size.
func RunT0(ctx context.Context, nfcStream io.Reader, cfg T0Config) (qcow2Path string, qcow2Size int64, err error) {
	t0Start := time.Now()

	// Step 0: Check qemu-img is available
	if err := CheckQemuImg(); err != nil {
		return "", 0, err
	}

	// Step 1: Create staging directory
	if err := os.MkdirAll(cfg.StagingDir, 0755); err != nil {
		return "", 0, fmt.Errorf("creating staging dir: %w", err)
	}

	vmdkPath := filepath.Join(cfg.StagingDir, fmt.Sprintf("disk-%d.vmdk", cfg.DiskKey))
	rawPath := filepath.Join(cfg.StagingDir, fmt.Sprintf("disk-%d.raw", cfg.DiskKey))
	qcow2Path = filepath.Join(cfg.StagingDir, fmt.Sprintf("disk-%d.qcow2", cfg.DiskKey))

	// Step 2: Save NFC stream to temp VMDK file with progress logging
	log.Info().Str("vmdk_path", vmdkPath).Msg("saving NFC VMDK stream to file")
	nfcStart := time.Now()
	written, err := saveStreamToFile(ctx, nfcStream, vmdkPath, cfg.Capacity)
	if err != nil {
		return "", 0, fmt.Errorf("saving NFC stream: %w", err)
	}
	nfcElapsed := time.Since(nfcStart).Truncate(time.Second)
	log.Info().
		Int64("bytes_written_mb", written/(1024*1024)).
		Str("elapsed", nfcElapsed.String()).
		Msg("VMDK stream saved")

	// Step 3: Convert VMDK -> raw using qemu-img (handles streamOptimized format correctly)
	log.Info().Str("vmdk", vmdkPath).Str("raw", rawPath).Msg("converting VMDK to raw via qemu-img")
	convertStart := time.Now()
	if err := ConvertVMDKToRaw(vmdkPath, rawPath); err != nil {
		return "", 0, fmt.Errorf("converting VMDK to raw: %w", err)
	}
	rawStat, err := os.Stat(rawPath)
	if err != nil {
		return "", 0, fmt.Errorf("stating raw file: %w", err)
	}
	vmdkToRawElapsed := time.Since(convertStart).Truncate(time.Second)
	log.Info().
		Int64("raw_size_mb", rawStat.Size()/(1024*1024)).
		Str("elapsed", vmdkToRawElapsed.String()).
		Msg("VMDK → raw conversion complete")

	// Step 4: Verify raw file
	if err := VerifyRawFile(rawPath); err != nil {
		return "", 0, fmt.Errorf("verifying raw file: %w", err)
	}

	// Step 5: Convert raw -> qcow2
	rawToQcow2Start := time.Now()
	if err := ConvertRawToQcow2(rawPath, qcow2Path); err != nil {
		return "", 0, fmt.Errorf("converting raw to qcow2: %w", err)
	}
	rawToQcow2Elapsed := time.Since(rawToQcow2Start).Truncate(time.Second)

	qcow2Stat, err := os.Stat(qcow2Path)
	if err != nil {
		return "", 0, fmt.Errorf("stating qcow2: %w", err)
	}
	qcow2Size = qcow2Stat.Size()

	// Step 6: Delete temp VMDK file (keep raw for future T1 patching)
	if err := os.Remove(vmdkPath); err != nil {
		log.Warn().Err(err).Str("vmdk", vmdkPath).Msg("failed to remove temp VMDK")
	}

	// Print summary
	totalElapsed := time.Since(t0Start).Truncate(time.Second)
	fmt.Println()
	fmt.Println("=========================================")
	fmt.Println("  T0 Repository Sync Summary")
	fmt.Println("=========================================")
	fmt.Printf("  NFC download:     %s (%d MB compressed)\n", nfcElapsed, written/(1024*1024))
	fmt.Printf("  VMDK → raw:       %s (%d MB)\n", vmdkToRawElapsed, rawStat.Size()/(1024*1024))
	fmt.Printf("  raw → qcow2:      %s (%d MB compressed)\n", rawToQcow2Elapsed, qcow2Size/(1024*1024))
	fmt.Printf("  Total:            %s\n", totalElapsed)
	fmt.Println("=========================================")
	fmt.Println()

	return qcow2Path, qcow2Size, nil
}

// ParseVMDKToRaw opens a saved VMDK file, parses grains using VMDKStreamReader,
// and writes decompressed data to a raw file using RawFileWriter.
func ParseVMDKToRaw(vmdkPath, rawPath string, capacity int64) (int64, error) {
	f, err := os.Open(vmdkPath)
	if err != nil {
		return 0, fmt.Errorf("opening VMDK: %w", err)
	}
	defer f.Close()

	reader, err := NewVMDKStreamReader(f)
	if err != nil {
		return 0, fmt.Errorf("creating VMDK reader: %w", err)
	}

	// Use capacity from VMDK header if available; fall back to caller-supplied value
	rawCapacity := reader.Capacity()
	if rawCapacity == 0 {
		rawCapacity = capacity
	}

	writer, err := NewRawFileWriter(rawPath, rawCapacity)
	if err != nil {
		return 0, fmt.Errorf("creating raw writer: %w", err)
	}
	defer writer.Close()

	ctx := context.Background()
	grainCount := 0
	lastLog := time.Now()

	for {
		grain, err := reader.ReadGrain()
		if err != nil {
			return 0, fmt.Errorf("reading grain %d: %w", grainCount, err)
		}
		if grain == nil {
			break // end of stream
		}

		if err := writer.WriteBlock(ctx, blockio.BlockData{
			Offset: grain.ByteOffset,
			Length: int64(len(grain.Data)),
			Data:   grain.Data,
		}); err != nil {
			return 0, fmt.Errorf("writing grain at offset %d: %w", grain.ByteOffset, err)
		}
		grainCount++

		if time.Since(lastLog) >= 10*time.Second {
			log.Info().
				Int("grains", grainCount).
				Int64("written_mb", writer.BytesWritten()/(1024*1024)).
				Msg("VMDK parse progress")
			lastLog = time.Now()
		}
	}

	if err := writer.Finalize(); err != nil {
		return 0, fmt.Errorf("finalizing raw file: %w", err)
	}

	log.Info().
		Int("total_grains", grainCount).
		Int64("bytes_written", writer.BytesWritten()).
		Msg("VMDK parse complete")

	return rawCapacity, nil
}

// saveStreamToFile copies an io.Reader to a file, logging progress every 10 seconds.
func saveStreamToFile(ctx context.Context, r io.Reader, path string, totalSize int64) (int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	buf := make([]byte, 256*1024) // 256KB buffer
	var total int64
	lastLog := time.Now()

	for {
		// Check context cancellation
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}

		n, err := r.Read(buf)
		if n > 0 {
			nn, werr := f.Write(buf[:n])
			total += int64(nn)
			if werr != nil {
				return total, werr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return total, err
		}

		if time.Since(lastLog) >= 10*time.Second {
			if totalSize > 0 {
				pct := float64(total) / float64(totalSize) * 100
				log.Info().
					Int64("bytes_mb", total/(1024*1024)).
					Int64("total_mb", totalSize/(1024*1024)).
					Float64("pct", pct).
					Msg("saving NFC stream progress")
				fmt.Printf("\r  NFC download: %.1f%% (%d / %d MB)", pct, total/(1024*1024), totalSize/(1024*1024))
			} else {
				log.Info().
					Int64("bytes_mb", total/(1024*1024)).
					Msg("saving NFC stream progress")
				fmt.Printf("\r  NFC download: %d MB", total/(1024*1024))
			}
			lastLog = time.Now()
		}
	}

	return total, nil
}
