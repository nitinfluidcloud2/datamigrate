package state

import (
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

var journalBucket = []byte("journal")

// JournalEntry records a completed block transfer for resumability.
type JournalEntry struct {
	DiskKey    int32  `json:"disk_key"`
	Offset     int64  `json:"offset"`
	Length     int32  `json:"length"`
	Checksum   uint32 `json:"checksum"`
}

// InitJournal creates the journal bucket if it doesn't exist.
func (s *Store) InitJournal() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(journalBucket)
		return err
	})
}

// WriteJournal records a completed block transfer.
func (s *Store) WriteJournal(planName string, entry JournalEntry) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(journalBucket)
		planBucket, err := b.CreateBucketIfNotExists([]byte(planName))
		if err != nil {
			return err
		}
		key := fmt.Sprintf("%d:%d", entry.DiskKey, entry.Offset)
		data, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		return planBucket.Put([]byte(key), data)
	})
}

// GetJournalEntries returns all journal entries for a plan.
func (s *Store) GetJournalEntries(planName string) ([]JournalEntry, error) {
	var entries []JournalEntry
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(journalBucket)
		planBucket := b.Bucket([]byte(planName))
		if planBucket == nil {
			return nil
		}
		return planBucket.ForEach(func(k, v []byte) error {
			var entry JournalEntry
			if err := json.Unmarshal(v, &entry); err != nil {
				return err
			}
			entries = append(entries, entry)
			return nil
		})
	})
	return entries, err
}

// ClearJournal removes all journal entries for a plan.
func (s *Store) ClearJournal(planName string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(journalBucket)
		if b == nil {
			return nil
		}
		if b.Bucket([]byte(planName)) == nil {
			return nil
		}
		return b.DeleteBucket([]byte(planName))
	})
}
