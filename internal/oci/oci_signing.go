package oci

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"
)

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
