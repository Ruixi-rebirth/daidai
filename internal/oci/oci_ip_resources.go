package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	publicIPPollInterval = 2 * time.Second
	publicIPPollTimeout  = 90 * time.Second
)

func (t *OCIIPTool) listReservedPublicIPs(ctx context.Context) ([]ociPublicIPItem, error) {
	endpoint := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps?scope=REGION&compartmentId=%s&lifetime=RESERVED",
		t.client.region, url.QueryEscape(t.client.compartmentID))
	ips, err := listOCIResources[ociPublicIPItem](ctx, t.client, endpoint)
	if err != nil {
		return nil, fmt.Errorf("读取 OCI 预留 IP 列表失败: %w", err)
	}
	return ips, nil
}

func (t *OCIIPTool) getPublicIPByPrivateIP(ctx context.Context, privateIPID string) (*ociPublicIPItem, error) {
	endpoint := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps/actions/getByPrivateIpId", t.client.region)
	resp, body, err := t.client.Do(ctx, http.MethodPost, endpoint, map[string]string{"privateIpId": privateIPID})
	if err != nil {
		return nil, fmt.Errorf("查询主网卡当前公网 IP 失败: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("查询主网卡当前公网 IP 返回 HTTP %d: %s", resp.StatusCode, summarizeOCIResponse(body))
	}
	var item ociPublicIPItem
	if err := json.Unmarshal(body, &item); err != nil {
		return nil, fmt.Errorf("解析主网卡当前公网 IP 响应失败: %w", err)
	}
	if item.ID == "" || item.IPAddress == "" {
		return nil, fmt.Errorf("主网卡当前公网 IP 响应缺少资源 ID 或地址")
	}
	return &item, nil
}

func (t *OCIIPTool) getPublicIPByID(ctx context.Context, publicIPID string) (*ociPublicIPItem, error) {
	endpoint := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps/%s", t.client.region, url.PathEscape(publicIPID))
	resp, body, err := t.client.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("查询预留 IP %s 返回 HTTP %d: %s", publicIPID, resp.StatusCode, summarizeOCIResponse(body))
	}
	var item ociPublicIPItem
	if err := json.Unmarshal(body, &item); err != nil {
		return nil, fmt.Errorf("解析预留 IP %s 响应失败: %w", publicIPID, err)
	}
	if item.ID == "" {
		return nil, fmt.Errorf("预留 IP %s 响应缺少资源 ID", publicIPID)
	}
	return &item, nil
}

func (t *OCIIPTool) getPrivateIPByID(ctx context.Context, privateIPID string) (*ociPrivateIPItem, error) {
	endpoint := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/privateIps/%s", t.client.region, url.PathEscape(privateIPID))
	resp, body, err := t.client.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("查询私网 IP %s 失败: %w", privateIPID, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("查询私网 IP %s 返回 HTTP %d: %s", privateIPID, resp.StatusCode, summarizeOCIResponse(body))
	}
	var item ociPrivateIPItem
	if err := json.Unmarshal(body, &item); err != nil {
		return nil, fmt.Errorf("解析私网 IP %s 响应失败: %w", privateIPID, err)
	}
	if item.ID == "" || item.IPAddress == "" {
		return nil, fmt.Errorf("私网 IP %s 响应缺少资源 ID 或地址", privateIPID)
	}
	return &item, nil
}

func isPublicIPAvailable(item *ociPublicIPItem) bool {
	if item == nil || item.PrivateIPID != "" {
		return false
	}
	return item.LifecycleState == "AVAILABLE" || item.LifecycleState == "UNASSIGNED"
}

func publicIPStateLabel(item *ociPublicIPItem) string {
	if item == nil || strings.TrimSpace(item.LifecycleState) == "" {
		return "未知"
	}
	return item.LifecycleState
}

// waitPublicIPLifecycle waits for an asynchronous OCI assignment transition.
// terminal is true when OCI reports a state that cannot satisfy the requested
// transition, so callers may safely decide whether a rollback is appropriate.
func (t *OCIIPTool) waitPublicIPLifecycle(ctx context.Context, publicIPID, expectedState, expectedPrivateIPID string) (terminal bool, err error) {
	pollCtx, cancel := context.WithTimeout(ctx, publicIPPollTimeout)
	defer cancel()
	ticker := time.NewTicker(publicIPPollInterval)
	defer ticker.Stop()
	// An AVAILABLE snapshot immediately after an ambiguous PUT may predate the
	// asynchronous assignment. Only treat it as a failed final state after OCI
	// has first reported ASSIGNING; otherwise keep polling and report uncertainty.
	observedAssigning := false

	for {
		item, getErr := t.getPublicIPByID(pollCtx, publicIPID)
		if getErr != nil {
			return false, fmt.Errorf("轮询预留 IP %s 状态失败: %w", publicIPID, getErr)
		}
		if item == nil {
			if expectedState == "TERMINATED" {
				return false, nil
			}
			return true, fmt.Errorf("预留 IP %s 查询结果为不存在", publicIPID)
		}
		available := expectedState == "AVAILABLE" && item.LifecycleState == "UNASSIGNED"
		if (item.LifecycleState == expectedState || available) && item.PrivateIPID == expectedPrivateIPID {
			return false, nil
		}
		if expectedState == "ASSIGNED" && item.LifecycleState == "ASSIGNING" {
			observedAssigning = true
		}
		if item.LifecycleState == "ASSIGNED" && item.PrivateIPID != expectedPrivateIPID {
			return true, fmt.Errorf("预留 IP %s 已绑定到其他私网 IP %s", publicIPID, item.PrivateIPID)
		}
		if expectedState == "ASSIGNED" && item.LifecycleState == "TERMINATED" {
			return true, fmt.Errorf("预留 IP %s 到达终态 %s，未绑定到目标私网 IP", publicIPID, item.LifecycleState)
		}
		if expectedState == "ASSIGNED" && observedAssigning && (item.LifecycleState == "AVAILABLE" || item.LifecycleState == "UNASSIGNED") {
			return true, fmt.Errorf("预留 IP %s 经 ASSIGNING 后回到 %s，未绑定到目标私网 IP", publicIPID, item.LifecycleState)
		}
		if expectedState == "AVAILABLE" && item.LifecycleState == "TERMINATED" {
			return true, fmt.Errorf("预留 IP %s 已终止，无法确认解绑状态", publicIPID)
		}

		select {
		case <-pollCtx.Done():
			return false, fmt.Errorf("等待预留 IP %s 到达 %s 超时或取消；当前状态 %s，privateIpId=%q: %w", publicIPID, expectedState, item.LifecycleState, item.PrivateIPID, pollCtx.Err())
		case <-ticker.C:
		}
	}
}

// ociPrivateIPItem is an OCI private IP resource attached to a VNIC.
type ociPrivateIPItem struct {
	ID          string `json:"id"`
	IPAddress   string `json:"ipAddress"`
	VnicID      string `json:"vnicId"`
	DisplayName string `json:"displayName"`
	IsPrimary   bool   `json:"isPrimary"`
}

func (t *OCIIPTool) createSecondaryPrivateIP(ctx context.Context, vnicID, displayName string) (*ociPrivateIPItem, error) {
	createURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/privateIps", t.client.region)
	createBody := map[string]string{
		"vnicId":      vnicID,
		"displayName": displayName,
	}
	resp, body, err := t.client.Do(ctx, http.MethodPost, createURL, createBody)
	if err != nil {
		return nil, fmt.Errorf("创建辅助私网 IP 请求失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("创建辅助私网 IP 失败 (HTTP %d): %s", resp.StatusCode, summarizeOCIResponse(body))
	}
	var item ociPrivateIPItem
	if err := json.Unmarshal(body, &item); err != nil {
		return nil, fmt.Errorf("解析辅助私网 IP 响应失败: %w", err)
	}
	if item.ID == "" || item.IPAddress == "" {
		return nil, fmt.Errorf("辅助私网 IP 响应缺少资源 ID 或 IP 地址")
	}
	return &item, nil
}

func (t *OCIIPTool) deleteSecondaryPrivateIP(ctx context.Context, privateIPID string) error {
	delURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/privateIps/%s", t.client.region, url.PathEscape(privateIPID))
	resp, body, err := t.client.Do(ctx, http.MethodDelete, delURL, nil)
	if err != nil {
		return fmt.Errorf("删除辅助私网 IP %s 失败: %w", privateIPID, err)
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	return fmt.Errorf("删除辅助私网 IP %s 返回 HTTP %d: %s", privateIPID, resp.StatusCode, summarizeOCIResponse(body))
}

func (t *OCIIPTool) cleanupOrphanedProbePrivateIPs(ctx context.Context, vnicID string, iface string, maskBits int) error {
	privURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/privateIps?vnicId=%s",
		t.client.region, url.QueryEscape(vnicID))
	resp, b, err := t.client.Do(ctx, http.MethodGet, privURL, nil)
	if err != nil {
		return fmt.Errorf("读取主网卡辅助私网 IP 失败，停止新探测: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("读取主网卡辅助私网 IP 返回 HTTP %d，停止新探测: %s", resp.StatusCode, summarizeOCIResponse(b))
	}
	var privList []ociPrivateIPItem
	if err := json.Unmarshal(b, &privList); err != nil {
		return fmt.Errorf("解析主网卡辅助私网 IP 响应失败，停止新探测: %w", err)
	}
	var cleanupErrors []error
	for _, item := range privList {
		if !item.IsPrimary && strings.HasPrefix(item.DisplayName, "probe-") {
			publicIP, err := t.getPublicIPByPrivateIP(ctx, item.ID)
			if err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("无法确认残留辅助私网 IP %s (%s) 的公网绑定状态，已保留资源: %w", item.ID, item.IPAddress, err))
				continue
			}
			if publicIP != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("残留辅助私网 IP %s (%s) 仍关联公网 IP %s (%s)，已保留映射并停止新探测", item.ID, item.IPAddress, publicIP.ID, publicIP.IPAddress))
				continue
			}
			if iface != "" && item.IPAddress != "" && t.hostIP != nil {
				if err := t.hostIP.DelIP(ctx, item.IPAddress, maskBits, iface); err != nil {
					cleanupErrors = append(cleanupErrors, fmt.Errorf("从网卡 %s 删除残留地址 %s 失败: %w", iface, item.IPAddress, err))
					continue
				}
			}
			slog.Info("清理未绑定的 probe 辅助私网 IP", "id", item.ID, "ip", item.IPAddress)
			if err := t.deleteSecondaryPrivateIP(ctx, item.ID); err != nil {
				cleanupErrors = append(cleanupErrors, err)
			}
		}
	}
	return errors.Join(cleanupErrors...)
}

func (t *OCIIPTool) bindPublicIP(ctx context.Context, publicIPID, privateIPID string) error {
	bindURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps/%s", t.client.region, url.PathEscape(publicIPID))
	resp, body, err := t.client.Do(ctx, http.MethodPut, bindURL, map[string]string{"privateIpId": privateIPID})
	if err != nil {
		return fmt.Errorf("绑定公网 IP %s 到私网 IP %s 失败: %w", publicIPID, privateIPID, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("绑定公网 IP %s 返回 HTTP %d: %s", publicIPID, resp.StatusCode, summarizeOCIResponse(body))
	}
	return nil
}

func (t *OCIIPTool) unbindPublicIP(ctx context.Context, publicIPID string) error {
	unbindURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps/%s", t.client.region, url.PathEscape(publicIPID))
	resp, body, err := t.client.Do(ctx, http.MethodPut, unbindURL, map[string]string{"privateIpId": ""})
	if err != nil {
		return fmt.Errorf("解绑预留公网 IP %s 失败: %w", publicIPID, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("解绑预留公网 IP %s 返回 HTTP %d: %s", publicIPID, resp.StatusCode, summarizeOCIResponse(body))
	}
	return nil
}
