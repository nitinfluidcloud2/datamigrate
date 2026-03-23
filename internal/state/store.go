package state

import (
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

var migrationBucket = []byte("migrations")

// Store provides persistent state storage using BoltDB.
type Store struct {
	db *bolt.DB
}

// NewStore opens or creates a BoltDB state file.
func NewStore(path string) (*Store, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("opening state db: %w", err)
	}

	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(migrationBucket)
		return err
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("creating bucket: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// SaveMigration persists a migration state.
func (s *Store) SaveMigration(state *MigrationState) error {
	state.UpdatedAt = time.Now()
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(migrationBucket)
		data, err := json.Marshal(state)
		if err != nil {
			return err
		}
		return b.Put([]byte(state.PlanName), data)
	})
}

// GetMigration retrieves a migration state by plan name.
func (s *Store) GetMigration(planName string) (*MigrationState, error) {
	var state MigrationState
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(migrationBucket)
		data := b.Get([]byte(planName))
		if data == nil {
			return fmt.Errorf("migration %q not found", planName)
		}
		return json.Unmarshal(data, &state)
	})
	if err != nil {
		return nil, err
	}
	return &state, nil
}

// DeleteMigration removes a migration state.
func (s *Store) DeleteMigration(planName string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(migrationBucket)
		return b.Delete([]byte(planName))
	})
}

// ListMigrations returns all stored migration states.
func (s *Store) ListMigrations() ([]MigrationState, error) {
	var states []MigrationState
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(migrationBucket)
		return b.ForEach(func(k, v []byte) error {
			var state MigrationState
			if err := json.Unmarshal(v, &state); err != nil {
				return err
			}
			states = append(states, state)
			return nil
		})
	})
	return states, err
}
