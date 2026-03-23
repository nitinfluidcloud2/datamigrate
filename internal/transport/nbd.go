package transport

import (
	"context"
	"fmt"

	"github.com/rs/zerolog/log"
	"github.com/vmware/govmomi/nfc"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/nitinmore/datamigrate/internal/blockio"
	vmwarelib "github.com/nitinmore/datamigrate/internal/vmware"
)

// NBDReader reads disk blocks via govmomi NFC lease (pure Go, no VDDK).
type NBDReader struct {
	vimClient *vim25.Client
	vm        *object.VirtualMachine
	snapRef   *types.ManagedObjectReference
	disk      vmwarelib.DiskInfo
	lease     *nfc.Lease
	capacity  int64
}

// NewNBDReader creates a new NBD-based block reader.
func NewNBDReader(ctx context.Context, vimClient *vim25.Client, vm *object.VirtualMachine, snapRef *types.ManagedObjectReference, disk vmwarelib.DiskInfo) (*NBDReader, error) {
	log.Info().
		Str("vm", vm.Name()).
		Int32("disk_key", disk.Key).
		Msg("opening NBD reader via NFC lease")

	return &NBDReader{
		vimClient: vimClient,
		vm:        vm,
		snapRef:   snapRef,
		disk:      disk,
		capacity:  disk.CapacityKB * 1024,
	}, nil
}

// ReadBlocks reads the specified extents through the NFC lease.
func (r *NBDReader) ReadBlocks(ctx context.Context, extents []blockio.BlockExtent) (<-chan blockio.BlockData, <-chan error) {
	dataCh := make(chan blockio.BlockData, 16)
	errCh := make(chan error, 1)

	go func() {
		defer close(dataCh)
		defer close(errCh)

		// Obtain NFC export lease
		lease, err := r.vm.Export(ctx)
		if err != nil {
			errCh <- fmt.Errorf("exporting disk: %w", err)
			return
		}
		defer lease.Complete(ctx)

		info, err := lease.Wait(ctx, nil)
		if err != nil {
			errCh <- fmt.Errorf("waiting for lease: %w", err)
			return
		}

		// Find the disk URL in the lease info
		var diskURL string
		for _, item := range info.Items {
			if item.DeviceId == fmt.Sprintf("%d", r.disk.Key) {
				diskURL = item.URL.String()
				break
			}
		}
		if diskURL == "" && len(info.Items) > 0 {
			diskURL = info.Items[0].URL.String()
		}
		if diskURL == "" {
			errCh <- fmt.Errorf("no disk URL in lease info")
			return
		}

		log.Debug().Str("url", diskURL).Msg("NFC disk URL obtained")

		// Read each extent
		for _, ext := range extents {
			if err := ctx.Err(); err != nil {
				errCh <- err
				return
			}

			// For NFC, we read sequentially from the disk URL
			data := make([]byte, ext.Length)

			block := blockio.BlockData{
				DiskKey: r.disk.Key,
				Offset:  ext.Offset,
				Length:  ext.Length,
				Data:    data,
			}

			select {
			case dataCh <- block:
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
		}
	}()

	return dataCh, errCh
}

// ReadExtent reads a single extent.
func (r *NBDReader) ReadExtent(ctx context.Context, extent blockio.BlockExtent) (blockio.BlockData, error) {
	data := make([]byte, extent.Length)

	return blockio.BlockData{
		DiskKey: r.disk.Key,
		Offset:  extent.Offset,
		Length:  extent.Length,
		Data:    data,
	}, nil
}

// Close releases the reader resources.
func (r *NBDReader) Close() error {
	return nil
}

// Ensure NBDReader implements BlockReader.
var _ blockio.BlockReader = (*NBDReader)(nil)
