package blockio

// BlockExtent represents a range of blocks on a disk.
type BlockExtent struct {
	Offset int64
	Length int64
}

// BlockData holds the actual data for a block extent.
type BlockData struct {
	DiskKey int32
	Offset  int64
	Length  int64
	Data    []byte
}
