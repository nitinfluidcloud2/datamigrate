package migration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/nitinmore/datamigrate/internal/blockio"
	"github.com/nitinmore/datamigrate/internal/repository"
	"github.com/nitinmore/datamigrate/internal/state"
	"github.com/nitinmore/datamigrate/internal/util"
	"github.com/nitinmore/datamigrate/internal/vmware"
)

// IncrementalSync performs a delta sync using CBT (T1..TN).
// With iSCSI transport, only changed blocks cross the network.
// With image transport, the full qcow2 is re-uploaded each time.
func (o *Orchestrator) IncrementalSync(ctx context.Context) error {
	syncStart := time.Now()

	ms, err := o.GetState()
	if err != nil {
		return fmt.Errorf("getting migration state: %w", err)
	}

	if ms.Status != state.StatusSyncing && ms.Status != state.StatusCutoverReady {
		return fmt.Errorf("incremental sync requires SYNCING or CUTOVER_READY state, currently %s", ms.Status)
	}

	vm, _, err := o.vmClient.FindVM(ctx, "", o.plan.VMName)
	if err != nil {
		return fmt.Errorf("finding VM: %w", err)
	}

	// Ensure CBT is still enabled (can get reset after snapshot removal)
	if err := o.vmClient.EnableCBT(ctx, vm); err != nil {
		log.Warn().Err(err).Msg("could not re-enable CBT, continuing anyway")
	}

	// For iSCSI transport, ensure our initiator is still whitelisted on the VG.
	// It can be lost if the VG was attached/detached from a VM.
	if ms.Transport == state.TransportISCSI && ms.VolumeGroupID != "" {
		initiatorIQN := "iqn.2026-01.com.datamigrate:initiator"
		if err := o.nxClient.AttachISCSIClient(ctx, ms.VolumeGroupID, initiatorIQN); err != nil {
			log.Warn().Err(err).Msg("could not re-attach iSCSI initiator, sync may fail")
		}
	}

	// Create snapshot for this sync
	snapName := fmt.Sprintf("datamigrate-t%d-%s", ms.SyncCount, o.plan.Name)
	snapRef, err := o.vmClient.CreateSnapshot(ctx, vm, snapName, "Incremental sync snapshot")
	if err != nil {
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

	progress := NewProgress()
	totalChanged := int64(0)

	for i := range ms.Disks {
		disk := &ms.Disks[i]
		diskInfo := vmware.DiskInfo{
			Key:        disk.Key,
			FileName:   disk.FileName,
			CapacityKB: disk.CapacityB / 1024,
		}

		// Query only changed blocks since last sync
		changeID := disk.ChangeID
		if changeID == "" {
			log.Warn().Int32("disk_key", disk.Key).Msg("no changeID, falling back to full sync for disk")
			changeID = "*"
		}

		areas, newChangeID, err := o.vmClient.QueryChangedBlocks(ctx, vm, snapRef, diskInfo, changeID)
		if err != nil {
			if changeID != "*" {
				// CBT invalidated — fall back to full query
				log.Warn().Err(err).Int32("disk_key", disk.Key).Msg("CBT query failed, falling back to full sync with changeId=*")
				areas, newChangeID, err = o.vmClient.QueryChangedBlocks(ctx, vm, snapRef, diskInfo, "*")
			}
			if err != nil {
				// QueryChangedDiskAreas completely failed (even with "*") —
				// fall back to reading the entire disk without CBT
				log.Warn().Err(err).Int32("disk_key", disk.Key).Msg("CBT query with * also failed, copying entire disk")
				areas = []vmware.ChangedArea{{Offset: 0, Length: disk.CapacityB}}
				newChangeID, _ = o.vmClient.GetSnapshotDiskChangeID(ctx, snapRef, disk.Key)
				err = nil
			}
		}

		var extents []blockio.BlockExtent
		diskChanged := int64(0)
		for _, area := range areas {
			extents = append(extents, blockio.BlockExtent{
				Offset: area.Offset,
				Length: area.Length,
			})
			diskChanged += area.Length
		}
		totalChanged += diskChanged

		if len(extents) == 0 {
			log.Info().Int32("disk_key", disk.Key).Msg("no changes detected, skipping")
			disk.ChangeID = newChangeID
			disk.LastSyncedAt = time.Now()
			continue
		}

		log.Info().
			Int32("disk_key", disk.Key).
			Int("changed_areas", len(extents)).
			Int64("changed_bytes", diskChanged).
			Str("transport", string(ms.Transport)).
			Msg("incremental sync for disk")

		progress.AddDisk(disk.Key, diskChanged)

		// Repository transport: NFC → temp VMDK → temp raw → patch extents → qcow2 → upload
		if ms.Transport == state.TransportRepository {
			nfcReader, err := o.vmClient.OpenDiskReader(ctx, vm, snapRef, diskInfo)
			if err != nil {
				return fmt.Errorf("opening NFC reader for T1: %w", err)
			}

			stagingDir := o.plan.Staging.Directory
			if stagingDir == "" {
				stagingDir = "/tmp/datamigrate"
			}
			diskDir := filepath.Join(stagingDir, o.plan.Name)

			rawPath := disk.LocalPath // repository raw file from T0
			if rawPath == "" {
				rawPath = filepath.Join(diskDir, fmt.Sprintf("disk-%d.raw", disk.Key))
			}

			fmt.Printf("  Incremental sync (repository): %d changed areas, %d MB\n",
				len(extents), diskChanged/(1024*1024))

			patchedBytes, err := repository.RunT1(ctx, nfcReader.StreamReader(), repository.T1Config{
				StagingDir:     diskDir,
				DiskKey:        disk.Key,
				Capacity:       disk.CapacityB,
				RawPath:        rawPath,
				ChangedExtents: extents,
			})
			nfcReader.Close()
			if err != nil {
				return fmt.Errorf("repository T1: %w", err)
			}

			fmt.Printf("  Patched %d MB into repository\n", patchedBytes/(1024*1024))

			// Upload updated qcow2
			qcow2Path := filepath.Join(diskDir, fmt.Sprintf("disk-%d.qcow2", disk.Key))
			imageName := fmt.Sprintf("%s-disk-%d-t%d-%s", o.plan.VMName, disk.Key, ms.SyncCount, time.Now().Format("20060102-150405"))
			fmt.Printf("  Creating image %q...\n", imageName)

			qcow2Stat, err := os.Stat(qcow2Path)
			if err != nil {
				return fmt.Errorf("stating qcow2: %w", err)
			}

			imageUUID, err := o.nxClient.CreateImage(ctx, imageName, qcow2Stat.Size())
			if err != nil {
				return fmt.Errorf("creating image: %w", err)
			}

			fmt.Printf("  Uploading qcow2 (%d MB)...\n", qcow2Stat.Size()/(1024*1024))
			if err := o.nxClient.UploadImage(ctx, imageUUID, qcow2Path); err != nil {
				return fmt.Errorf("uploading image: %w", err)
			}
			fmt.Println("  Image uploaded successfully.")

			disk.ImageUUID = imageUUID
			disk.ChangeID = newChangeID
			disk.BytesCopied += diskChanged
			disk.LastSyncedAt = time.Now()
			continue
		}

		// Open reader — iSCSI needs RangeReader for correct offset writes,
		// stream/image uses NFC reader (sequential stream)
		var reader blockio.BlockReader
		if ms.Transport == state.TransportISCSI {
			r, err := o.vmClient.OpenRangeReader(ctx, vm, snapRef, diskInfo)
			if err != nil {
				return fmt.Errorf("opening range reader: %w", err)
			}
			reader = r
		} else {
			r, err := o.vmClient.OpenDiskReader(ctx, vm, snapRef, diskInfo)
			if err != nil {
				return fmt.Errorf("opening disk reader: %w", err)
			}
			reader = r
		}

		// Create writer based on transport mode
		var writer blockio.BlockWriter
		if ms.Transport == state.TransportISCSI {
			w, err := o.createISCSIWriter(ctx, ms, disk)
			if err != nil {
				reader.Close()
				return fmt.Errorf("creating iSCSI writer: %w", err)
			}
			writer = w
			defer w.Disconnect(ctx)
		} else {
			w, err := o.createImageWriter(ms, disk)
			if err != nil {
				reader.Close()
				return fmt.Errorf("creating qcow2 writer: %w", err)
			}
			writer = w
		}

		// Run pipeline with live progress output
		pipeline := blockio.NewPipeline(reader, writer, blockio.DefaultPipelineConfig())
		lastPrint := time.Now()
		lastLog := time.Now()
		if err := pipeline.Run(ctx, extents, func(transferred, total int64) {
			progress.Update(disk.Key, transferred)
			now := time.Now()
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
					Msg("incremental copy in progress")
				lastLog = now
			}
		}); err != nil {
			reader.Close()
			return fmt.Errorf("running incremental pipeline: %w", err)
		}

		reader.Close()
		fmt.Printf("\r  [disk-%d] %s\n", disk.Key, progress.String())

		if err := writer.Finalize(); err != nil {
			return fmt.Errorf("finalizing writer: %w", err)
		}

		// For stream/image transport, create a new image and upload the qcow2
		if ms.Transport == state.TransportStream || ms.Transport == state.TransportImage {
			qcow2W := writer.(*blockio.Qcow2Writer)
			disk.LocalPath = qcow2W.Qcow2Path()

			qcow2Stat, err := os.Stat(qcow2W.Qcow2Path())
			if err != nil {
				return fmt.Errorf("stating qcow2: %w", err)
			}

			imageName := fmt.Sprintf("%s-disk-%d-t%d-%s", o.plan.VMName, disk.Key, ms.SyncCount, time.Now().Format("20060102-150405"))
			fmt.Printf("Creating new image %q...\n", imageName)
			imageUUID, err := o.nxClient.CreateImage(ctx, imageName, qcow2Stat.Size())
			if err != nil {
				return fmt.Errorf("creating image: %w", err)
			}
			disk.ImageUUID = imageUUID

			fmt.Printf("Uploading qcow2 (%d MB)...\n", qcow2Stat.Size()/(1024*1024))
			if err := o.nxClient.UploadImage(ctx, disk.ImageUUID, qcow2W.Qcow2Path()); err != nil {
				return fmt.Errorf("uploading image: %w", err)
			}
			fmt.Println("Image uploaded successfully.")
		}
		// For iSCSI transport: nothing extra needed — blocks already written to VG

		disk.ChangeID = newChangeID
		disk.BytesCopied += diskChanged
		disk.LastSyncedAt = time.Now()
	}

	// Re-authenticate before cleanup — session may have expired during long copy
	if err := o.vmClient.Relogin(ctx); err != nil {
		log.Warn().Err(err).Msg("failed to re-authenticate, snapshot removal may fail")
	}

	// Remove snapshots we created (using saved MoRefs)
	o.removeTrackedSnapshots(ctx, vm, ms)

	ms.SyncCount++
	if err := o.TransitionTo(ms, state.StatusCutoverReady); err != nil {
		if ms.Status == state.StatusCutoverReady {
			return o.store.SaveMigration(ms)
		}
		return err
	}

	elapsed := time.Since(syncStart).Truncate(time.Second)
	log.Info().
		Int("sync_count", ms.SyncCount).
		Int64("changed_bytes", totalChanged).
		Str("transport", string(ms.Transport)).
		Str("elapsed", elapsed.String()).
		Msg("incremental sync complete")

	fmt.Printf("\nSync complete: %s transferred in %s\n", util.HumanSize(totalChanged), elapsed)

	return o.store.SaveMigration(ms)
}
