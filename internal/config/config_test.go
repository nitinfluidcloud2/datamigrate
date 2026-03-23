package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFromCredsFiles(t *testing.T) {
	dir := t.TempDir()

	vmwareCreds := `# VMware credentials
username = administrator@vsphere.local
password = "vmware-secret-123"
host = vcenter.example.com
insecure = true
`
	nutanixCreds := `# Nutanix credentials
username = admin
password = 'nutanix-secret-456'
host = prism.example.com
cluster_uuid = abc-123-def
`
	if err := os.WriteFile(filepath.Join(dir, "vmware.creds"), []byte(vmwareCreds), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nutanix.creds"), []byte(nutanixCreds), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Source.VCenter != "vcenter.example.com" {
		t.Errorf("VCenter = %q, want vcenter.example.com", cfg.Source.VCenter)
	}
	if cfg.Source.Username != "administrator@vsphere.local" {
		t.Errorf("Username = %q", cfg.Source.Username)
	}
	if cfg.Source.Password != "vmware-secret-123" {
		t.Errorf("Password = %q", cfg.Source.Password)
	}
	if !cfg.Source.Insecure {
		t.Error("Source.Insecure should be true")
	}
	if cfg.Target.PrismCentral != "prism.example.com" {
		t.Errorf("PrismCentral = %q", cfg.Target.PrismCentral)
	}
	if cfg.Target.Username != "admin" {
		t.Errorf("Target.Username = %q", cfg.Target.Username)
	}
	if cfg.Target.Password != "nutanix-secret-456" {
		t.Errorf("Target.Password = %q", cfg.Target.Password)
	}
	if cfg.Target.ClusterUUID != "abc-123-def" {
		t.Errorf("ClusterUUID = %q", cfg.Target.ClusterUUID)
	}
	if cfg.Staging.Directory != "/tmp/datamigrate" {
		t.Errorf("Directory = %q, want /tmp/datamigrate", cfg.Staging.Directory)
	}
}

func TestLoadStagingDirectoryOverride(t *testing.T) {
	dir := t.TempDir()

	vmwareCreds := "username=admin\npassword=pass\nhost=vc.test.com\n"
	nutanixCreds := "username=admin\npassword=pass\nhost=prism.test.com\ncluster_uuid=abc\nstaging_directory=/data/staging\n"

	os.WriteFile(filepath.Join(dir, "vmware.creds"), []byte(vmwareCreds), 0600)
	os.WriteFile(filepath.Join(dir, "nutanix.creds"), []byte(nutanixCreds), 0600)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Staging.Directory != "/data/staging" {
		t.Errorf("Directory = %q, want /data/staging", cfg.Staging.Directory)
	}
}

func TestLoadMissingCredsFile(t *testing.T) {
	dir := t.TempDir()

	// Only vmware.creds, no nutanix.creds
	os.WriteFile(filepath.Join(dir, "vmware.creds"), []byte("username=a\npassword=b\nhost=c\n"), 0600)

	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for missing nutanix.creds")
	}
}

func TestLoadMissingRequiredField(t *testing.T) {
	dir := t.TempDir()

	// vmware.creds missing password
	os.WriteFile(filepath.Join(dir, "vmware.creds"), []byte("username=admin\nhost=vc.test.com\n"), 0600)
	os.WriteFile(filepath.Join(dir, "nutanix.creds"), []byte("username=a\npassword=b\nhost=c\n"), 0600)

	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for missing password")
	}
}

func TestLoadEnvVarOverride(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "vmware.creds"), []byte("username=admin\npassword=old\nhost=vc.test.com\n"), 0600)
	os.WriteFile(filepath.Join(dir, "nutanix.creds"), []byte("username=admin\npassword=old\nhost=prism.test.com\n"), 0600)

	t.Setenv("DATAMIGRATE_SOURCE_PASSWORD", "env-override")

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Source.Password != "env-override" {
		t.Errorf("Password = %q, want env-override", cfg.Source.Password)
	}
}

func TestMigrationPlanSaveLoad(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "plan.yaml")

	plan := &MigrationPlan{
		Name:   "test-migration",
		VMName: "web-server-01",
		Source: SourceConfig{
			VCenter:  "vcenter.test.com",
			Username: "admin",
			Password: "secret",
		},
		Target: TargetConfig{
			PrismCentral: "prism.test.com",
			Username:     "nutanix",
			Password:     "secret2",
		},
		NetworkMap: []NetworkMapping{
			{Source: "VM Network", Target: "subnet-uuid-1"},
		},
		TargetVMSpec: TargetVMSpec{
			NumCPUs:  4,
			MemoryMB: 8192,
		},
	}

	if err := SavePlan(plan, planPath); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	loaded, err := LoadPlan(planPath)
	if err != nil {
		t.Fatalf("LoadPlan: %v", err)
	}

	if loaded.Name != "test-migration" {
		t.Errorf("Name = %q", loaded.Name)
	}
	if loaded.VMName != "web-server-01" {
		t.Errorf("VMName = %q", loaded.VMName)
	}
	if len(loaded.NetworkMap) != 1 {
		t.Errorf("NetworkMap count = %d", len(loaded.NetworkMap))
	}
	if loaded.TargetVMSpec.NumCPUs != 4 {
		t.Errorf("NumCPUs = %d", loaded.TargetVMSpec.NumCPUs)
	}
}

func TestLoadPlanWithCredsOverride(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "plan.yaml")

	plan := &MigrationPlan{
		Name:   "test",
		VMName: "vm1",
		Source: SourceConfig{
			VCenter:  "old-vc",
			Username: "old-user",
			Password: "old-pass",
		},
		Target: TargetConfig{
			PrismCentral: "old-prism",
			Username:     "old-user",
			Password:     "old-pass",
		},
	}
	if err := SavePlan(plan, planPath); err != nil {
		t.Fatal(err)
	}

	// Place creds files next to plan
	os.WriteFile(filepath.Join(dir, "vmware.creds"), []byte("username=new-user\npassword=new-pass\nhost=new-vc\n"), 0600)
	os.WriteFile(filepath.Join(dir, "nutanix.creds"), []byte("username=nx-user\npassword=nx-pass\nhost=new-prism\ncluster_uuid=uuid-1\n"), 0600)

	loaded, err := LoadPlan(planPath)
	if err != nil {
		t.Fatalf("LoadPlan: %v", err)
	}

	if loaded.Source.VCenter != "new-vc" {
		t.Errorf("VCenter = %q, want new-vc", loaded.Source.VCenter)
	}
	if loaded.Target.ClusterUUID != "uuid-1" {
		t.Errorf("ClusterUUID = %q, want uuid-1", loaded.Target.ClusterUUID)
	}
}
