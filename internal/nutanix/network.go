package nutanix

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/rs/zerolog/log"
)

// Subnet represents a Nutanix subnet.
type Subnet struct {
	UUID    string `json:"uuid"`
	Name    string `json:"name"`
	VlanID  int    `json:"vlan_id"`
	Type    string `json:"type"`
}

// SubnetListResponse is the response from listing subnets.
type SubnetListResponse struct {
	Entities []struct {
		Metadata struct {
			UUID string `json:"uuid"`
		} `json:"metadata"`
		Spec struct {
			Name      string `json:"name"`
			Resources struct {
				VlanID     int    `json:"vlan_id"`
				SubnetType string `json:"subnet_type"`
			} `json:"resources"`
		} `json:"spec"`
	} `json:"entities"`
}

// ListSubnets returns all subnets from Prism Central.
func (c *Client) ListSubnets(ctx context.Context) ([]Subnet, error) {
	log.Info().Msg("listing Nutanix subnets")

	reqBody := map[string]interface{}{
		"kind":   "subnet",
		"length": 500,
	}

	body, status, err := c.doRequest(ctx, http.MethodPost, "/subnets/list", reqBody)
	if err != nil {
		return nil, fmt.Errorf("listing subnets: %w", err)
	}
	if status >= 300 {
		return nil, fmt.Errorf("list subnets failed with status %d: %s", status, string(body))
	}

	var resp SubnetListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing subnets: %w", err)
	}

	var subnets []Subnet
	for _, e := range resp.Entities {
		subnets = append(subnets, Subnet{
			UUID:   e.Metadata.UUID,
			Name:   e.Spec.Name,
			VlanID: e.Spec.Resources.VlanID,
			Type:   e.Spec.Resources.SubnetType,
		})
	}

	log.Info().Int("count", len(subnets)).Msg("subnets listed")
	return subnets, nil
}

// Container represents a Nutanix storage container.
type Container struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

// ListContainers returns all storage containers.
func (c *Client) ListContainers(ctx context.Context) ([]Container, error) {
	log.Info().Msg("listing Nutanix containers")

	reqBody := map[string]interface{}{
		"kind":   "storage_container",
		"length": 500,
	}

	body, status, err := c.doRequest(ctx, http.MethodPost, "/storage_containers/list", reqBody)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	if status >= 300 {
		return nil, fmt.Errorf("list containers failed with status %d: %s", status, string(body))
	}

	var resp struct {
		Entities []struct {
			Metadata struct {
				UUID string `json:"uuid"`
			} `json:"metadata"`
			Spec struct {
				Name string `json:"name"`
			} `json:"spec"`
		} `json:"entities"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing containers: %w", err)
	}

	var containers []Container
	for _, e := range resp.Entities {
		containers = append(containers, Container{
			UUID: e.Metadata.UUID,
			Name: e.Spec.Name,
		})
	}

	log.Info().Int("count", len(containers)).Msg("containers listed")
	return containers, nil
}
