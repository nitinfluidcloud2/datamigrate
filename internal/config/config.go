package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the top-level configuration.
type Config struct {
	Source  SourceConfig  `yaml:"source"`
	Target TargetConfig  `yaml:"target"`
	Staging StagingConfig `yaml:"staging"`
}

// SourceConfig holds VMware vCenter connection details.
type SourceConfig struct {
	VCenter  string `yaml:"vcenter"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	Insecure bool   `yaml:"insecure"`
}

// TargetConfig holds Nutanix Prism Central connection details.
type TargetConfig struct {
	PrismCentral         string `yaml:"prism_central"`
	Username             string `yaml:"username"`
	Password             string `yaml:"password"`
	Insecure             bool   `yaml:"insecure"`
	ClusterUUID          string `yaml:"cluster_uuid"`
	StorageContainerUUID string `yaml:"storage_container_uuid,omitempty"`
}

// StagingConfig holds local staging directory settings.
type StagingConfig struct {
	Directory string `yaml:"directory"`
}

// Load reads configuration from two credentials files in the given directory.
//
// vmware.creds — VMware vCenter connection:
//
//	host, username, password, insecure
//
// nutanix.creds — Nutanix Prism Central connection:
//
//	host, username, password, insecure, cluster_uuid
//
// The path argument can be either:
//   - A directory path containing the .creds files
//   - A file path (the directory containing it is used to find .creds files)
//
// Environment variables (DATAMIGRATE_SOURCE_PASSWORD, etc.) override creds files.
func Load(path string) (*Config, error) {
	// Determine the directory containing the creds files
	dir := path
	info, err := os.Stat(path)
	if err == nil && !info.IsDir() {
		dir = filepath.Dir(path)
	}

	cfg := &Config{
		Source:  SourceConfig{Insecure: true},
		Target:  TargetConfig{Insecure: true},
		Staging: StagingConfig{Directory: "/tmp/datamigrate"},
	}

	// Load from vmware.creds
	vmwareCreds := loadCredsFile(dir, "vmware.creds")
	if vmwareCreds == nil {
		return nil, fmt.Errorf("vmware.creds not found in %s", dir)
	}
	cfg.Source.VCenter = vmwareCreds["host"]
	cfg.Source.Username = vmwareCreds["username"]
	cfg.Source.Password = vmwareCreds["password"]
	if v, ok := vmwareCreds["insecure"]; ok {
		cfg.Source.Insecure = v == "true" || v == "yes" || v == "1"
	}

	// Load from nutanix.creds
	nutanixCreds := loadCredsFile(dir, "nutanix.creds")
	if nutanixCreds == nil {
		return nil, fmt.Errorf("nutanix.creds not found in %s", dir)
	}
	cfg.Target.PrismCentral = nutanixCreds["host"]
	cfg.Target.Username = nutanixCreds["username"]
	cfg.Target.Password = nutanixCreds["password"]
	cfg.Target.ClusterUUID = nutanixCreds["cluster_uuid"]
	cfg.Target.StorageContainerUUID = nutanixCreds["storage_container_uuid"]
	if v, ok := nutanixCreds["insecure"]; ok {
		cfg.Target.Insecure = v == "true" || v == "yes" || v == "1"
	}
	if v, ok := nutanixCreds["staging_directory"]; ok {
		cfg.Staging.Directory = v
	}

	// Environment variables override creds files
	if v := os.Getenv("DATAMIGRATE_SOURCE_VCENTER"); v != "" {
		cfg.Source.VCenter = v
	}
	if v := os.Getenv("DATAMIGRATE_SOURCE_USERNAME"); v != "" {
		cfg.Source.Username = v
	}
	if v := os.Getenv("DATAMIGRATE_SOURCE_PASSWORD"); v != "" {
		cfg.Source.Password = v
	}
	if v := os.Getenv("DATAMIGRATE_TARGET_PRISM_CENTRAL"); v != "" {
		cfg.Target.PrismCentral = v
	}
	if v := os.Getenv("DATAMIGRATE_TARGET_USERNAME"); v != "" {
		cfg.Target.Username = v
	}
	if v := os.Getenv("DATAMIGRATE_TARGET_PASSWORD"); v != "" {
		cfg.Target.Password = v
	}

	// Validate required fields
	if cfg.Source.VCenter == "" {
		return nil, fmt.Errorf("VMware host not set: add 'host' to vmware.creds")
	}
	if cfg.Source.Username == "" {
		return nil, fmt.Errorf("VMware username not set: add 'username' to vmware.creds")
	}
	if cfg.Source.Password == "" {
		return nil, fmt.Errorf("VMware password not set: add 'password' to vmware.creds")
	}
	if cfg.Target.PrismCentral == "" {
		return nil, fmt.Errorf("Nutanix host not set: add 'host' to nutanix.creds")
	}
	if cfg.Target.Username == "" {
		return nil, fmt.Errorf("Nutanix username not set: add 'username' to nutanix.creds")
	}
	if cfg.Target.Password == "" {
		return nil, fmt.Errorf("Nutanix password not set: add 'password' to nutanix.creds")
	}

	return cfg, nil
}

// loadCredsFile reads a credentials file and returns a map of key=value pairs.
// Returns nil if the file doesn't exist.
func loadCredsFile(dir, filename string) map[string]string {
	path := filepath.Join(dir, filename)
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	result := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.ToLower(key))
		value = strings.TrimSpace(value)
		value = stripQuotes(value)

		result[key] = value
	}
	return result
}

// stripQuotes removes matching surrounding single or double quotes.
func stripQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
