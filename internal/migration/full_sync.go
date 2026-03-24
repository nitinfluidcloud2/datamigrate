package migration

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/nitinmore/datamigrate/internal/blockio"
	"github.com/nitinmore/datamigrate/internal/nutanix"
	"github.com/nitinmore/datamigrate/internal/repository"
	"github.com/nitinmore/datamigrate/internal/state"
	"github.com/nitinmore/datamigrate/internal/util"
	"github.com/nitinmore/datamigrate/internal/vmware"
)

// FullSync performs the initial full block copy (T0).
func (o *Orchestrator) FullSync(ctx context.Context) error {
	syncStart := time.Now()

	ms, err := o.GetState()
	if err != nil {
		return fmt.Errorf("getting migration state: %w", err)
	}

	if ms.Status != state.StatusCreated && ms.Status != state.StatusFailed {
		return fmt.Errorf("full sync requires CREATED or FAILED state, currently %s", ms.Status)
	}

	// If retrying after failure, clean up any previously created images
	if ms.Status == state.StatusFailed {
		log.Info().Msg("retrying full sync after previous failure")
		for i := range ms.Disks {
			if ms.Disks[i].ImageUUID != "" {
				log.Info().
					Str("image_uuid", ms.Disks[i].ImageUUID).
					Int32("disk_key", ms.Disks[i].Key).
					Msg("clearing stale image UUID from previous failed attempt")
				ms.Disks[i].ImageUUID = ""
			}
			ms.Disks[i].BytesCopied = 0
		}
	}

	if err := o.TransitionTo(ms, state.StatusFullSync); err != nil {
		return err
	}

	// Find the VM
	vm, _, err := o.vmClient.FindVM(ctx, "", o.plan.VMName)
	if err != nil {
		o.SetError(ms, err)
		return fmt.Errorf("finding VM: %w", err)
	}

	// Full CBT reset: disable → stun/unstun → enable → stun/unstun.
	// This ensures CBT is properly activated even if it was in a broken state.
	if err := o.vmClient.ResetCBT(ctx, vm); err != nil {
		log.Warn().Err(err).Msg("CBT reset failed, continuing with enable-only")
		if err := o.vmClient.EnableCBT(ctx, vm); err != nil {
			o.SetError(ms, err)
			return fmt.Errorf("enabling CBT: %w", err)
		}
	}

	// Create the real snapshot (now with CBT active)
	snapName := fmt.Sprintf("datamigrate-t0-%s", o.plan.Name)
	snapRef, err := o.vmClient.CreateSnapshot(ctx, vm, snapName, "Full sync snapshot")
	if err != nil {
		o.SetError(ms, err)
		return fmt.Errorf("creating snapshot: %w", err)
	}

	// Track snapshot in state for targeted cleanup
	ms.Snapshots = append(ms.Snapshots, state.SnapshotRef{
		Name:  snapName,
		MoRef: snapRef.Value,
	})
	if err := o.store.SaveMigration(ms); err != nil {
		return fmt.Errorf("saving snapshot ref: %w", err)
	}

	// For iSCSI transport: create Volume Group on Nutanix and whitelist our initiator
	if ms.Transport == state.TransportISCSI && ms.VolumeGroupID == "" {
		log.Info().Msg("setting up Nutanix Volume Group for iSCSI transport")
		if err := o.setupVolumeGroup(ctx, ms); err != nil {
			o.SetError(ms, err)
			return fmt.Errorf("setting up volume group: %w", err)
		}

		// Register our iSCSI initiator IQN so the target accepts our login
		initiatorIQN := "iqn.2026-01.com.datamigrate:initiator"
		if err := o.nxClient.AttachISCSIClient(ctx, ms.VolumeGroupID, initiatorIQN); err != nil {
			o.SetError(ms, err)
			return fmt.Errorf("attaching iSCSI client: %w", err)
		}
	}

	progress := NewProgress()

	// Process each disk
	for i := range ms.Disks {
		disk := &ms.Disks[i]
		log.Info().
			Int32("disk_key", disk.Key).
			Str("file", disk.FileName).
			Int64("capacity", disk.CapacityB).
			Str("transport", string(ms.Transport)).
			Msg("starting full sync for disk")

		// Progress: for iSCSI with CBT, total is updated below after extent calculation.
		// For stream/image, it's the disk capacity (adjusted after opening reader).
		if ms.Transport != state.TransportISCSI {
			progress.AddDisk(disk.Key, disk.CapacityB)
		}

		diskInfo := vmware.DiskInfo{
			Key:        disk.Key,
			FileName:   disk.FileName,
			CapacityKB: disk.CapacityB / 1024,
		}

		// Get the changeId from the snapshot for future incremental syncs
		// Save immediately so it survives if T0 crashes mid-copy
		changeID, err := o.vmClient.GetSnapshotDiskChangeID(ctx, snapRef, disk.Key)
		if err != nil {
			log.Warn().Err(err).Msg("could not get snapshot changeId, incremental sync may require full re-read")
		} else {
			disk.ChangeID = changeID
			if err := o.store.SaveMigration(ms); err != nil {
				return fmt.Errorf("saving changeId: %w", err)
			}
			log.Info().Str("change_id", changeID).Int32("disk_key", disk.Key).Msg("changeId saved to state")
		}

		// Build extents and open reader based on transport mode
		var extents []blockio.BlockExtent
		var reader blockio.BlockReader

		switch ms.Transport {
		case state.TransportISCSI:
			// iSCSI transport: try CBT to get allocated extents for thin-disk optimization.
			// If CBT query fails (known issue on some ESXi with changeId="*"),
			// fall back to full sequential read using RawDiskReader.
			log.Info().Int32("disk_key", disk.Key).Msg("querying CBT for allocated extents (iSCSI T0)")
			areas, _, cbtErr := o.vmClient.QueryChangedBlocks(ctx, vm, snapRef, diskInfo, "*")
			if cbtErr != nil {
				log.Warn().Err(cbtErr).Int32("disk_key", disk.Key).
					Msg("CBT query with changeId=* failed, falling back to full sequential read")
				extents = []blockio.BlockExtent{{Offset: 0, Length: disk.CapacityB}}
				progress.AddDisk(disk.Key, disk.CapacityB)

				// Use RawDiskReader — single sequential HTTP stream of the flat VMDK
				r, err := o.vmClient.OpenRawDiskReader(ctx, vm, snapRef, diskInfo)
				if err != nil {
					o.SetError(ms, err)
					return fmt.Errorf("opening raw disk reader: %w", err)
				}
				reader = r
			} else {
				for _, area := range areas {
					extents = append(extents, blockio.BlockExtent{
						Offset: area.Offset,
						Length: area.Length,
					})
				}
				var totalAllocated int64
				for _, ext := range extents {
					totalAllocated += ext.Length
				}
				log.Info().
					Int("extents", len(extents)).
					Int64("allocated_mb", totalAllocated/(1024*1024)).
					Int64("capacity_mb", disk.CapacityB/(1024*1024)).
					Msg("CBT extents for T0 (only allocated blocks will be transferred)")
				progress.AddDisk(disk.Key, totalAllocated)

				// Use RangeReader — per-extent HTTP Range requests at correct offsets
				r, err := o.vmClient.OpenRangeReader(ctx, vm, snapRef, diskInfo)
				if err != nil {
					o.SetError(ms, err)
					return fmt.Errorf("opening range reader: %w", err)
				}
				reader = r
			}
		default:
			// NFC export for stream/image transport (gives streamOptimized VMDK)
			extents = []blockio.BlockExtent{{Offset: 0, Length: disk.CapacityB}}
			r, err := o.vmClient.OpenDiskReader(ctx, vm, snapRef, diskInfo)
			if err != nil {
				o.SetError(ms, err)
				return fmt.Errorf("opening disk reader: %w", err)
			}
			reader = r
		}

		// Create writer based on transport mode
		var writer blockio.BlockWriter
		var iscsiWriter *blockio.ISCSIWriter
		switch ms.Transport {
		case state.TransportISCSI:
			// Direct iSCSI writes to Nutanix Volume Group — the Move-style approach.
			// Random-access WriteAt to block device, only actual data crosses the wire.
			w, err := o.createISCSIWriter(ctx, ms, disk)
			if err != nil {
				reader.Close()
				o.SetError(ms, err)
				return fmt.Errorf("creating iSCSI writer: %w", err)
			}
			iscsiWriter = w
			writer = w
			log.Info().
				Str("device", w.DevicePath()).
				Int32("disk_key", disk.Key).
				Msg("iSCSI writer connected to Volume Group")

		case state.TransportStream:
			// NFC gives streamOptimized VMDK. We save it locally, convert to
			// qcow2 with qemu-img, then upload. This is the only reliable way
			// to get a bootable disk on Nutanix.
			nfcReader := reader.(*vmware.DiskReader)

			stagingDir := o.plan.Staging.Directory
			if stagingDir == "" {
				stagingDir = "/tmp/datamigrate"
			}
			diskDir := filepath.Join(stagingDir, o.plan.Name)
			if err := os.MkdirAll(diskDir, 0755); err != nil {
				reader.Close()
				o.SetError(ms, err)
				return fmt.Errorf("creating staging dir: %w", err)
			}

			vmdkPath := filepath.Join(diskDir, fmt.Sprintf("disk-%d.vmdk", disk.Key))
			qcow2Path := filepath.Join(diskDir, fmt.Sprintf("disk-%d.qcow2", disk.Key))

			// Step 1: Save NFC VMDK stream to local file
			log.Info().
				Str("vmdk_path", vmdkPath).
				Int64("stream_size_mb", nfcReader.StreamSize()/(1024*1024)).
				Msg("saving NFC VMDK stream to local file")

			vmdkFile, err := os.Create(vmdkPath)
			if err != nil {
				reader.Close()
				o.SetError(ms, err)
				return fmt.Errorf("creating vmdk file: %w", err)
			}

			nfcStream := nfcReader.StreamReader()
			written, err := io.Copy(vmdkFile, nfcStream)
			vmdkFile.Close()
			reader.Close()

			if err != nil {
				o.SetError(ms, err)
				return fmt.Errorf("saving VMDK stream: %w", err)
			}

			log.Info().
				Int64("bytes_written_mb", written/(1024*1024)).
				Str("vmdk_path", vmdkPath).
				Msg("VMDK saved, converting to qcow2")

			// Step 2: Convert VMDK → qcow2 with qemu-img
			cmd := exec.CommandContext(ctx, "qemu-img", "convert", "-f", "vmdk", "-O", "qcow2", vmdkPath, qcow2Path)
			output, err := cmd.CombinedOutput()
			if err != nil {
				o.SetError(ms, err)
				return fmt.Errorf("converting VMDK to qcow2: %w: %s", err, string(output))
			}

			qcow2Stat, err := os.Stat(qcow2Path)
			if err != nil {
				o.SetError(ms, err)
				return fmt.Errorf("stating qcow2 file: %w", err)
			}

			log.Info().
				Int64("qcow2_size_mb", qcow2Stat.Size()/(1024*1024)).
				Str("qcow2_path", qcow2Path).
				Msg("qcow2 conversion complete, uploading to Nutanix")

			// Step 3: Upload qcow2 to Nutanix
			imageName := fmt.Sprintf("%s-disk-%d-%s", o.plan.VMName, disk.Key, time.Now().Format("20060102-150405"))
			imageUUID, err := o.nxClient.CreateImage(ctx, imageName, qcow2Stat.Size())
			if err != nil {
				o.SetError(ms, err)
				return fmt.Errorf("creating image: %w", err)
			}

			if err := o.nxClient.UploadImage(ctx, imageUUID, qcow2Path); err != nil {
				o.SetError(ms, err)
				return fmt.Errorf("uploading qcow2: %w", err)
			}

			disk.ImageUUID = imageUUID
			disk.LocalPath = qcow2Path
			disk.ChangeID = changeID
			disk.BytesCopied = disk.CapacityB

			// Cleanup VMDK (keep qcow2 for potential re-upload)
			os.Remove(vmdkPath)

			log.Info().
				Str("image_uuid", imageUUID).
				Int32("disk_key", disk.Key).
				Msg("disk uploaded to Nutanix")

			if err := o.store.SaveMigration(ms); err != nil {
				return fmt.Errorf("saving disk state: %w", err)
			}

			log.Info().
				Int32("disk_key", disk.Key).
				Str("change_id", disk.ChangeID).
				Str("transport", string(ms.Transport)).
				Msg("disk full sync complete")

			continue // skip the pipeline path below

		case state.TransportImage:
			// qcow2 file to local disk, then upload via API
			w, err := o.createImageWriter(ms, disk)
			if err != nil {
				reader.Close()
				o.SetError(ms, err)
				return fmt.Errorf("creating qcow2 writer: %w", err)
			}
			writer = w

		case state.TransportRepository:
			// Repository transport: NFC → parse VMDK → raw file → qcow2 → upload
			nfcReader := reader.(*vmware.DiskReader)

			stagingDir := o.plan.Staging.Directory
			if stagingDir == "" {
				stagingDir = "/tmp/datamigrate"
			}
			diskDir := filepath.Join(stagingDir, o.plan.Name)

			qcow2Path, qcow2Size, err := repository.RunT0(ctx, nfcReader.StreamReader(), repository.T0Config{
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
			disk.LastSyncedAt = time.Now()

			log.Info().Str("image_uuid", imageUUID).Str("qcow2", qcow2Path).Msg("repository T0 complete")

			if err := o.store.SaveMigration(ms); err != nil {
				return fmt.Errorf("saving disk state: %w", err)
			}
			continue // skip the pipeline path below

		default:
			reader.Close()
			return fmt.Errorf("unknown transport mode: %s", ms.Transport)
		}

		// Choose pipeline config based on writer type
		pipelineConfig := blockio.DefaultPipelineConfig()

		// Run pipeline with live progress output
		pipeline := blockio.NewPipeline(reader, writer, pipelineConfig)
		lastPrint := time.Now()
		lastLog := time.Now()
		if err := pipeline.Run(ctx, extents, func(transferred, total int64) {
			progress.Update(disk.Key, transferred)
			now := time.Now()
			// Print progress every 5 seconds
			if now.Sub(lastPrint) >= 5*time.Second {
				fmt.Printf("\r  [disk-%d] %s", disk.Key, progress.String())
				lastPrint = now
			}
			// Structured log every 60 seconds for visibility
			if now.Sub(lastLog) >= 60*time.Second {
				log.Info().
					Int32("disk_key", disk.Key).
					Str("progress", fmt.Sprintf("%.1f%%", progress.Percentage())).
					Str("copied", fmt.Sprintf("%d/%d MB", transferred/(1024*1024), total/(1024*1024))).
					Str("rate", fmt.Sprintf("%.1f MB/s", progress.Rate()/(1024*1024))).
					Str("eta", progress.ETA().Truncate(time.Second).String()).
					Msg("disk copy in progress")
				lastLog = now
			}
		}); err != nil {
			reader.Close()
			if iscsiWriter != nil {
				iscsiWriter.Disconnect(ctx)
			}
			o.SetError(ms, err)
			return fmt.Errorf("running pipeline: %w", err)
		}

		reader.Close()
		fmt.Printf("\r  [disk-%d] %s\n", disk.Key, progress.String())

		// Finalize writer
		if err := writer.Finalize(); err != nil {
			if iscsiWriter != nil {
				iscsiWriter.Disconnect(ctx)
			}
			o.SetError(ms, err)
			return fmt.Errorf("finalizing writer: %w", err)
		}

		// Disconnect iSCSI after finalize
		if iscsiWriter != nil {
			if err := iscsiWriter.Disconnect(ctx); err != nil {
				log.Warn().Err(err).Msg("iSCSI disconnect failed")
			}
		}

		// Update disk state
		disk.ChangeID = changeID
		disk.BytesCopied = disk.CapacityB

		// Save image UUID for stream writer
		if sw, ok := writer.(*blockio.StreamWriter); ok {
			disk.ImageUUID = sw.ImageUUID()
		}

		// For image transport, upload to Nutanix
		if ms.Transport == state.TransportImage {
			qcow2W := writer.(*blockio.Qcow2Writer)
			disk.LocalPath = qcow2W.Qcow2Path()
			if err := o.uploadDiskImage(ctx, ms, disk); err != nil {
				o.SetError(ms, err)
				return err
			}
		}

		if err := o.store.SaveMigration(ms); err != nil {
			return fmt.Errorf("saving disk state: %w", err)
		}

		log.Info().
			Int32("disk_key", disk.Key).
			Str("change_id", disk.ChangeID).
			Str("transport", string(ms.Transport)).
			Msg("disk full sync complete")
	}

	// Re-authenticate before cleanup — session may have expired during long copy
	if err := o.vmClient.Relogin(ctx); err != nil {
		log.Warn().Err(err).Msg("failed to re-authenticate, snapshot removal may fail")
	}

	// Remove snapshots we created (using saved MoRefs)
	o.removeTrackedSnapshots(ctx, vm, ms)

	// Transition to SYNCING
	if err := o.TransitionTo(ms, state.StatusSyncing); err != nil {
		return err
	}

	elapsed := time.Since(syncStart).Truncate(time.Second)
	var totalBytes int64
	for _, d := range ms.Disks {
		totalBytes += d.BytesCopied
	}
	fmt.Printf("\nFull sync complete: %s transferred in %s\n", util.HumanSize(totalBytes), elapsed)

	ms.SyncCount = 1
	return o.store.SaveMigration(ms)
}

// setupVolumeGroup creates a Nutanix Volume Group with disks for each VM disk.
func (o *Orchestrator) setupVolumeGroup(ctx context.Context, ms *state.MigrationState) error {
	vgName := fmt.Sprintf("datamigrate-%s", o.plan.VMName)

	var vgDisks []nutanix.VGDisk
	for i, disk := range ms.Disks {
		vgDisks = append(vgDisks, nutanix.VGDisk{
			DiskSizeBytes: disk.CapacityB,
			Index:         i,
		})
		ms.Disks[i].VGDiskIndex = i
	}

	clusterUUID := o.plan.TargetVMSpec.ClusterUUID
	if clusterUUID == "" {
		clusterUUID = o.plan.Target.ClusterUUID
	}

	storageContainerUUID := o.plan.Target.StorageContainerUUID

	vg, err := o.nxClient.CreateVolumeGroup(ctx, vgName, vgDisks, clusterUUID, storageContainerUUID)
	if err != nil {
		return fmt.Errorf("creating volume group: %w", err)
	}

	ms.VolumeGroupID = vg.UUID

	log.Info().
		Str("vg_uuid", vg.UUID).
		Str("iscsi_target", vg.ISCSITarget).
		Int("disks", len(vgDisks)).
		Msg("volume group created for migration")

	return o.store.SaveMigration(ms)
}

// createISCSIWriter creates an iSCSI block writer connected to the Volume Group.
func (o *Orchestrator) createISCSIWriter(ctx context.Context, ms *state.MigrationState, disk *state.DiskState) (*blockio.ISCSIWriter, error) {
	portal, err := o.nxClient.GetISCSIPortal(ctx, ms.VolumeGroupID)
	if err != nil {
		return nil, fmt.Errorf("getting iSCSI portal: %w", err)
	}

	writer, err := blockio.NewISCSIWriter(blockio.ISCSIConfig{
		TargetIQN:     portal.TargetIQN,
		PortalIP:      portal.PortalIP,
		PortalPort:    portal.PortalPort,
		MaxWriteBytes: o.plan.ISCSIChunkBytes,
	})
	if err != nil {
		return nil, err
	}

	if err := writer.Connect(ctx); err != nil {
		return nil, fmt.Errorf("connecting iSCSI: %w", err)
	}

	return writer, nil
}

// createImageWriter creates a qcow2 writer for legacy image transport.
func (o *Orchestrator) createImageWriter(ms *state.MigrationState, disk *state.DiskState) (*blockio.Qcow2Writer, error) {
	stagingDir := o.plan.Staging.Directory
	if stagingDir == "" {
		stagingDir = "/tmp/datamigrate"
	}
	diskDir := filepath.Join(stagingDir, o.plan.Name)
	return blockio.NewQcow2Writer(diskDir, disk.Key, disk.CapacityB)
}

// uploadDiskImage uploads a qcow2 file to Nutanix (image transport only).
func (o *Orchestrator) uploadDiskImage(ctx context.Context, ms *state.MigrationState, disk *state.DiskState) error {
	imageName := fmt.Sprintf("%s-disk-%d-%s", o.plan.VMName, disk.Key, time.Now().Format("20060102-150405"))

	uuid, err := o.nxClient.CreateImage(ctx, imageName, disk.CapacityB)
	if err != nil {
		return fmt.Errorf("creating Nutanix image: %w", err)
	}

	if err := o.nxClient.UploadImage(ctx, uuid, disk.LocalPath); err != nil {
		return fmt.Errorf("uploading image: %w", err)
	}

	disk.ImageUUID = uuid
	return nil
}
