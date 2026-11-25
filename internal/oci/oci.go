package oci

import (
	"context"
	"crypto/md5"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"daidai/internal/config"
)

var imdsBaseURL = "http://169.254.169.254/opc/v2"

const (
	signatureVersion     = "1"
	defaultRegion        = "ap-singapore-1"
	maxIMDSResponseBytes = 1 << 20
	maxOCIResponseBytes  = 8 << 20
	maxOCIPaginatedBytes = 64 << 20
)

// OCIClient Oracle Cloud REST API 原生签名客户端
type OCIClient struct {
	userOCID      string
	tenancyOCID   string
	region        string
	compartmentID string
	fingerprint   string
	privateKey    *rsa.PrivateKey
	httpClient    *http.Client
}

type imdsInstanceMeta struct {
	CompartmentID string `json:"compartmentId"`
	Region        string `json:"region"`
	Canonical     string `json:"canonicalRegionName"`
	TenantID      string `json:"tenantId"`
}

type imdsVnicMeta struct {
	VnicID          string `json:"vnicId"`
	PrivateIP       string `json:"privateIp"`
	MacAddr         string `json:"macAddr"`
	SubnetCidrBlock string `json:"subnetCidrBlock"`
}

// FetchIMDS 从 OCI 实例元数据服务 (IMDSv2) 获取元数据
func FetchIMDS(ctx context.Context, path string, target any) error {
	url := fmt.Sprintf("%s/%s", strings.TrimRight(imdsBaseURL, "/"), strings.TrimPrefix(path, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer Oracle")

	client := &http.Client{
		Timeout: 3 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("IMDS returned status %d", resp.StatusCode)
	}

	if target != nil {
		data, err := readLimitedBody(resp.Body, maxIMDSResponseBytes)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, target)
	}
	return nil
}

// calcFingerprint 计算 RSA 私钥对应公钥 DER 的 MD5 指纹 (xx:xx:xx...)
func calcFingerprint(privKey *rsa.PrivateKey) (string, error) {
	if privKey == nil {
		return "", errors.New("private key is nil")
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&privKey.PublicKey)
	if err != nil {
		return "", fmt.Errorf("marshal public key error: %w", err)
	}
	h := md5.Sum(pubDER)
	parts := make([]string, len(h))
	for i, b := range h {
		parts[i] = fmt.Sprintf("%02x", b)
	}
	return strings.Join(parts, ":"), nil
}

// parseRSAPrivateKey 解析 PEM 格式的 RSA 私钥内容 (支持 PKCS#1 与 PKCS#8)
func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("failed to decode PEM block containing private key")
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	pk8, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err == nil {
		if rsaKey, ok := pk8.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
		return nil, errors.New("found PKCS#8 key but it is not an RSA private key")
	}

	return nil, fmt.Errorf("failed to parse RSA private key: %w", err)
}

func validOCIRegion(region string) bool {
	if len(region) == 0 || len(region) > 63 || region[0] == '-' || region[len(region)-1] == '-' {
		return false
	}
	for _, char := range region {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}

// NewOCIClient 从 OCIConfig 实例化客户端 (私钥与认证凭据直接从配置内容读取)
func NewOCIClient(ctx context.Context, cfg config.OCIConfig) (*OCIClient, error) {
	pemStr := strings.TrimSpace(cfg.PrivateKey)
	if pemStr == "" {
		return nil, errors.New("OCI private_key 未配置")
	}

	privKey, err := parseRSAPrivateKey([]byte(pemStr))
	if err != nil {
		return nil, fmt.Errorf("parse OCI private key error: %w", err)
	}

	fp, err := calcFingerprint(privKey)
	if err != nil {
		return nil, fmt.Errorf("calculate fingerprint error: %w", err)
	}

	userOCID := strings.TrimSpace(cfg.UserOCID)
	if userOCID == "" {
		return nil, errors.New("未指定 OCI user_ocid")
	}

	tenancyOCID := strings.TrimSpace(cfg.TenancyOCID)
	region := strings.TrimSpace(cfg.Region)
	compartmentID := strings.TrimSpace(cfg.CompartmentID)

	// 若部分元数据未填写，尝试从本机 IMDSv2 获取
	if tenancyOCID == "" || region == "" || compartmentID == "" {
		var meta imdsInstanceMeta
		imdsCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if err := FetchIMDS(imdsCtx, "instance", &meta); err == nil {
			if tenancyOCID == "" {
				tenancyOCID = strings.TrimSpace(meta.TenantID)
			}
			if compartmentID == "" && meta.CompartmentID != "" {
				compartmentID = meta.CompartmentID
			}
			if region == "" {
				if meta.Canonical != "" {
					region = meta.Canonical
				} else if meta.Region != "" {
					region = meta.Region
				}
			}
		}
		cancel()
	}

	if region == "" {
		region = defaultRegion
	}
	region = strings.ToLower(region)
	if !validOCIRegion(region) {
		return nil, fmt.Errorf("无效的 OCI region %q：只允许字母、数字和连字符", region)
	}
	if compartmentID == "" && tenancyOCID != "" {
		compartmentID = tenancyOCID
	}
	if tenancyOCID == "" {
		return nil, errors.New("未能识别 OCI tenancy_ocid")
	}

	return &OCIClient{
		userOCID:      userOCID,
		tenancyOCID:   tenancyOCID,
		region:        region,
		compartmentID: compartmentID,
		fingerprint:   fp,
		privateKey:    privKey,
		httpClient: &http.Client{
			Timeout: 45 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}
