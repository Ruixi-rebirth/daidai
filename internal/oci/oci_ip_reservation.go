package oci

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

func (t *OCIIPTool) cleanupReservationByName(displayName string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for attempt := 0; attempt < 5; attempt++ {
		ips, err := t.listReservedPublicIPs(ctx)
		if err != nil {
			return false, err
		}
		found := false
		for _, item := range ips {
			if item.DisplayName != displayName {
				continue
			}
			found = true
			if item.ID == "" {
				return false, fmt.Errorf("同名预留 IP 缺少资源 OCID")
			}
			if item.PrivateIPID != "" {
				return false, fmt.Errorf("同名预留 IP %s 已绑定私网地址 %s，拒绝自动删除", item.IPAddress, item.PrivateIPID)
			}
			if err := t.releaseReservedIP(ctx, item.ID); err != nil {
				return false, fmt.Errorf("释放同名预留 IP %s 失败: %w", item.IPAddress, err)
			}
		}
		if found {
			return true, nil
		}
		if attempt < 4 {
			if err := waitContext(ctx, time.Second); err != nil {
				return false, err
			}
		}
	}
	return false, nil
}

func (t *OCIIPTool) releaseReservedIP(ctx context.Context, publicIPID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := t.getPublicIPByID(ctx, publicIPID)
	if err != nil {
		return fmt.Errorf("删除前无法确认预留 IP %s 状态: %w", publicIPID, err)
	}
	if current == nil {
		return nil
	}
	if current.LifecycleState == "TERMINATED" {
		return nil
	}
	if current.LifecycleState == "TERMINATING" {
		if current.PrivateIPID != "" {
			return fmt.Errorf("拒绝等待删除仍绑定的预留 IP %s (privateIpId=%q)", publicIPID, current.PrivateIPID)
		}
		verifyCtx, cancel := context.WithTimeout(context.Background(), publicIPPollTimeout+15*time.Second)
		defer cancel()
		if _, err := t.waitPublicIPLifecycle(verifyCtx, publicIPID, "TERMINATED", ""); err != nil {
			return fmt.Errorf("预留 IP %s 已进入 TERMINATING，但未能确认删除完成: %w", publicIPID, err)
		}
		return nil
	}
	if !isPublicIPAvailable(current) {
		return fmt.Errorf("拒绝删除状态不明确或仍绑定的预留 IP %s (lifecycleState=%q, privateIpId=%q)", publicIPID, current.LifecycleState, current.PrivateIPID)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	url := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps/%s", t.client.region, publicIPID)
	resp, body, err := t.client.Do(ctx, http.MethodDelete, url, nil)
	if err != nil {
		verifyCtx, cancel := context.WithTimeout(context.Background(), publicIPPollTimeout+15*time.Second)
		defer cancel()
		if _, stateErr := t.waitPublicIPLifecycle(verifyCtx, publicIPID, "TERMINATED", ""); stateErr == nil {
			return nil
		} else {
			return fmt.Errorf("删除预留 IP %s 的请求结果不确定，最终状态也未能确认 (%v): %w", publicIPID, stateErr, err)
		}
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, summarizeOCIResponse(body))
	}
	verifyCtx, cancel := context.WithTimeout(context.Background(), publicIPPollTimeout+15*time.Second)
	defer cancel()
	if _, err := t.waitPublicIPLifecycle(verifyCtx, publicIPID, "TERMINATED", ""); err != nil {
		return fmt.Errorf("删除请求已接受，但未能确认资源进入 TERMINATED: %w", err)
	}
	return nil
}
