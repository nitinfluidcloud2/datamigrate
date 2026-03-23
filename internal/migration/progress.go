package migration

import (
	"fmt"
	"sync"
	"time"

	"github.com/nitinmore/datamigrate/internal/util"
)

// Progress tracks migration progress with ETA calculation.
type Progress struct {
	mu           sync.Mutex
	totalBytes   int64
	copiedBytes  int64
	startTime    time.Time
	lastUpdate   time.Time
	diskProgress map[int32]*DiskProgress
}

// DiskProgress tracks per-disk progress.
type DiskProgress struct {
	Key         int32
	TotalBytes  int64
	CopiedBytes int64
	StartTime   time.Time
}

// NewProgress creates a new progress tracker.
func NewProgress() *Progress {
	return &Progress{
		startTime:    time.Now(),
		lastUpdate:   time.Now(),
		diskProgress: make(map[int32]*DiskProgress),
	}
}

// AddDisk registers a disk for tracking.
func (p *Progress) AddDisk(key int32, totalBytes int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.totalBytes += totalBytes
	p.diskProgress[key] = &DiskProgress{
		Key:        key,
		TotalBytes: totalBytes,
		StartTime:  time.Now(),
	}
}

// Update sets the absolute bytes transferred for a disk.
// Recalculates overall copiedBytes from all disks.
func (p *Progress) Update(diskKey int32, bytes int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastUpdate = time.Now()
	if dp, ok := p.diskProgress[diskKey]; ok {
		dp.CopiedBytes = bytes
	}
	// Recalculate total from all disks
	var total int64
	for _, dp := range p.diskProgress {
		total += dp.CopiedBytes
	}
	p.copiedBytes = total
}

// Percentage returns the overall completion percentage.
func (p *Progress) Percentage() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.totalBytes == 0 {
		return 0
	}
	return float64(p.copiedBytes) / float64(p.totalBytes) * 100
}

// ETA returns the estimated time remaining.
func (p *Progress) ETA() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.copiedBytes == 0 {
		return 0
	}
	elapsed := time.Since(p.startTime)
	rate := float64(p.copiedBytes) / elapsed.Seconds()
	remaining := float64(p.totalBytes-p.copiedBytes) / rate
	return time.Duration(remaining * float64(time.Second))
}

// Rate returns the current transfer rate in bytes/sec.
func (p *Progress) Rate() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	elapsed := time.Since(p.startTime).Seconds()
	if elapsed == 0 {
		return 0
	}
	return float64(p.copiedBytes) / elapsed
}

// String returns a human-readable progress summary.
func (p *Progress) String() string {
	pct := p.Percentage()
	rate := p.Rate()
	eta := p.ETA()

	p.mu.Lock()
	copied := p.copiedBytes
	total := p.totalBytes
	p.mu.Unlock()

	return fmt.Sprintf("%.1f%% (%s / %s) @ %s/s ETA: %s",
		pct,
		util.HumanSize(copied),
		util.HumanSize(total),
		util.HumanSize(int64(rate)),
		eta.Truncate(time.Second),
	)
}
