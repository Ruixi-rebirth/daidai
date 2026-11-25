package oci

import (
	"context"
	"fmt"
	"net/url"
)

type ociAvailabilityDomain struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

type ociCompartmentItem struct {
	ID             string `json:"id"`
	LifecycleState string `json:"lifecycleState"`
}

const maxOCITenancyCompartments = 1000

type ociVolumeItem struct {
	ID             string `json:"id"`
	DisplayName    string `json:"displayName"`
	SizeInGBs      int64  `json:"sizeInGBs"`
	LifecycleState string `json:"lifecycleState"`
}

type ociPublicIPItem struct {
	ID             string `json:"id"`
	IPAddress      string `json:"ipAddress"`
	DisplayName    string `json:"displayName"`
	Lifetime       string `json:"lifetime"`
	PrivateIPID    string `json:"privateIpId"`
	LifecycleState string `json:"lifecycleState"`
	TimeCreated    string `json:"timeCreated"`
}

func listTenancyCompartmentIDs(ctx context.Context, client *OCIClient) ([]string, error) {
	endpoint := fmt.Sprintf("https://identity.%s.oci.oraclecloud.com/20160918/compartments?compartmentId=%s&compartmentIdInSubtree=true&accessLevel=ANY",
		client.region, url.QueryEscape(client.tenancyOCID))
	items, err := listOCIResources[ociCompartmentItem](ctx, client, endpoint)
	if err != nil {
		return nil, err
	}
	if len(items) > maxOCITenancyCompartments {
		return nil, fmt.Errorf("tenancy has more than %d compartments; refusing an incomplete resource scan", maxOCITenancyCompartments)
	}

	ids := []string{client.tenancyOCID}
	seen := map[string]struct{}{client.tenancyOCID: {}}
	for _, item := range items {
		if item.ID == "" || item.LifecycleState == "DELETED" {
			continue
		}
		if _, exists := seen[item.ID]; exists {
			continue
		}
		seen[item.ID] = struct{}{}
		ids = append(ids, item.ID)
	}
	return ids, nil
}

func inspectOCIVolumes(ctx context.Context, client *OCIClient, compartments []string, availabilityDomains []ociAvailabilityDomain) (int64, int, error) {
	var totalGB int64
	var volumeCount int
	for _, compartmentID := range compartments {
		for _, domain := range availabilityDomains {
			if err := ctx.Err(); err != nil {
				return 0, 0, err
			}
			endpoint := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/bootVolumes?availabilityDomain=%s&compartmentId=%s",
				client.region, url.QueryEscape(domain.Name), url.QueryEscape(compartmentID))
			volumes, err := listOCIResources[ociVolumeItem](ctx, client, endpoint)
			if err != nil {
				return 0, 0, fmt.Errorf("查询启动卷失败 (compartment %s, availability domain %s): %w", compartmentID, domain.Name, err)
			}
			totalGB, volumeCount = addActiveOCIVolumes(totalGB, volumeCount, volumes)
		}

		endpoint := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/volumes?compartmentId=%s", client.region, url.QueryEscape(compartmentID))
		volumes, err := listOCIResources[ociVolumeItem](ctx, client, endpoint)
		if err != nil {
			return 0, 0, fmt.Errorf("查询块存储卷失败 (compartment %s): %w", compartmentID, err)
		}
		totalGB, volumeCount = addActiveOCIVolumes(totalGB, volumeCount, volumes)
	}
	return totalGB, volumeCount, nil
}

func addActiveOCIVolumes(totalGB int64, volumeCount int, volumes []ociVolumeItem) (int64, int) {
	for _, volume := range volumes {
		if volume.LifecycleState == "TERMINATED" || volume.LifecycleState == "TERMINATING" {
			continue
		}
		totalGB += volume.SizeInGBs
		volumeCount++
	}
	return totalGB, volumeCount
}

func inspectOCIReservedIPs(ctx context.Context, client *OCIClient, compartments []string) ([]string, []string, error) {
	var unboundIPs []string
	var boundIPs []string
	for _, compartmentID := range compartments {
		endpoint := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps?scope=REGION&compartmentId=%s&lifetime=RESERVED",
			client.region, url.QueryEscape(compartmentID))
		ips, err := listOCIResources[ociPublicIPItem](ctx, client, endpoint)
		if err != nil {
			return nil, nil, fmt.Errorf("查询预留公网 IP 失败 (compartment %s)，无法完成风险巡检: %w", compartmentID, err)
		}
		for _, ip := range ips {
			if ip.PrivateIPID == "" {
				unboundIPs = append(unboundIPs, ip.IPAddress)
			} else {
				boundIPs = append(boundIPs, ip.IPAddress)
			}
		}
	}
	return unboundIPs, boundIPs, nil
}
