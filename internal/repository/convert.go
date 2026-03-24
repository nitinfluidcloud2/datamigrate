package repository

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/rs/zerolog/log"
)

// CheckQemuImg verifies qemu-img is installed and returns its path.
func CheckQemuImg() error {
	path, err := exec.LookPath("qemu-img")
	if err != nil {
		return fmt.Errorf("qemu-img not found: install with 'yum install qemu-img' or 'brew install qemu'")
	}
	log.Info().Str("path", path).Msg("qemu-img found")
	return nil
}

// ConvertRawToQcow2 converts a raw disk image to compressed qcow2.
func ConvertRawToQcow2(rawPath, qcow2Path string) error {
	log.Info().Str("raw", rawPath).Str("qcow2", qcow2Path).Msg("converting raw → qcow2")
	cmd := exec.Command("qemu-img", "convert", "-f", "raw", "-O", "qcow2", "-c", rawPath, qcow2Path)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("qemu-img convert: %w", err)
	}
	stat, err := os.Stat(qcow2Path)
	if err != nil {
		return fmt.Errorf("stating qcow2: %w", err)
	}
	log.Info().Str("qcow2", qcow2Path).Int64("size_mb", stat.Size()/(1024*1024)).Msg("qcow2 conversion complete")
	return nil
}

// ConvertVMDKToRaw converts a streamOptimized VMDK to raw format using qemu-img.
func ConvertVMDKToRaw(vmdkPath, rawPath string) error {
	log.Info().Str("vmdk", vmdkPath).Str("raw", rawPath).Msg("converting VMDK → raw")
	cmd := exec.Command("qemu-img", "convert", "-f", "vmdk", "-O", "raw", vmdkPath, rawPath)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("qemu-img convert vmdk→raw: %w", err)
	}
	stat, err := os.Stat(rawPath)
	if err != nil {
		return fmt.Errorf("stating raw: %w", err)
	}
	log.Info().Str("raw", rawPath).Int64("size_mb", stat.Size()/(1024*1024)).Msg("VMDK → raw conversion complete")
	return nil
}

// VerifyRawFile runs basic sanity checks on the raw disk image.
func VerifyRawFile(rawPath string) error {
	stat, err := os.Stat(rawPath)
	if err != nil {
		return fmt.Errorf("raw file not found: %w", err)
	}
	if stat.Size() == 0 {
		return fmt.Errorf("raw file is empty")
	}
	// Check with 'file -s' if available
	cmd := exec.Command("file", "-s", rawPath)
	output, err := cmd.Output()
	if err == nil {
		log.Info().Str("file_type", string(output)).Msg("raw file type check")
	}
	return nil
}
