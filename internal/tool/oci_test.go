package tool

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"testing"

	"daidai/internal/config"
	"daidai/internal/entity"
)

func generateTestRSAPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key error: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	b := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: der,
	})
	return string(b)
}

func TestOCI_ClientAndSigning(t *testing.T) {
	pemStr := generateTestRSAPEM(t)

	cfg := config.OCIConfig{
		Enabled:       true,
		PrivateKey:    pemStr,
		UserOCID:      "ocid1.user.oc1..testuser",
		TenancyOCID:   "ocid1.tenancy.oc1..testtenancy",
		Region:        "ap-singapore-1",
		CompartmentID: "ocid1.compartment.oc1..testcomp",
	}

	client, err := NewOCIClient(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewOCIClient failed: %v", err)
	}

	if client.fingerprint == "" {
		t.Error("expected non-empty fingerprint")
	}

	// 1. 测试 GET 请求签名
	getReq, _ := http.NewRequest(http.MethodGet, "https://iaas.ap-singapore-1.oraclecloud.com/20160918/publicIps?scope=REGION", nil)
	if err := client.SignRequest(getReq, nil); err != nil {
		t.Fatalf("SignRequest GET failed: %v", err)
	}
	authGet := getReq.Header.Get("Authorization")
	if authGet == "" || !containsSubstrings(authGet, "Signature", "algorithm=\"rsa-sha256\"", "headers=\"date (request-target) host\"") {
		t.Errorf("invalid GET authorization header: %s", authGet)
	}

	// 2. 测试 POST 请求签名
	postBody := []byte(`{"test":"value"}`)
	postReq, _ := http.NewRequest(http.MethodPost, "https://usageapi.ap-singapore-1.oci.oraclecloud.com/20200107/usage", nil)
	if err := client.SignRequest(postReq, postBody); err != nil {
		t.Fatalf("SignRequest POST failed: %v", err)
	}
	authPost := postReq.Header.Get("Authorization")
	if authPost == "" || !containsSubstrings(authPost, "Signature", "x-content-sha256", "content-length") {
		t.Errorf("invalid POST authorization header: %s", authPost)
	}
}

func TestOCI_ToolDefinitionsAndPermissions(t *testing.T) {
	pemStr := generateTestRSAPEM(t)
	cfg := config.OCIConfig{
		Enabled:     true,
		PrivateKey:  pemStr,
		UserOCID:    "ocid1.user.oc1..test",
		TenancyOCID: "ocid1.tenancy.oc1..test",
		Region:      "ap-singapore-1",
	}
	client, err := NewOCIClient(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewOCIClient failed: %v", err)
	}

	immune := entity.NewImmuneSystem("partner_123")
	billTool := NewOCIBillTool(client, "Asia/Singapore")
	ipTool := NewOCIIPTool(client, immune)

	if billTool.Name() != "oci_bill" {
		t.Errorf("expected oci_bill, got %s", billTool.Name())
	}
	if ipTool.Name() != "oci_ip" {
		t.Errorf("expected oci_ip, got %s", ipTool.Name())
	}

	// 权限拦截测试：非搭档/协作者调用 cleanup 动作应被拦截
	guestCtx := entity.WithSensoryEvent(context.Background(), &entity.SensoryEvent{
		Role:     entity.RoleGuest,
		SenderID: "stranger_999",
	})
	res, err := ipTool.Execute(guestCtx, json.RawMessage(`{"action":"cleanup"}`))
	if err != nil {
		t.Fatalf("unexpected execution error: %v", err)
	}
	if !containsSubstrings(res.Text, "权限拦截") {
		t.Errorf("expected permission block text, got: %s", res.Text)
	}
}

func containsSubstrings(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || (len(s) > 0 && len(substr) > 0 && stringContains(s, substr)))
}

func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
