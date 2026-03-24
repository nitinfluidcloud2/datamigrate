package repository

import (
	"os"
	"os/exec"
	"testing"
)

// TestCheckQemuImg verifies detection of qemu-img (skip if not installed).
func TestCheckQemuImg(t *testing.T) {
	_, err := exec.LookPath("qemu-img")
	if err != nil {
		t.Skip("qemu-img not installed, skipping")
	}
	if err := CheckQemuImg(); err != nil {
		t.Fatalf("expected qemu-img to be found, got error: %v", err)
	}
}

// TestVerifyRawFile_InvalidFile verifies error for nonexistent file.
func TestVerifyRawFile_InvalidFile(t *testing.T) {
	err := VerifyRawFile("/nonexistent/path/to/file.raw")
	if err == nil {
		t.Fatal("expected error for nonexistent file, got nil")
	}
}

// TestVerifyRawFile_EmptyFile verifies error for an empty file.
func TestVerifyRawFile_EmptyFile(t *testing.T) {
	f, err := os.CreateTemp("", "empty-*.raw")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(f.Name())
	f.Close()

	err = VerifyRawFile(f.Name())
	if err == nil {
		t.Fatal("expected error for empty file, got nil")
	}
}

// TestVerifyRawFile_ValidFile creates a temp file with data and verifies success.
func TestVerifyRawFile_ValidFile(t *testing.T) {
	f, err := os.CreateTemp("", "valid-*.raw")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(f.Name())

	// Write some data so the file is non-empty
	if _, err := f.Write([]byte("fake raw disk data for testing")); err != nil {
		f.Close()
		t.Fatalf("failed to write to temp file: %v", err)
	}
	f.Close()

	if err := VerifyRawFile(f.Name()); err != nil {
		t.Fatalf("expected success for valid file, got error: %v", err)
	}
}
