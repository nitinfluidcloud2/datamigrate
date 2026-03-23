package transport

// Mode represents the disk transport method.
type Mode string

const (
	ModeNBD  Mode = "nbd"
	ModeVDDK Mode = "vddk"
)

// String returns the string representation.
func (m Mode) String() string {
	return string(m)
}
