package vmware

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sync/atomic"

	"github.com/rs/zerolog/log"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/nitinmore/datamigrate/internal/blockio"
)

// RangeReader reads specific byte ranges from a VMware datastore flat VMDK
// using HTTP Range requests. Unlike RawDiskReader (which reads sequentially),
// this reader fetches data at exact offsets — critical for thin-provisioned disks
// where the flat file may be smaller than the virtual disk capacity.
type RangeReader struct {
	client    *Client
	vm        *object.VirtualMachine
	snapRef   *types.ManagedObjectReference
	disk      DiskInfo
	capacity  int64
	datastore *object.Datastore
	flatPath  string

	totalRead int64 // atomic
}

// OpenRangeReader creates a reader that fetches specific byte ranges from the
// flat VMDK via HTTP Range requests. Use with CBT extents for correct offsets.
func (c *Client) OpenRangeReader(ctx context.Context, vm *object.VirtualMachine, snapRef *types.ManagedObjectReference, disk DiskInfo) (*RangeReader, error) {
	capacity := disk.CapacityKB * 1024

	dsName, vmdkRelPath, err := parseDatastorePath(disk.FileName)
	if err != nil {
		return nil, err
	}
	flatPath := flatVMDKPath(vmdkRelPath)

	log.Info().
		Str("vm", vm.Name()).
		Int32("disk_key", disk.Key).
		Str("datastore", dsName).
		Str("flat_path", flatPath).
		Int64("capacity_bytes", capacity).
		Msg("opening range-based disk reader")

	finder := find.NewFinder(c.vimClient, true)
	dc, err := finder.DefaultDatacenter(ctx)
	if err != nil {
		return nil, fmt.Errorf("finding datacenter: %w", err)
	}
	finder.SetDatacenter(dc)

	ds, err := finder.Datastore(ctx, dsName)
	if err != nil {
		return nil, fmt.Errorf("finding datastore %s: %w", dsName, err)
	}

	return &RangeReader{
		client:    c,
		vm:        vm,
		snapRef:   snapRef,
		disk:      disk,
		capacity:  capacity,
		datastore: ds,
		flatPath:  flatPath,
	}, nil
}

// ReadBlocks reads data for each extent using HTTP Range requests.
// Each extent is read at its exact offset — data is written to the channel
// with correct offsets for the iSCSI writer.
func (rr *RangeReader) ReadBlocks(ctx context.Context, extents []blockio.BlockExtent) (<-chan blockio.BlockData, <-chan error) {
	dataCh := make(chan blockio.BlockData, 16)
	errCh := make(chan error, 1)

	go func() {
		defer close(dataCh)
		defer close(errCh)

		var totalRead int64
		var chunkCount int64

		for extIdx, ext := range extents {
			if err := ctx.Err(); err != nil {
				errCh <- err
				return
			}

			log.Info().
				Int("extent", extIdx+1).
				Int("total_extents", len(extents)).
				Int64("offset", ext.Offset).
				Int64("length_mb", ext.Length/(1024*1024)).
				Msg("reading extent via HTTP Range")

			// Open HTTP Range request for this extent
			reader, err := rr.downloadRange(ctx, ext.Offset, ext.Length)
			if err != nil {
				errCh <- fmt.Errorf("downloading extent at offset %d: %w", ext.Offset, err)
				return
			}

			// Read this extent in chunks
			extentRead := int64(0)
			for extentRead < ext.Length {
				remaining := ext.Length - extentRead
				chunkSize := int64(blockChunkSize)
				if remaining < chunkSize {
					chunkSize = remaining
				}

				data := make([]byte, chunkSize)
				n, err := io.ReadFull(reader, data)
				if n > 0 {
					block := blockio.BlockData{
						DiskKey: rr.disk.Key,
						Offset:  ext.Offset + extentRead,
						Length:  int64(n),
						Data:    data[:n],
					}

					select {
					case dataCh <- block:
					case <-ctx.Done():
						reader.Close()
						errCh <- ctx.Err()
						return
					}

					extentRead += int64(n)
					totalRead += int64(n)
					chunkCount++
					atomic.StoreInt64(&rr.totalRead, totalRead)

					if chunkCount%10 == 0 {
						var memStats runtime.MemStats
						runtime.ReadMemStats(&memStats)
						log.Info().
							Int64("chunks_sent", chunkCount).
							Int64("bytes_read_mb", totalRead/(1024*1024)).
							Int64("capacity_mb", rr.capacity/(1024*1024)).
							Uint64("heap_mb", memStats.Alloc/(1024*1024)).
							Msg("range reader progress")
					}
				}

				if err != nil {
					reader.Close()
					if err == io.EOF || err == io.ErrUnexpectedEOF {
						break // done with this extent
					}
					errCh <- fmt.Errorf("reading extent at offset %d: %w", ext.Offset, err)
					return
				}
			}
			reader.Close()
		}

		log.Info().
			Int64("total_read_mb", totalRead/(1024*1024)).
			Int64("total_chunks", chunkCount).
			Msg("range reader complete")
	}()

	return dataCh, errCh
}

// downloadRange opens an HTTP Range request for a specific byte range of the flat VMDK.
// We bypass govmomi's Download() because it only accepts 200 OK — Range requests
// return 206 Partial Content which govmomi treats as an error.
func (rr *RangeReader) downloadRange(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
	// Get authenticated URL (with service ticket cookie if needed)
	u, cookie, err := rr.datastore.ServiceTicket(ctx, rr.flatPath, "GET")
	if err != nil {
		return nil, fmt.Errorf("getting service ticket: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	if cookie != nil {
		req.AddCookie(cookie)
	}

	// Use the underlying http.Client (has TLS config, cookies, etc.)
	res, err := rr.client.vimClient.Client.Client.Do(req)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		res.Body.Close()
		return nil, fmt.Errorf("unexpected status %d for range request at offset %d", res.StatusCode, offset)
	}

	return res.Body, nil
}

// ReadExtent reads a single extent via HTTP Range request.
func (rr *RangeReader) ReadExtent(ctx context.Context, extent blockio.BlockExtent) (blockio.BlockData, error) {
	reader, err := rr.downloadRange(ctx, extent.Offset, extent.Length)
	if err != nil {
		return blockio.BlockData{}, fmt.Errorf("downloading range: %w", err)
	}
	defer reader.Close()

	data := make([]byte, extent.Length)
	n, err := io.ReadFull(reader, data)
	if err != nil && err != io.ErrUnexpectedEOF {
		return blockio.BlockData{}, fmt.Errorf("reading range: %w", err)
	}

	return blockio.BlockData{
		DiskKey: rr.disk.Key,
		Offset:  extent.Offset,
		Length:  int64(n),
		Data:    data[:n],
	}, nil
}

// Close is a no-op — each range request is opened/closed per extent.
func (rr *RangeReader) Close() error {
	return nil
}

// Capacity returns the disk capacity in bytes.
func (rr *RangeReader) Capacity() int64 {
	return rr.capacity
}

var _ blockio.BlockReader = (*RangeReader)(nil)
