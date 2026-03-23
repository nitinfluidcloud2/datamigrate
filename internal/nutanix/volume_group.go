package nutanix

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// VolumeGroup represents a Nutanix Volume Group.
type VolumeGroup struct {
	UUID        string `json:"uuid"`
	Name        string `json:"name"`
	ISCSITarget string `json:"iscsi_target_name"`
}

// VGDisk represents a disk to add to a Volume Group.
type VGDisk struct {
	DiskSizeBytes       int64  `json:"diskSizeBytes"`
	Index               int    `json:"index"`
	StorageContainerRef string `json:"storageContainerUUID,omitempty"`
}

// ISCSIPortal holds iSCSI connection details.
type ISCSIPortal struct {
	TargetIQN  string
	PortalIP   string
	PortalPort int
	VGUUID     string
	DiskIndex  int
}

// v4VGCreateBody is the request body for creating a Volume Group via v4 API.
type v4VGCreateBody struct {
	Name                             string `json:"name"`
	Description                      string `json:"description,omitempty"`
	SharingStatus                    string `json:"sharingStatus"`
	TargetPrefix                     string `json:"targetPrefix"`
	ClusterReference                 string `json:"clusterReference"`
	UsageType                        string `json:"usageType"`
	IsHidden                         bool   `json:"isHidden"`
	ShouldLoadBalanceVmAttachments   bool   `json:"shouldLoadBalanceVmAttachments"`
}

// v4DiskCreateBody is the request body for adding a disk to a VG via v4 API.
type v4DiskCreateBody struct {
	DiskSizeBytes          int64              `json:"diskSizeBytes"`
	Index                  int                `json:"index"`
	DiskDataSourceRef      *v4DataSourceRef   `json:"diskDataSourceReference"`
}

// v4DataSourceRef references a storage container for disk creation.
type v4DataSourceRef struct {
	ExtID      string `json:"extId"`
	EntityType string `json:"entityType"`
}

// v4VMAttachBody is the request body for attaching a VM to a VG via v4 API.
type v4VMAttachBody struct {
	ExtID string `json:"extId"`
}

// v4Response is a generic v4 API response.
type v4Response struct {
	Data     json.RawMessage `json:"data"`
	Metadata struct {
		Flags []struct {
			Name  string `json:"name"`
			Value bool   `json:"value"`
		} `json:"flags"`
	} `json:"metadata"`
}

// v4TaskRef is the task reference returned by v4 API.
type v4TaskRef struct {
	ExtID string `json:"extId"`
}

// v4VGData is the VG data returned by v4 GET.
type v4VGData struct {
	ExtID         string `json:"extId"`
	Name          string `json:"name"`
	TargetName    string `json:"targetName"`
	SharingStatus string `json:"sharingStatus"`
}

// doV4Request executes an HTTP request against the v4 API with NTNX-Request-Id header.
func (c *Client) doV4Request(ctx context.Context, method, path string, body interface{}) ([]byte, int, error) {
	reqID := uuid.New().String()

	url := c.hostURL + path
	respBody, status, err := c.doRequestFull(ctx, method, url, body, map[string]string{
		"NTNX-Request-Id": reqID,
	})
	return respBody, status, err
}

// waitForV4Task polls a v4 task until it completes or fails.
func (c *Client) waitForV4Task(ctx context.Context, taskExtID string) error {
	log.Info().Str("task", taskExtID).Msg("waiting for Nutanix v4 task")

	path := "/api/prism/v4.0/config/tasks/" + taskExtID

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		body, status, err := c.doV4Request(ctx, http.MethodGet, path, nil)
		if err != nil {
			return fmt.Errorf("polling task: %w", err)
		}
		if status >= 300 {
			return fmt.Errorf("task poll returned status %d: %s", status, string(body))
		}

		var resp struct {
			Data struct {
				Status             string `json:"status"`
				ProgressPercentage int    `json:"progressPercentage"`
				ErrorMessages      []struct {
					Message string `json:"message"`
				} `json:"errorMessages"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return fmt.Errorf("parsing task status: %w", err)
		}

		log.Debug().
			Str("task", taskExtID).
			Str("status", resp.Data.Status).
			Int("percent", resp.Data.ProgressPercentage).
			Msg("v4 task progress")

		switch resp.Data.Status {
		case "SUCCEEDED":
			log.Info().Str("task", taskExtID).Msg("v4 task completed")
			return nil
		case "FAILED":
			errMsg := "unknown error"
			if len(resp.Data.ErrorMessages) > 0 {
				errMsg = resp.Data.ErrorMessages[0].Message
			}
			return fmt.Errorf("task failed: %s", errMsg)
		}

		// Wait before polling again
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-after(3):
		}
	}
}

// CreateVolumeGroup creates a Volume Group on Nutanix via v4 API.
func (c *Client) CreateVolumeGroup(ctx context.Context, name string, disks []VGDisk, clusterUUID string, storageContainerUUID string) (*VolumeGroup, error) {
	log.Info().Str("name", name).Int("disks", len(disks)).Str("cluster", clusterUUID).Msg("creating volume group")

	// Step 1: Create the VG (without disks — v4 adds disks separately)
	createBody := v4VGCreateBody{
		Name:                           name,
		Description:                    "datamigrate migration volume group",
		SharingStatus:                  "SHARED",
		TargetPrefix:                   name,
		ClusterReference:               clusterUUID,
		UsageType:                      "USER",
		IsHidden:                       false,
		ShouldLoadBalanceVmAttachments: false,
	}

	body, status, err := c.doV4Request(ctx, http.MethodPost, "/api/volumes/v4.1/config/volume-groups", createBody)
	if err != nil {
		return nil, fmt.Errorf("creating volume group: %w", err)
	}
	if status >= 300 {
		return nil, fmt.Errorf("create VG failed with status %d: %s", status, string(body))
	}

	// Parse task reference
	taskExtID, err := c.parseV4TaskRef(body)
	if err != nil {
		return nil, fmt.Errorf("parsing VG create response: %w", err)
	}

	if err := c.waitForV4Task(ctx, taskExtID); err != nil {
		return nil, fmt.Errorf("waiting for VG creation: %w", err)
	}

	// Get VG UUID from task's affected entities
	vgUUID, err := c.getEntityFromTask(ctx, taskExtID)
	if err != nil {
		return nil, fmt.Errorf("getting VG UUID from task: %w", err)
	}

	// Step 2: Add disks to the VG
	for _, disk := range disks {
		containerUUID := disk.StorageContainerRef
		if containerUUID == "" {
			containerUUID = storageContainerUUID
		}
		diskBody := v4DiskCreateBody{
			DiskSizeBytes: disk.DiskSizeBytes,
			Index:         disk.Index,
			DiskDataSourceRef: &v4DataSourceRef{
				ExtID:      containerUUID,
				EntityType: "STORAGE_CONTAINER",
			},
		}

		diskPath := fmt.Sprintf("/api/volumes/v4.1/config/volume-groups/%s/disks", vgUUID)
		body, status, err := c.doV4Request(ctx, http.MethodPost, diskPath, diskBody)
		if err != nil {
			return nil, fmt.Errorf("adding disk %d: %w", disk.Index, err)
		}
		if status >= 300 {
			return nil, fmt.Errorf("add disk %d failed with status %d: %s", disk.Index, status, string(body))
		}

		diskTaskID, err := c.parseV4TaskRef(body)
		if err != nil {
			return nil, fmt.Errorf("parsing disk add response: %w", err)
		}
		if err := c.waitForV4Task(ctx, diskTaskID); err != nil {
			return nil, fmt.Errorf("waiting for disk %d add: %w", disk.Index, err)
		}

		log.Info().Int("index", disk.Index).Int64("size_bytes", disk.DiskSizeBytes).Msg("disk added to volume group")
	}

	// Step 3: Get VG details
	vg, err := c.GetVolumeGroup(ctx, vgUUID)
	if err != nil {
		return nil, err
	}

	log.Info().Str("uuid", vgUUID).Str("iscsi_target", vg.ISCSITarget).Msg("volume group created")
	return vg, nil
}

// GetVolumeGroup retrieves a Volume Group by UUID via v4 API.
func (c *Client) GetVolumeGroup(ctx context.Context, vgUUID string) (*VolumeGroup, error) {
	path := fmt.Sprintf("/api/volumes/v4.1/config/volume-groups/%s", vgUUID)
	body, status, err := c.doV4Request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("getting volume group: %w", err)
	}
	if status >= 300 {
		return nil, fmt.Errorf("get VG failed with status %d: %s", status, string(body))
	}

	var resp struct {
		Data v4VGData `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing VG response: %w", err)
	}

	iscsiTarget := resp.Data.TargetName
	// The v4 API returns just the target name (e.g., "datamigrate-rhel6-test-xxx").
	// If it's not already a full IQN, prepend the Nutanix IQN prefix.
	if iscsiTarget != "" && !strings.HasPrefix(iscsiTarget, "iqn.") {
		iscsiTarget = "iqn.2010-06.com.nutanix:" + iscsiTarget
	}

	log.Debug().
		Str("raw_target_name", resp.Data.TargetName).
		Str("iscsi_target", iscsiTarget).
		Msg("volume group iSCSI target")

	return &VolumeGroup{
		UUID:        resp.Data.ExtID,
		Name:        resp.Data.Name,
		ISCSITarget: iscsiTarget,
	}, nil
}

// ListVolumeGroups returns all Volume Groups via v4 API.
func (c *Client) ListVolumeGroups(ctx context.Context) ([]VolumeGroup, error) {
	log.Info().Msg("listing volume groups")

	body, status, err := c.doV4Request(ctx, http.MethodGet, "/api/volumes/v4.1/config/volume-groups", nil)
	if err != nil {
		return nil, fmt.Errorf("listing volume groups: %w", err)
	}
	if status >= 300 {
		return nil, fmt.Errorf("list VGs failed with status %d: %s", status, string(body))
	}

	var resp struct {
		Data []v4VGData `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing VG list response: %w", err)
	}

	var vgs []VolumeGroup
	for _, vg := range resp.Data {
		iscsiTarget := vg.TargetName
		if iscsiTarget != "" && !strings.HasPrefix(iscsiTarget, "iqn.") {
			iscsiTarget = "iqn.2010-06.com.nutanix:" + iscsiTarget
		}
		vgs = append(vgs, VolumeGroup{
			UUID:        vg.ExtID,
			Name:        vg.Name,
			ISCSITarget: iscsiTarget,
		})
	}

	log.Info().Int("count", len(vgs)).Msg("volume groups listed")
	return vgs, nil
}

// FindVolumeGroupByName finds a Volume Group by name. Returns nil if not found.
func (c *Client) FindVolumeGroupByName(ctx context.Context, name string) (*VolumeGroup, error) {
	vgs, err := c.ListVolumeGroups(ctx)
	if err != nil {
		return nil, err
	}
	for _, vg := range vgs {
		if vg.Name == name {
			return &vg, nil
		}
	}
	return nil, fmt.Errorf("volume group %q not found", name)
}

// DeleteVolumeGroup removes a Volume Group via v4 API.
func (c *Client) DeleteVolumeGroup(ctx context.Context, vgUUID string) error {
	log.Info().Str("uuid", vgUUID).Msg("deleting volume group")

	path := fmt.Sprintf("/api/volumes/v4.1/config/volume-groups/%s", vgUUID)
	body, status, err := c.doV4Request(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return fmt.Errorf("deleting volume group: %w", err)
	}
	if status >= 300 {
		return fmt.Errorf("delete VG failed with status %d: %s", status, string(body))
	}

	taskExtID, err := c.parseV4TaskRef(body)
	if err == nil && taskExtID != "" {
		if err := c.waitForV4Task(ctx, taskExtID); err != nil {
			return fmt.Errorf("waiting for VG deletion: %w", err)
		}
	}

	return nil
}

// GetISCSIPortal returns the iSCSI connection details for a Volume Group.
// It tries the cluster's data services IP first, then falls back to the
// Prism Central host IP if the data services IP is not reachable.
func (c *Client) GetISCSIPortal(ctx context.Context, vgUUID string) (*ISCSIPortal, error) {
	vg, err := c.GetVolumeGroup(ctx, vgUUID)
	if err != nil {
		return nil, err
	}

	dsIP, err := c.getDataServicesIP(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting data services IP: %w", err)
	}

	// Test if the data services IP is reachable on port 3260
	portalIP := dsIP
	if !isPortReachable(dsIP, 3260) {
		// Fall back to the Prism Central host — often the public IP that
		// exposes port 3260 when the internal data services IP is not routable.
		pcHost := c.prismCentralHost()
		log.Warn().
			Str("internal_ip", dsIP).
			Str("prism_host", pcHost).
			Msg("data services IP not reachable, falling back to Prism Central host")

		if isPortReachable(pcHost, 3260) {
			portalIP = pcHost
		} else {
			return nil, fmt.Errorf("iSCSI port 3260 not reachable on %s or %s", dsIP, pcHost)
		}
	}

	log.Info().Str("portal_ip", portalIP).Int("port", 3260).Msg("iSCSI portal selected")

	return &ISCSIPortal{
		TargetIQN:  vg.ISCSITarget,
		PortalIP:   portalIP,
		PortalPort: 3260,
		VGUUID:     vgUUID,
	}, nil
}

// getDataServicesIP retrieves the cluster's iSCSI data services IP.
func (c *Client) getDataServicesIP(ctx context.Context) (string, error) {
	reqBody := map[string]any{
		"kind":   "cluster",
		"length": 1,
	}

	body, status, err := c.doRequest(ctx, http.MethodPost, "/clusters/list", reqBody)
	if err != nil {
		return "", fmt.Errorf("listing clusters: %w", err)
	}
	if status >= 300 {
		return "", fmt.Errorf("list clusters failed with status %d", status)
	}

	var resp struct {
		Entities []struct {
			Spec struct {
				Resources struct {
					Network struct {
						ExternalDataServicesIP string `json:"external_data_services_ip"`
					} `json:"network"`
				} `json:"resources"`
			} `json:"spec"`
		} `json:"entities"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parsing cluster response: %w", err)
	}

	if len(resp.Entities) == 0 {
		return "", fmt.Errorf("no clusters found")
	}

	ip := resp.Entities[0].Spec.Resources.Network.ExternalDataServicesIP
	if ip == "" {
		return "", fmt.Errorf("cluster has no data services IP configured")
	}

	log.Info().Str("data_services_ip", ip).Msg("found cluster data services IP")
	return ip, nil
}

// AttachVGToVM attaches a Volume Group to a VM via v4 API.
func (c *Client) AttachVGToVM(ctx context.Context, vgUUID, vmUUID string) error {
	log.Info().Str("vg", vgUUID).Str("vm", vmUUID).Msg("attaching volume group to VM")

	path := fmt.Sprintf("/api/volumes/v4.1/config/volume-groups/%s/$actions/attach-vm", vgUUID)
	attachBody := v4VMAttachBody{ExtID: vmUUID}

	body, status, err := c.doV4Request(ctx, http.MethodPost, path, attachBody)
	if err != nil {
		return fmt.Errorf("attaching VG to VM: %w", err)
	}
	if status >= 300 {
		return fmt.Errorf("attach VG failed with status %d: %s", status, string(body))
	}

	taskExtID, err := c.parseV4TaskRef(body)
	if err == nil && taskExtID != "" {
		if err := c.waitForV4Task(ctx, taskExtID); err != nil {
			return fmt.Errorf("waiting for VG attach: %w", err)
		}
	}

	return nil
}

// AttachISCSIClient registers an external iSCSI initiator on the Volume Group.
// This is required before the initiator can login to the VG's iSCSI target.
func (c *Client) AttachISCSIClient(ctx context.Context, vgUUID, initiatorIQN string) error {
	log.Info().
		Str("vg", vgUUID).
		Str("initiator_iqn", initiatorIQN).
		Msg("attaching iSCSI client to volume group")

	path := fmt.Sprintf("/api/volumes/v4.1/config/volume-groups/%s/$actions/attach-iscsi-client", vgUUID)

	attachBody := map[string]any{
		"iscsiInitiatorName": initiatorIQN,
	}

	body, status, err := c.doV4Request(ctx, http.MethodPost, path, attachBody)
	if err != nil {
		return fmt.Errorf("attaching iSCSI client: %w", err)
	}
	if status >= 300 {
		// Tolerate "already attached" errors
		if strings.Contains(string(body), "ALREADY") || strings.Contains(string(body), "already") || strings.Contains(string(body), "duplicate") {
			log.Info().Msg("iSCSI initiator already attached, continuing")
			return nil
		}
		return fmt.Errorf("attach iSCSI client failed with status %d: %s", status, string(body))
	}

	taskExtID, err := c.parseV4TaskRef(body)
	if err == nil && taskExtID != "" {
		if err := c.waitForV4Task(ctx, taskExtID); err != nil {
			return fmt.Errorf("waiting for iSCSI client attach: %w", err)
		}
	}

	log.Info().
		Str("vg", vgUUID).
		Str("initiator_iqn", initiatorIQN).
		Msg("iSCSI client attached to volume group")

	return nil
}

// ListISCSIClients returns the iSCSI client UUIDs attached to a Volume Group.
func (c *Client) ListISCSIClients(ctx context.Context, vgUUID string) ([]string, error) {
	// Try v4.2 first (external-iscsi-attachments), fall back to v4.1
	path := fmt.Sprintf("/api/volumes/v4.2/config/volume-groups/%s/external-iscsi-attachments", vgUUID)
	body, status, err := c.doV4Request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("listing iSCSI clients: %w", err)
	}
	if status == 404 {
		// v4.2 not available, try v4.1
		path = fmt.Sprintf("/api/volumes/v4.1/config/volume-groups/%s/external-iscsi-attachments", vgUUID)
		body, status, err = c.doV4Request(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, fmt.Errorf("listing iSCSI clients (v4.1): %w", err)
		}
	}
	if status >= 300 {
		return nil, fmt.Errorf("list iSCSI clients failed with status %d: %s", status, string(body))
	}

	var resp struct {
		Data []struct {
			ExtID string `json:"extId"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing iSCSI clients: %w", err)
	}

	var ids []string
	for _, client := range resp.Data {
		ids = append(ids, client.ExtID)
	}
	return ids, nil
}

// DetachVGFromExternal detaches all iSCSI clients from a Volume Group via v4 API.
func (c *Client) DetachVGFromExternal(ctx context.Context, vgUUID string) error {
	log.Info().Str("vg", vgUUID).Msg("detaching external iSCSI access from volume group")

	clients, err := c.ListISCSIClients(ctx, vgUUID)
	if err != nil {
		return fmt.Errorf("listing iSCSI clients: %w", err)
	}

	if len(clients) == 0 {
		log.Info().Msg("no iSCSI clients attached to volume group")
		return nil
	}

	for _, clientID := range clients {
		path := fmt.Sprintf("/api/volumes/v4.1/config/volume-groups/%s/$actions/detach-iscsi-client", vgUUID)
		detachBody := map[string]any{
			"extId": clientID,
		}

		body, status, err := c.doV4Request(ctx, http.MethodPost, path, detachBody)
		if err != nil {
			return fmt.Errorf("detaching iSCSI client %s: %w", clientID, err)
		}
		if status >= 300 {
			return fmt.Errorf("detach iSCSI client %s failed with status %d: %s", clientID, status, string(body))
		}

		taskExtID, err := c.parseV4TaskRef(body)
		if err == nil && taskExtID != "" {
			if err := c.waitForV4Task(ctx, taskExtID); err != nil {
				return fmt.Errorf("waiting for iSCSI client %s detach: %w", clientID, err)
			}
		}

		log.Info().Str("client", clientID).Msg("iSCSI client detached")
	}

	return nil
}

// parseV4TaskRef extracts the task extId from a v4 API response.
func (c *Client) parseV4TaskRef(body []byte) (string, error) {
	var resp struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", err
	}

	// Check if data is a task reference
	var taskRef v4TaskRef
	if err := json.Unmarshal(resp.Data, &taskRef); err != nil {
		return "", err
	}

	return taskRef.ExtID, nil
}

// getEntityFromTask extracts the affected entity UUID from a v4 task.
func (c *Client) getEntityFromTask(ctx context.Context, taskExtID string) (string, error) {
	path := "/api/prism/v4.0/config/tasks/" + taskExtID
	body, status, err := c.doV4Request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	if status >= 300 {
		return "", fmt.Errorf("get task failed with status %d", status)
	}

	var resp struct {
		Data struct {
			EntitiesAffected []struct {
				ExtID string `json:"extId"`
			} `json:"entitiesAffected"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", err
	}

	if len(resp.Data.EntitiesAffected) == 0 {
		return "", fmt.Errorf("no entities in task")
	}

	return resp.Data.EntitiesAffected[0].ExtID, nil
}

// isPortReachable checks if a TCP port is reachable within 5 seconds.
func isPortReachable(host string, port int) bool {
	addr := fmt.Sprintf("%s:%d", host, port)
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		log.Debug().Str("addr", addr).Err(err).Msg("port not reachable")
		return false
	}
	conn.Close()
	return true
}

// prismCentralHost returns the hostname/IP of the Prism Central from the client's base URL.
func (c *Client) prismCentralHost() string {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	// Resolve hostname to IP for TCP connection
	addrs, err := net.LookupHost(host)
	if err != nil || len(addrs) == 0 {
		return host
	}
	return addrs[0]
}
