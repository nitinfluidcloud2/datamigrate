package nutanix

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTestConnection(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/clusters/list") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		// Check auth
		user, pass, ok := r.BasicAuth()
		if !ok || user != "admin" || pass != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"entities": []interface{}{},
		})
	}))
	defer server.Close()

	// Extract host from test server URL (remove https://)
	host := strings.TrimPrefix(server.URL, "https://")

	client := &Client{
		baseURL:    server.URL + "/api/nutanix/v3",
		username:   "admin",
		password:   "secret",
		httpClient: server.Client(),
	}

	err := client.TestConnection(context.Background())
	if err != nil {
		t.Fatalf("TestConnection: %v", err)
	}

	_ = host
}

func TestListSubnets(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/subnets/list") {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"entities": []map[string]interface{}{
					{
						"metadata": map[string]string{"uuid": "subnet-1"},
						"spec": map[string]interface{}{
							"name": "VLAN-100",
							"resources": map[string]interface{}{
								"vlan_id":     100,
								"subnet_type": "VLAN",
							},
						},
					},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := &Client{
		baseURL:    server.URL + "/api/nutanix/v3",
		username:   "admin",
		password:   "secret",
		httpClient: server.Client(),
	}

	subnets, err := client.ListSubnets(context.Background())
	if err != nil {
		t.Fatalf("ListSubnets: %v", err)
	}
	if len(subnets) != 1 {
		t.Fatalf("subnet count = %d, want 1", len(subnets))
	}
	if subnets[0].Name != "VLAN-100" {
		t.Errorf("subnet name = %q, want VLAN-100", subnets[0].Name)
	}
	if subnets[0].UUID != "subnet-1" {
		t.Errorf("subnet UUID = %q", subnets[0].UUID)
	}
}

func TestCreateImage(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/images") && r.Method == http.MethodPost {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"metadata": map[string]string{"uuid": "img-uuid-1"},
				"status": map[string]interface{}{
					"execution_context": map[string]string{},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := &Client{
		baseURL:    server.URL + "/api/nutanix/v3",
		username:   "admin",
		password:   "secret",
		httpClient: server.Client(),
	}

	uuid, err := client.CreateImage(context.Background(), "test-image", 1073741824)
	if err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	if uuid != "img-uuid-1" {
		t.Errorf("image UUID = %q", uuid)
	}
}
