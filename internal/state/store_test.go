package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreLifecycle(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	// Save
	ms := &MigrationState{
		PlanName:  "test-plan",
		VMName:    "test-vm",
		Status:    StatusCreated,
		CreatedAt: time.Now(),
		Disks: []DiskState{
			{Key: 2000, FileName: "[ds1] test/test.vmdk", CapacityB: 10737418240},
		},
	}
	if err := store.SaveMigration(ms); err != nil {
		t.Fatalf("SaveMigration: %v", err)
	}

	// Get
	got, err := store.GetMigration("test-plan")
	if err != nil {
		t.Fatalf("GetMigration: %v", err)
	}
	if got.VMName != "test-vm" {
		t.Errorf("VMName = %q, want %q", got.VMName, "test-vm")
	}
	if got.Status != StatusCreated {
		t.Errorf("Status = %q, want %q", got.Status, StatusCreated)
	}
	if len(got.Disks) != 1 {
		t.Fatalf("Disks count = %d, want 1", len(got.Disks))
	}

	// Update
	ms.Status = StatusFullSync
	if err := store.SaveMigration(ms); err != nil {
		t.Fatalf("SaveMigration update: %v", err)
	}
	got, _ = store.GetMigration("test-plan")
	if got.Status != StatusFullSync {
		t.Errorf("Status after update = %q, want %q", got.Status, StatusFullSync)
	}

	// List
	list, err := store.ListMigrations()
	if err != nil {
		t.Fatalf("ListMigrations: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("ListMigrations count = %d, want 1", len(list))
	}

	// Delete
	if err := store.DeleteMigration("test-plan"); err != nil {
		t.Fatalf("DeleteMigration: %v", err)
	}
	_, err = store.GetMigration("test-plan")
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestJournal(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	if err := store.InitJournal(); err != nil {
		t.Fatalf("InitJournal: %v", err)
	}

	// Write entries
	if err := store.WriteJournal("plan1", JournalEntry{DiskKey: 1, Offset: 0, Length: 65536}); err != nil {
		t.Fatalf("WriteJournal: %v", err)
	}
	if err := store.WriteJournal("plan1", JournalEntry{DiskKey: 1, Offset: 65536, Length: 65536}); err != nil {
		t.Fatalf("WriteJournal: %v", err)
	}

	// Read entries
	entries, err := store.GetJournalEntries("plan1")
	if err != nil {
		t.Fatalf("GetJournalEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("entries count = %d, want 2", len(entries))
	}

	// Clear
	if err := store.ClearJournal("plan1"); err != nil {
		t.Fatalf("ClearJournal: %v", err)
	}
	entries, _ = store.GetJournalEntries("plan1")
	if len(entries) != 0 {
		t.Errorf("entries after clear = %d, want 0", len(entries))
	}

	// Suppress unused
	_ = os.TempDir()
}
