package util

import "testing"

func TestHumanSize(t *testing.T) {
	tests := []struct {
		bytes    int64
		expected string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.00 KB"},
		{1536, "1.50 KB"},
		{1048576, "1.00 MB"},
		{1073741824, "1.00 GB"},
		{1099511627776, "1.00 TB"},
		{5368709120, "5.00 GB"},
	}

	for _, tt := range tests {
		result := HumanSize(tt.bytes)
		if result != tt.expected {
			t.Errorf("HumanSize(%d) = %q, want %q", tt.bytes, result, tt.expected)
		}
	}
}
