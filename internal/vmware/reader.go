package vmware

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"sync/atomic"

	"github.com/rs/zerolog/log"
	"github.com/vmware/govmomi/nfc"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/nitinmore/datamigrate/internal/blockio"
)

// blockChunkSize is the maximum size of a single block read (64 MB).
const blockChunkSize int64 = 64 * 1024 * 1024

// DiskReader reads blocks from a VMware virtual disk via NFC lease.
type DiskReader struct {
	vm       *object.VirtualMachine
	client   *Client
	snapRef  *types.ManagedObjectReference
	disk     DiskInfo
	capacity int64

	// NFC lease state
	lease      *nfc.Lease
	leaseInfo  *nfc.LeaseInfo
	updater    *nfc.LeaseUpdater
	diskItem   *nfc.FileItem
	nfcReader  io.ReadCloser
	streamSize int64 // actual size of the NFC VMDK stream
	totalRead  int64 // atomic: bytes read so far
}

// OpenDiskReader creates a block reader for a specific disk on a VM snapshot.
// It exports the snapshot via NFC and locates the disk's download URL.
func (c *Client) OpenDiskReader(ctx context.Context, vm *object.VirtualMachine, snapRef *types.ManagedObjectReference, disk DiskInfo) (*DiskReader, error) {
	capacity := disk.CapacityKB * 1024

	log.Info().
		Str("vm", vm.Name()).
		Int32("disk_key", disk.Key).
		Str("file", disk.FileName).
		Int64("capacity_bytes", capacity).
		Msg("opening disk reader via NFC lease")

	// Export the snapshot to get an NFC lease
	lease, err := vm.ExportSnapshot(ctx, snapRef)
	if err != nil {
		return nil, fmt.Errorf("exporting snapshot: %w", err)
	}

	// Wait for the lease to be ready
	info, err := lease.Wait(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("waiting for NFC lease: %w", err)
	}

	log.Info().
		Int("items", len(info.Items)).
		Msg("NFC lease ready")

	// Find the FileItem matching our disk key.
	// NFC DeviceId format varies: could be "2000", "/vm-123/VirtualLsiLogicController0:0", etc.
	// Also match by size (disk capacity) to avoid picking up CD-ROM/ISO devices.
	var diskItem *nfc.FileItem
	diskKeyStr := fmt.Sprintf("%d", disk.Key)
	for i, item := range info.Items {
		log.Info().
			Str("device_id", item.DeviceId).
			Str("path", item.Path).
			Int64("size", item.Size).
			Int64("disk_capacity", capacity).
			Msg("NFC lease item")

		// Match by device key
		if item.DeviceId == diskKeyStr {
			diskItem = &info.Items[i]
			break
		}
	}

	// Fallback: match by size — pick the item closest to our disk capacity.
	// This filters out CD-ROM/ISO images which are much smaller.
	if diskItem == nil {
		var bestMatch *nfc.FileItem
		var bestDiff int64 = -1
		for i, item := range info.Items {
			// Skip items that are obviously not our disk (e.g., tiny ISOs)
			// The NFC stream for a disk is typically smaller than capacity
			// but should be at least a fraction of it for a used disk.
			// CD-ROMs are typically < 1 GB while disks are 10+ GB.
			diff := capacity - item.Size
			if diff < 0 {
				diff = -diff
			}
			if bestDiff < 0 || diff < bestDiff {
				bestDiff = diff
				bestMatch = &info.Items[i]
			}
		}
		if bestMatch != nil {
			diskItem = bestMatch
			log.Warn().
				Str("device_id", diskItem.DeviceId).
				Str("path", diskItem.Path).
				Int64("size", diskItem.Size).
				Int32("wanted_key", disk.Key).
				Msg("matched NFC item by size (key mismatch)")
		}
	}

	if diskItem == nil {
		_ = lease.Abort(ctx, nil)
		return nil, fmt.Errorf("disk key %d not found in NFC lease items", disk.Key)
	}

	log.Info().
		Str("device_id", diskItem.DeviceId).
		Int64("file_size", diskItem.Size).
		Int32("disk_key", disk.Key).
		Msg("found disk in NFC lease")

	// Start the lease updater (sends progress every 2s to keep the lease alive)
	updater := lease.StartUpdater(ctx, info)

	// Open HTTP stream to the disk data (streamOptimized VMDK format)
	opts := soap.Download{Method: "GET"}
	reader, contentLength, err := c.vimClient.Download(ctx, diskItem.URL, &opts)
	if err != nil {
		updater.Done()
		_ = lease.Abort(ctx, nil)
		return nil, fmt.Errorf("opening NFC download stream: %w", err)
	}

	// Use content length from HTTP response, fallback to lease file size
	streamSize := contentLength
	if streamSize <= 0 {
		streamSize = diskItem.Size
	}

	log.Info().
		Int64("stream_size", streamSize).
		Int64("stream_size_mb", streamSize/(1024*1024)).
		Int64("capacity", capacity).
		Int64("capacity_mb", capacity/(1024*1024)).
		Msg("NFC download stream opened (streamOptimized VMDK)")

	return &DiskReader{
		vm:         vm,
		client:     c,
		snapRef:    snapRef,
		disk:       disk,
		capacity:   capacity,
		lease:      lease,
		leaseInfo:  info,
		updater:    updater,
		diskItem:   diskItem,
		nfcReader:  reader,
		streamSize: streamSize,
	}, nil
}

// StreamSize returns the actual size of the NFC VMDK stream.
// This is typically much smaller than the disk capacity because the
// streamOptimized VMDK only contains used blocks (compressed).
func (dr *DiskReader) StreamSize() int64 {
	return dr.streamSize
}

// StreamReader returns the raw NFC download stream for direct piping.
// The stream contains a streamOptimized VMDK that Nutanix can accept directly.
// Caller should NOT call ReadBlocks if using StreamReader.
func (dr *DiskReader) StreamReader() io.Reader {
	return dr.nfcReader
}

// ReadBlocks reads disk data from the NFC lease stream and sends it through a channel.
// The NFC stream is read in chunks for progress tracking.
func (dr *DiskReader) ReadBlocks(ctx context.Context, extents []blockio.BlockExtent) (<-chan blockio.BlockData, <-chan error) {
	dataCh := make(chan blockio.BlockData, 16)
	errCh := make(chan error, 1)

	go func() {
		defer close(dataCh)
		defer close(errCh)

		var totalSent int64
		var chunkCount int64

		// For NFC streaming, ignore extents and read the entire stream sequentially
		// The NFC stream is a streamOptimized VMDK — we chunk it for pipeline progress
		streamTotal := dr.streamSize
		if streamTotal <= 0 {
			streamTotal = dr.capacity
		}

		log.Info().
			Int64("stream_size_mb", streamTotal/(1024*1024)).
			Int64("chunk_size_mb", blockChunkSize/(1024*1024)).
			Msg("reading NFC stream in chunks")

		for {
			if err := ctx.Err(); err != nil {
				errCh <- err
				return
			}

			data := make([]byte, blockChunkSize)
			n, err := io.ReadFull(dr.nfcReader, data)

			if n > 0 {
				block := blockio.BlockData{
					DiskKey: dr.disk.Key,
					Offset:  totalSent,
					Length:  int64(n),
					Data:    data[:n],
				}

				select {
				case dataCh <- block:
				case <-ctx.Done():
					errCh <- ctx.Err()
					return
				}

				totalSent += int64(n)
				chunkCount++
				atomic.StoreInt64(&dr.totalRead, totalSent)

				// Log progress periodically
				if chunkCount%10 == 0 {
					var memStats runtime.MemStats
					runtime.ReadMemStats(&memStats)
					pct := float64(totalSent) / float64(streamTotal) * 100
					log.Info().
						Int64("chunks_sent", chunkCount).
						Int64("bytes_read_mb", totalSent/(1024*1024)).
						Int64("stream_total_mb", streamTotal/(1024*1024)).
						Float64("progress_pct", pct).
						Uint64("heap_mb", memStats.Alloc/(1024*1024)).
						Msg("NFC reader progress")
				}
			}

			if err != nil {
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					log.Info().
						Int64("total_read_mb", totalSent/(1024*1024)).
						Int64("total_chunks", chunkCount).
						Msg("NFC stream complete (EOF)")
					return
				}
				errCh <- fmt.Errorf("reading NFC stream: %w", err)
				return
			}
		}
	}()

	return dataCh, errCh
}

// Close releases the NFC lease and reader resources.
func (dr *DiskReader) Close() error {
	if dr.nfcReader != nil {
		dr.nfcReader.Close()
		dr.nfcReader = nil
	}

	if dr.updater != nil {
		dr.updater.Done()
		dr.updater = nil
	}

	if dr.lease != nil {
		ctx := context.Background()
		if err := dr.lease.Complete(ctx); err != nil {
			log.Warn().Err(err).Msg("failed to complete NFC lease, aborting")
			_ = dr.lease.Abort(ctx, nil)
		}
		dr.lease = nil
	}

	return nil
}

// Capacity returns the disk capacity in bytes.
func (dr *DiskReader) Capacity() int64 {
	return dr.capacity
}

// Ensure DiskReader implements BlockReader.
var _ blockio.BlockReader = (*DiskReader)(nil)

// ReadExtent reads a single extent — for NFC, delegates to stream read.
func (dr *DiskReader) ReadExtent(ctx context.Context, extent blockio.BlockExtent) (blockio.BlockData, error) {
	data := make([]byte, extent.Length)

	if dr.nfcReader != nil {
		n, err := io.ReadFull(dr.nfcReader, data)
		if err != nil && err != io.ErrUnexpectedEOF {
			return blockio.BlockData{}, fmt.Errorf("reading extent from NFC: %w", err)
		}
		data = data[:n]
	}

	return blockio.BlockData{
		DiskKey: dr.disk.Key,
		Offset:  extent.Offset,
		Length:  int64(len(data)),
		Data:    data,
	}, nil
}
