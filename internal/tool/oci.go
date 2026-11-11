package tool

import (
	"bytes"
	"context"
	"crypto"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"daidai/internal/config"
)

const (
	imdsBaseURL          = "http://169.254.169.254/opc/v2"
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
	VnicID    string `json:"vnicId"`
	PrivateIP string `json:"privateIp"`
}

// FetchIMDS 从 OCI 实例元数据服务 (IMDSv2) 获取元数据
func FetchIMDS(ctx context.Context, path string, target any) error {
	url := fmt.Sprintf("%s/%s", imdsBaseURL, strings.TrimPrefix(path, "/"))
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
				if meta.TenantID != "" {
					tenancyOCID = meta.TenantID
				} else if meta.CompartmentID != "" {
					tenancyOCID = meta.CompartmentID
				}
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

// SignRequest 对 http.Request 实施 OCI HTTP Request Signing 签名
func (c *OCIClient) SignRequest(req *http.Request, bodyBytes []byte) error {
	dateStr := time.Now().UTC().Format(http.TimeFormat)
	req.Header.Set("Date", dateStr)

	host := req.URL.Host
	if host == "" {
		host = req.Host
	}
	req.Header.Set("Host", host)

	method := strings.ToLower(req.Method)
	hasBody := method == "post" || method == "put" || method == "patch"

	var headersList []string
	var signableLines []string

	if hasBody {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Length", fmt.Sprintf("%d", len(bodyBytes)))
		shaSum := sha256.Sum256(bodyBytes)
		xSha256 := base64.StdEncoding.EncodeToString(shaSum[:])
		req.Header.Set("X-Content-Sha256", xSha256)

		headersList = []string{"date", "(request-target)", "host", "content-length", "content-type", "x-content-sha256"}
	} else {
		headersList = []string{"date", "(request-target)", "host"}
	}

	pathWithQuery := req.URL.RequestURI()
	if pathWithQuery == "" {
		pathWithQuery = req.URL.Path
		if req.URL.RawQuery != "" {
			pathWithQuery += "?" + req.URL.RawQuery
		}
	}

	for _, h := range headersList {
		switch h {
		case "(request-target)":
			signableLines = append(signableLines, fmt.Sprintf("(request-target): %s %s", method, pathWithQuery))
		case "host":
			signableLines = append(signableLines, fmt.Sprintf("host: %s", host))
		case "date":
			signableLines = append(signableLines, fmt.Sprintf("date: %s", dateStr))
		default:
			val := req.Header.Get(h)
			signableLines = append(signableLines, fmt.Sprintf("%s: %s", h, val))
		}
	}

	signableText := strings.Join(signableLines, "\n")
	hashed := sha256.Sum256([]byte(signableText))

	sigBytes, err := rsa.SignPKCS1v15(rand.Reader, c.privateKey, crypto.SHA256, hashed[:])
	if err != nil {
		return fmt.Errorf("rsa sign error: %w", err)
	}

	b64Sig := base64.StdEncoding.EncodeToString(sigBytes)
	keyID := fmt.Sprintf("%s/%s/%s", c.tenancyOCID, c.userOCID, c.fingerprint)

	authHeader := fmt.Sprintf(
		`Signature algorithm="rsa-sha256",headers="%s",keyId="%s",signature="%s",version="%s"`,
		strings.Join(headersList, " "),
		keyID,
		b64Sig,
		signatureVersion,
	)

	req.Header.Set("Authorization", authHeader)
	return nil
}

// Do 发送已签名的 OCI REST API 请求
func (c *OCIClient) Do(ctx context.Context, method, urlStr string, bodyData any) (*http.Response, []byte, error) {
	var bodyBytes []byte
	var bodyReader io.Reader

	if bodyData != nil {
		var err error
		bodyBytes, err = json.Marshal(bodyData)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal request body error: %w", err)
		}
		bodyReader = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequestWithContext(ctx, method, urlStr, bodyReader)
	if err != nil {
		return nil, nil, fmt.Errorf("new request error: %w", err)
	}

	if err := c.SignRequest(req, bodyBytes); err != nil {
		return nil, nil, fmt.Errorf("sign request error: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("http request [%s %s] failed: %w", method, urlStr, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := readLimitedBody(resp.Body, maxOCIResponseBytes)
	if err != nil {
		return resp, nil, fmt.Errorf("read response body error: %w", err)
	}

	return resp, respBytes, nil
}

func readLimitedBody(body io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("response body exceeded %d byte limit", maxBytes)
	}
	return data, nil
}

func summarizeOCIResponse(body []byte) string {
	const maxSummaryRunes = 4000
	text := strings.TrimSpace(string(body))
	runes := []rune(text)
	if len(runes) > maxSummaryRunes {
		return string(runes[:maxSummaryRunes]) + "…(已截断)"
	}
	return text
}

func listOCIResources[T any](ctx context.Context, client *OCIClient, endpoint string) ([]T, error) {
	pageURL := endpoint
	seenPages := make(map[string]struct{})
	var all []T
	var totalBytes int64
	for page := 0; page < 1000; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resp, body, err := client.Do(ctx, http.MethodGet, pageURL, nil)
		if err != nil {
			return nil, fmt.Errorf("OCI list request failed: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("OCI list request returned HTTP %d: %s", resp.StatusCode, summarizeOCIResponse(body))
		}
		totalBytes += int64(len(body))
		if totalBytes > maxOCIPaginatedBytes {
			return nil, fmt.Errorf("OCI list response exceeded %d byte aggregate limit", maxOCIPaginatedBytes)
		}
		var items []T
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, fmt.Errorf("decode OCI list response: %w", err)
		}
		all = append(all, items...)

		next := strings.TrimSpace(resp.Header.Get("opc-next-page"))
		if next == "" {
			return all, nil
		}
		if _, exists := seenPages[next]; exists {
			return nil, fmt.Errorf("OCI list response repeated a pagination token")
		}
		seenPages[next] = struct{}{}
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return nil, err
		}
		query := parsed.Query()
		query.Set("page", next)
		parsed.RawQuery = query.Encode()
		pageURL = parsed.String()
	}
	return nil, fmt.Errorf("OCI list exceeded 1000 pages")
}
