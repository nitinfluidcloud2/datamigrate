package vmware

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/rs/zerolog/log"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/nitinmore/datamigrate/internal/blockio"
)

// RawDiskReader reads raw flat disk bytes from a VMware datastore via HTTPS.
// This downloads the *-flat.vmdk file directly from the datastore, which
// contains raw sector data — exactly what an iSCSI block device needs.
// No VMDK headers, no conversion, no qemu-img dependency.
type RawDiskReader struct {
	client    *Client
	vm        *object.VirtualMachine
	snapRef   *types.ManagedObjectReference
	disk      DiskInfo
	capacity  int64
	datastore *object.Datastore
	flatPath  string // path within datastore to the flat VMDK

	httpReader io.ReadCloser
	totalRead  int64 // atomic: bytes read
}

// DatastorePath parses "[datastore] path/to/file.vmdk" into datastore name and path.
func parseDatastorePath(dsPath string) (dsName, filePath string, err error) {
	// Format: "[datastore-name] relative/path/file.vmdk"
	if !strings.HasPrefix(dsPath, "[") {
		return "", "", fmt.Errorf("invalid datastore path: %q (expected [datastore] path)", dsPath)
	}
	idx := strings.Index(dsPath, "] ")
	if idx < 0 {
		return "", "", fmt.Errorf("invalid datastore path: %q (missing ] separator)", dsPath)
	}
	dsName = dsPath[1:idx]
	filePath = dsPath[idx+2:]
	return dsName, filePath, nil
}

// flatVMDKPath converts a VMDK descriptor path to its base flat backing file path.
// Snapshot delta files (e.g., "vm/disk-000002.vmdk") don't have their own flat file.
// The flat file belongs to the base disk: "vm/disk-flat.vmdk".
// This function strips any snapshot suffix (-000001, -000002, etc.) first.
//
// Examples:
//   "vm/disk.vmdk"        → "vm/disk-flat.vmdk"
//   "vm/disk-000001.vmdk" → "vm/disk-flat.vmdk"
//   "vm/disk-000003.vmdk" → "vm/disk-flat.vmdk"
func flatVMDKPath(vmdkPath string) string {
	// Strip snapshot suffix: "disk-000002.vmdk" → "disk.vmdk"
	base := strings.TrimSuffix(vmdkPath, ".vmdk")

	// Check for snapshot pattern: ends with -NNNNNN (6 digits)
	if len(base) > 7 && base[len(base)-7] == '-' {
		suffix := base[len(base)-6:]
		isSnapshot := true
		for _, c := range suffix {
			if c < '0' || c > '9' {
				isSnapshot = false
				break
			}
		}
		if isSnapshot {
			base = base[:len(base)-7] // strip "-000002"
		}
	}

	return base + "-flat.vmdk"
}

// OpenRawDiskReader creates a reader that downloads raw flat disk bytes
// directly from the vSphere datastore via HTTPS.
func (c *Client) OpenRawDiskReader(ctx context.Context, vm *object.VirtualMachine, snapRef *types.ManagedObjectReference, disk DiskInfo) (*RawDiskReader, error) {
	capacity := disk.CapacityKB * 1024

	// Parse the datastore path: "[ssd-002032] rhel6-test/rhel6-test.vmdk"
	dsName, vmdkRelPath, err := parseDatastorePath(disk.FileName)
	if err != nil {
		return nil, err
	}
	flatPath := flatVMDKPath(vmdkRelPath)

	log.Info().
		Str("vm", vm.Name()).
		Int32("disk_key", disk.Key).
		Str("datastore", dsName).
		Str("vmdk_path", vmdkRelPath).
		Str("flat_path", flatPath).
		Int64("capacity_bytes", capacity).
		Msg("opening raw datastore disk reader")

	// Find the datastore object
	finder := find.NewFinder(c.vimClient, true)

	// Find datacenter first
	dc, err := finder.DefaultDatacenter(ctx)
	if err != nil {
		return nil, fmt.Errorf("finding datacenter: %w", err)
	}
	finder.SetDatacenter(dc)

	ds, err := finder.Datastore(ctx, dsName)
	if err != nil {
		return nil, fmt.Errorf("finding datastore %s: %w", dsName, err)
	}

	// Download the flat VMDK via HTTPS
	opts := soap.Download{Method: "GET"}
	reader, contentLength, err := ds.Download(ctx, flatPath, &opts)
	if err != nil {
		return nil, fmt.Errorf("downloading flat VMDK %s: %w", flatPath, err)
	}

	log.Info().
		Int64("content_length", contentLength).
		Int64("capacity", capacity).
		Str("flat_path", flatPath).
		Msg("raw datastore download stream opened")

	return &RawDiskReader{
		client:     c,
		vm:         vm,
		snapRef:    snapRef,
		disk:       disk,
		capacity:   capacity,
		datastore:  ds,
		flatPath:   flatPath,
		httpReader: reader,
	}, nil
}

// ReadBlocks reads raw flat disk bytes from the datastore HTTPS stream.
// The flat VMDK contains raw sector data — no headers, no compression.
func (dr *RawDiskReader) ReadBlocks(ctx context.Context, extents []blockio.BlockExtent) (<-chan blockio.BlockData, <-chan error) {
	dataCh := make(chan blockio.BlockData, 16)
	errCh := make(chan error, 1)

	go func() {
		defer close(dataCh)
		defer close(errCh)

		var totalSent int64
		var chunkCount int64

		log.Info().
			Int64("capacity_mb", dr.capacity/(1024*1024)).
			Int64("chunk_size_mb", blockChunkSize/(1024*1024)).
			Msg("reading raw flat disk from datastore")

		for {
			if err := ctx.Err(); err != nil {
				errCh <- err
				return
			}

			data := make([]byte, blockChunkSize)
			n, err := io.ReadFull(dr.httpReader, data)

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

				if chunkCount%10 == 0 {
					var memStats runtime.MemStats
					runtime.ReadMemStats(&memStats)
					pct := float64(totalSent) / float64(dr.capacity) * 100
					log.Info().
						Int64("chunks_sent", chunkCount).
						Int64("bytes_read_mb", totalSent/(1024*1024)).
						Int64("capacity_mb", dr.capacity/(1024*1024)).
						Float64("progress_pct", pct).
						Uint64("heap_mb", memStats.Alloc/(1024*1024)).
						Msg("raw reader progress")
				}
			}

			if err != nil {
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					log.Info().
						Int64("total_read_mb", totalSent/(1024*1024)).
						Int64("total_chunks", chunkCount).
						Msg("raw disk read complete (EOF)")
					return
				}
				errCh <- fmt.Errorf("reading flat VMDK: %w", err)
				return
			}
		}
	}()

	return dataCh, errCh
}

// Close releases the HTTP reader.
func (dr *RawDiskReader) Close() error {
	if dr.httpReader != nil {
		dr.httpReader.Close()
		dr.httpReader = nil
	}
	return nil
}

// Capacity returns the disk capacity in bytes.
func (dr *RawDiskReader) Capacity() int64 {
	return dr.capacity
}

// StreamSize returns the disk capacity (flat VMDK = raw bytes = full capacity).
func (dr *RawDiskReader) StreamSize() int64 {
	return dr.capacity
}

// StreamReader returns the underlying HTTP reader for direct piping.
func (dr *RawDiskReader) StreamReader() io.Reader {
	return dr.httpReader
}

// ReadExtent reads a single extent from the stream.
func (dr *RawDiskReader) ReadExtent(ctx context.Context, extent blockio.BlockExtent) (blockio.BlockData, error) {
	data := make([]byte, extent.Length)
	if dr.httpReader != nil {
		n, err := io.ReadFull(dr.httpReader, data)
		if err != nil && err != io.ErrUnexpectedEOF {
			return blockio.BlockData{}, fmt.Errorf("reading raw extent: %w", err)
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

// Ensure RawDiskReader implements BlockReader.
var _ blockio.BlockReader = (*RawDiskReader)(nil)
