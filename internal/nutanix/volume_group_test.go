package nutanix

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateVolumeGroup(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		// VG creation
		case strings.HasSuffix(r.URL.Path, "/config/volume-groups") && r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]string{"extId": "task-create-1"},
			})
		// Disk add
		case strings.Contains(r.URL.Path, "/volume-groups/vg-uuid-1/disks") && r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]string{"extId": "task-disk-1"},
			})
		// Task poll - create task returns VG entity
		case strings.Contains(r.URL.Path, "/tasks/task-create-1"):
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"status":             "SUCCEEDED",
					"progressPercentage": 100,
					"entitiesAffected": []map[string]any{
						{"extId": "vg-uuid-1"},
					},
				},
			})
		// Task poll - disk add
		case strings.Contains(r.URL.Path, "/tasks/task-disk-1"):
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"status":             "SUCCEEDED",
					"progressPercentage": 100,
				},
			})
		// Get VG
		case strings.Contains(r.URL.Path, "/volume-groups/vg-uuid-1") && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"extId":      "vg-uuid-1",
					"name":       "datamigrate-test",
					"targetName": "iqn.2010-06.com.nutanix:datamigrate-test-vg-uuid-1",
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := &Client{
		baseURL:    server.URL + "/api/nutanix/v3",
		hostURL:    server.URL,
		username:   "admin",
		password:   "secret",
		httpClient: server.Client(),
	}

	disks := []VGDisk{
		{DiskSizeBytes: 102400 * 1024 * 1024, Index: 0},
		{DiskSizeBytes: 51200 * 1024 * 1024, Index: 1},
	}

	vg, err := client.CreateVolumeGroup(context.Background(), "datamigrate-test", disks, "cluster-uuid-1", "container-uuid-1")
	if err != nil {
		t.Fatalf("CreateVolumeGroup: %v", err)
	}
	if vg.UUID != "vg-uuid-1" {
		t.Errorf("VG UUID = %q", vg.UUID)
	}
	if vg.ISCSITarget != "iqn.2010-06.com.nutanix:datamigrate-test-vg-uuid-1" {
		t.Errorf("iSCSI target = %q", vg.ISCSITarget)
	}
}

func TestGetISCSIPortal(t *testing.T) {
	// Start a TCP listener on port 3260 to simulate iSCSI reachability
	listener, err := net.Listen("tcp", "127.0.0.1:3260")
	if err != nil {
		t.Skip("cannot bind port 3260 for test (may need root or port in use)")
	}
	defer listener.Close()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/volume-groups/vg-uuid-1") && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"extId":      "vg-uuid-1",
					"name":       "test-vg",
					"targetName": "iqn.2010-06.com.nutanix:test-vg",
				},
			})
		case strings.HasSuffix(r.URL.Path, "/clusters/list"):
			json.NewEncoder(w).Encode(map[string]any{
				"entities": []map[string]any{
					{
						"spec": map[string]any{
							"resources": map[string]any{
								"network": map[string]any{
									"external_data_services_ip": "127.0.0.1",
								},
							},
						},
					},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := &Client{
		baseURL:    server.URL + "/api/nutanix/v3",
		hostURL:    server.URL,
		username:   "admin",
		password:   "secret",
		httpClient: server.Client(),
	}

	portal, err := client.GetISCSIPortal(context.Background(), "vg-uuid-1")
	if err != nil {
		t.Fatalf("GetISCSIPortal: %v", err)
	}
	if portal.TargetIQN != "iqn.2010-06.com.nutanix:test-vg" {
		t.Errorf("TargetIQN = %q", portal.TargetIQN)
	}
	if portal.PortalIP != "127.0.0.1" {
		t.Errorf("PortalIP = %q", portal.PortalIP)
	}
	if portal.PortalPort != 3260 {
		t.Errorf("PortalPort = %d", portal.PortalPort)
	}
}

func TestDeleteVolumeGroup(t *testing.T) {
	deleted := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/volume-groups/vg-uuid-1") && r.Method == http.MethodDelete {
			deleted = true
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]string{"extId": "task-del-1"},
			})
			return
		}
		if strings.Contains(r.URL.Path, "/tasks/task-del-1") {
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"status":             "SUCCEEDED",
					"progressPercentage": 100,
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := &Client{
		baseURL:    server.URL + "/api/nutanix/v3",
		hostURL:    server.URL,
		username:   "admin",
		password:   "secret",
		httpClient: server.Client(),
	}

	err := client.DeleteVolumeGroup(context.Background(), "vg-uuid-1")
	if err != nil {
		t.Fatalf("DeleteVolumeGroup: %v", err)
	}
	if !deleted {
		t.Error("DELETE was not called")
	}
}
