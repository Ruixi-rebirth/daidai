package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type vnicHostInfo struct {
	vnicID          string
	macAddr         string
	subnetCidrBlock string
	primaryPrivID   string
	primaryPrivIP   string
	ifaceName       string
	maskBits        int
}

func findInterfaceByMAC(macStr, privIP string) (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("读取本机网卡列表失败: %w", err)
	}
	if macStr != "" {
		for _, iface := range ifaces {
			if strings.EqualFold(iface.HardwareAddr.String(), macStr) {
				return iface.Name, nil
			}
		}
	}
	var addressErr error
	if privIP != "" {
		for _, iface := range ifaces {
			addrs, err := iface.Addrs()
			if err != nil {
				if addressErr == nil {
					addressErr = fmt.Errorf("读取网卡 %s 地址失败: %w", iface.Name, err)
				}
				continue
			}
			for _, addr := range addrs {
				if strings.HasPrefix(addr.String(), privIP+"/") {
					return iface.Name, nil
				}
			}
		}
	}
	if addressErr != nil {
		return "", fmt.Errorf("找不到与 OCI 网卡元数据匹配的本机网卡 (MAC %q, 私网 IP %q): %w", macStr, privIP, addressErr)
	}
	return "", fmt.Errorf("找不到与 OCI 网卡元数据匹配的本机网卡 (MAC %q, 私网 IP %q)", macStr, privIP)
}

func (t *OCIIPTool) resolveVNICInfo(ctx context.Context) (*vnicHostInfo, error) {
	var vnics []imdsVnicMeta
	if err := FetchIMDS(ctx, "vnics", &vnics); err != nil {
		return nil, fmt.Errorf("读取 OCI 实例网卡元数据失败: %w", err)
	}
	if len(vnics) == 0 || vnics[0].VnicID == "" {
		return nil, fmt.Errorf("OCI 实例元数据中没有可用的主网卡 OCID")
	}

	info := &vnicHostInfo{
		vnicID:          vnics[0].VnicID,
		macAddr:         vnics[0].MacAddr,
		subnetCidrBlock: vnics[0].SubnetCidrBlock,
		primaryPrivIP:   vnics[0].PrivateIP,
	}

	_, subnet, err := net.ParseCIDR(info.subnetCidrBlock)
	if err != nil {
		return nil, fmt.Errorf("OCI 主网卡子网 CIDR 无效 %q: %w", info.subnetCidrBlock, err)
	}
	ones, bits := subnet.Mask.Size()
	if bits != 32 || ones < 0 || ones > 32 {
		return nil, fmt.Errorf("OCI 主网卡子网不是有效的 IPv4 CIDR: %q", info.subnetCidrBlock)
	}
	info.maskBits = ones

	privURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/privateIps?vnicId=%s",
		t.client.region, url.QueryEscape(info.vnicID))
	resp, b, err := t.client.Do(ctx, http.MethodGet, privURL, nil)
	if err != nil {
		return nil, fmt.Errorf("查询主网卡私网 IP 失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("查询主网卡私网 IP 返回 HTTP %d: %s", resp.StatusCode, summarizeOCIResponse(b))
	}
	var privList []struct {
		ID        string `json:"id"`
		IsPrimary bool   `json:"isPrimary"`
		IPAddress string `json:"ipAddress"`
	}
	if err := json.Unmarshal(b, &privList); err != nil {
		return nil, fmt.Errorf("解析主网卡私网 IP 响应失败: %w", err)
	}
	primaryID := ""
	for _, p := range privList {
		if !p.IsPrimary {
			continue
		}
		if p.ID == "" {
			return nil, fmt.Errorf("主网卡私网 IP 响应将主地址标记为缺少 OCID")
		}
		if primaryID != "" {
			return nil, fmt.Errorf("主网卡私网 IP 响应包含多个主地址，拒绝猜测目标")
		}
		primaryID = p.ID
		if p.IPAddress != "" {
			info.primaryPrivIP = p.IPAddress
		}
	}
	if primaryID == "" {
		return nil, fmt.Errorf("主网卡私网 IP 响应未标出主地址，拒绝猜测目标")
	}
	info.primaryPrivID = primaryID
	primaryIP := net.ParseIP(info.primaryPrivIP).To4()
	if primaryIP == nil || !subnet.Contains(primaryIP) {
		return nil, fmt.Errorf("OCI 主网卡私网 IP %q 与子网 %q 不匹配", info.primaryPrivIP, info.subnetCidrBlock)
	}
	ifaceName, err := findInterfaceByMAC(info.macAddr, info.primaryPrivIP)
	if err != nil {
		return nil, err
	}
	info.ifaceName = ifaceName
	return info, nil
}

// ResolvePrivateIPID 解析主私网 IP OCID
func (t *OCIIPTool) ResolvePrivateIPID(ctx context.Context) (string, error) {
	info, err := t.resolveVNICInfo(ctx)
	if err != nil {
		return "", err
	}
	return info.primaryPrivID, nil
}
