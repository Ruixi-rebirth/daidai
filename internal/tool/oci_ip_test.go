package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"daidai/internal/config"
	"daidai/internal/entity"
)

type ipTestTransport func(*http.Request) (*http.Response, error)

func (f ipTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func ipTestResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

type testMockHostIPManager struct {
	mu         sync.Mutex
	addedIPs   []string
	deletedIPs []string
}

func (m *testMockHostIPManager) AddIP(ctx context.Context, ip string, maskBits int, iface string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addedIPs = append(m.addedIPs, fmt.Sprintf("%s/%d@%s", ip, maskBits, iface))
	return nil
}

func (m *testMockHostIPManager) DelIP(ctx context.Context, ip string, maskBits int, iface string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedIPs = append(m.deletedIPs, fmt.Sprintf("%s/%d@%s", ip, maskBits, iface))
	return nil
}

func newIPTestTool(t *testing.T) *OCIIPTool {
	t.Helper()
	client, err := NewOCIClient(context.Background(), config.OCIConfig{
		Enabled: true, PrivateKey: generateTestRSAPEM(t), UserOCID: "user",
		TenancyOCID: "tenancy", Region: "ap-singapore-1", CompartmentID: "compartment",
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewOCIIPTool(client, entity.NewImmuneSystem("owner"))
}

func TestIPRollDefaultPreservesFirstCandidate(t *testing.T) {
	tool := newIPTestTool(t)
	calls := 0
	tool.client.httpClient.Transport = ipTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/publicIps") {
			t.Fatalf("unexpected cloud operation: %s %s", r.Method, r.URL.Path)
		}
		return ipTestResponse(200, `{"id":"candidate","ipAddress":"8.8.8.8","lifecycleState":"AVAILABLE"}`), nil
	})
	tool.qualityHTTP.Transport = ipTestTransport(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("unrestricted roll must not query third parties: %s", r.URL)
		return nil, fmt.Errorf("unexpected request")
	})
	ctx := entity.WithSensoryEvent(context.Background(), &entity.SensoryEvent{Role: entity.RolePartner, SenderID: "owner"})
	result, err := tool.Execute(ctx, json.RawMessage(`{"action":"roll"}`))
	if err != nil || result == nil {
		t.Fatalf("roll: %v", err)
	}
	if calls != 1 || !strings.Contains(result.Text, "未绑定") || strings.Contains(result.Text, "-1/100") {
		t.Fatalf("calls=%d result=%s", calls, result.Text)
	}
}

func TestIPReputationFilters(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		scoreBody                   string
		code, maxRisk               int
		proxyBody                   string
		noProxy, passed, incomplete bool
	}{
		{"zero", `{"score":"0"}`, 200, 0, "", false, true, false},
		{"high", `{"score":"70"}`, 200, 25, "", false, false, false},
		{"rate limited", "", 429, 50, "", false, false, true},
		{"blocked", "", 403, 50, "", false, false, true},
		{"missing score", "<html>changed</html>", 200, 50, "", false, false, true},
		{"proxy", "", 200, 100, `{"status":"success","proxy":true}`, true, false, false},
		{"unknown proxy", "", 200, 100, `{"status":"success"}`, true, false, true},
		{"non proxy", "", 200, 100, `{"status":"success","proxy":false}`, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := newIPTestTool(t)
			tool.qualityHTTP.Transport = ipTestTransport(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Host {
				case "ip-api.com":
					return ipTestResponse(200, tc.proxyBody), nil
				case "scamalytics.com":
					return ipTestResponse(tc.code, tc.scoreBody), nil
				default:
					t.Fatalf("unexpected probe: %s", r.URL)
					return nil, fmt.Errorf("unexpected request")
				}
			})
			rep := tool.checkIPReputation(context.Background(), "8.8.8.8", tc.maxRisk, tc.noProxy)
			if rep.Passed != tc.passed || rep.Incomplete != tc.incomplete {
				t.Fatalf("unexpected report: %+v", rep)
			}
		})
	}
}

func TestIPRollUnavailableScoreStopsAndCleansOnlyOwnCandidate(t *testing.T) {
	tool := newIPTestTool(t)
	creates, deletes := 0, 0
	deleted := false
	tool.client.httpClient.Transport = ipTestTransport(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodPost:
			creates++
			return ipTestResponse(200, `{"id":"candidate","ipAddress":"8.8.8.8","lifecycleState":"AVAILABLE"}`), nil
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/candidate"):
			if deleted {
				return ipTestResponse(404, ""), nil
			}
			return ipTestResponse(200, `{"id":"candidate","ipAddress":"8.8.8.8","lifecycleState":"AVAILABLE"}`), nil
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/candidate"):
			deletes++
			deleted = true
			return ipTestResponse(204, ""), nil
		default:
			t.Fatalf("must not touch manual or bound IP: %s %s", r.Method, r.URL.Path)
			return nil, fmt.Errorf("unexpected request")
		}
	})
	tool.qualityHTTP.Transport = ipTestTransport(func(_ *http.Request) (*http.Response, error) {
		return ipTestResponse(429, ""), nil
	})
	result, err := tool.handleRoll(context.Background(), nil, 50, false)
	if err != nil {
		t.Fatal(err)
	}
	if creates != 1 || deletes != 1 || !strings.Contains(result.Text, "HTTP 429") || !strings.Contains(result.Text, "已停止") {
		t.Fatalf("creates=%d deletes=%d result=%s", creates, deletes, result.Text)
	}
}

func TestIPCheckUnboundRunsNonDisruptiveProbe(t *testing.T) {
	tool := newIPTestTool(t)
	mockHost := &testMockHostIPManager{}
	tool.hostIP = mockHost
	tool.portLister = func(context.Context) ([]listeningPort, error) {
		return []listeningPort{{Protocol: "tcp", Address: "0.0.0.0", Port: 443}, {Protocol: "udp", Address: "0.0.0.0", Port: 443}}, nil
	}

	imdsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/vnics") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"vnicId":"test-vnic","privateIp":"10.0.0.206","macAddr":"02:00:17:03:d7:f0","subnetCidrBlock":"10.0.0.0/24"}]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer imdsServer.Close()
	origIMDS := imdsBaseURL
	imdsBaseURL = imdsServer.URL
	defer func() { imdsBaseURL = origIMDS }()

	candidateState := "AVAILABLE"
	candidatePrivateIP := ""
	secPrivateCreated := false
	secPrivateDeleted := false

	tool.client.httpClient.Transport = ipTestTransport(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/20160918/publicIps") && !strings.Contains(r.URL.Path, "/candidate-1"):
			return ipTestResponse(200, `[{"id":"candidate-1","ipAddress":"8.8.8.8","lifecycleState":"AVAILABLE","displayName":"cand"}]`), nil
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/20160918/privateIps"):
			return ipTestResponse(200, `[{"id":"primary-priv-id","isPrimary":true,"ipAddress":"10.0.0.206"}]`), nil
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/20160918/privateIps"):
			secPrivateCreated = true
			return ipTestResponse(200, `{"id":"sec-priv-id","isPrimary":false,"ipAddress":"10.0.0.250","vnicId":"test-vnic"}`), nil
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/candidate-1"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			switch body["privateIpId"] {
			case "sec-priv-id":
				candidateState = "ASSIGNED"
				candidatePrivateIP = "sec-priv-id"
			case "":
				candidateState = "AVAILABLE"
				candidatePrivateIP = ""
			default:
				t.Fatalf("unexpected privateIpId in PUT: %v", body)
			}
			return ipTestResponse(200, fmt.Sprintf(`{"id":"candidate-1","ipAddress":"8.8.8.8","lifecycleState":"%s","privateIpId":"%s"}`, candidateState, candidatePrivateIP)), nil
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/candidate-1"):
			return ipTestResponse(200, fmt.Sprintf(`{"id":"candidate-1","ipAddress":"8.8.8.8","lifecycleState":"%s","privateIpId":"%s"}`, candidateState, candidatePrivateIP)), nil
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/sec-priv-id"):
			secPrivateDeleted = true
			return ipTestResponse(204, ""), nil
		default:
			t.Fatalf("unexpected OCI call: %s %s", r.Method, r.URL.Path)
			return nil, fmt.Errorf("unexpected request")
		}
	})

	tool.qualityHTTP.Transport = ipTestTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "ip-api.com":
			return ipTestResponse(200, `{"status":"success","country":"Singapore","countryCode":"SG","city":"Singapore","as":"AS31898","proxy":false}`), nil
		case "scamalytics.com":
			return ipTestResponse(200, `{"score":"0"}`), nil
		case "api.globalping.io":
			if r.Method == http.MethodPost {
				return ipTestResponse(202, `{"id":"meas-test"}`), nil
			}
			return ipTestResponse(200, `{"status":"finished","results":[{"result":{"status":"finished","stats":{"rcv":2,"avg":180}}},{"result":{"status":"finished","stats":{"rcv":2,"avg":190}}}]}`), nil
		default:
			t.Fatalf("unexpected quality probe: %s", r.URL)
			return nil, fmt.Errorf("unexpected probe")
		}
	})

	ctx := entity.WithSensoryEvent(context.Background(), &entity.SensoryEvent{Role: entity.RolePartner, SenderID: "owner"})
	port := 443
	result, err := tool.handleCheck(ctx, "8.8.8.8", &port)
	if err != nil {
		t.Fatalf("handleCheck: %v", err)
	}
	if !secPrivateCreated || !secPrivateDeleted {
		t.Fatalf("secondary private IP lifecycle failed: created=%v, deleted=%v", secPrivateCreated, secPrivateDeleted)
	}
	if candidateState != "AVAILABLE" || candidatePrivateIP != "" {
		t.Fatalf("candidate reservation must survive check as an unbound IP: state=%s privateIP=%s", candidateState, candidatePrivateIP)
	}
	if len(mockHost.addedIPs) != 1 || !strings.HasPrefix(mockHost.addedIPs[0], "10.0.0.250/24") {
		t.Fatalf("expected host IP added, got: %v", mockHost.addedIPs)
	}
	if len(mockHost.deletedIPs) != 1 || !strings.HasPrefix(mockHost.deletedIPs[0], "10.0.0.250/24") {
		t.Fatalf("expected host IP deleted, got: %v", mockHost.deletedIPs)
	}
	if !strings.Contains(result.Text, "临时辅助私网测试") || !strings.Contains(result.Text, "主 IP 保持不变") {
		t.Fatalf("result missing non-disruptive confirmation: %s", result.Text)
	}
	if !strings.Contains(result.Text, "2/2 有效节点响应") || !strings.Contains(result.Text, "本机 UDP 绑定套接字") {
		t.Fatalf("result missing probe stats: %s", result.Text)
	}
}

func TestIPRollRejectionAndCancellation(t *testing.T) {
	for _, mode := range []string{"prefix", "score", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			tool := newIPTestTool(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			creates, deletes := 0, 0
			deleted := false
			tool.client.httpClient.Transport = ipTestTransport(func(r *http.Request) (*http.Response, error) {
				switch {
				case r.Method == http.MethodPost:
					creates++
					if mode == "cancel" {
						cancel()
					}
					deleted = false
					return ipTestResponse(200, fmt.Sprintf(`{"id":"candidate%d","ipAddress":"8.8.8.%d","lifecycleState":"AVAILABLE"}`, creates, creates)), nil
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, fmt.Sprintf("/candidate%d", creates)):
					if r.Context().Err() != nil {
						t.Fatal("cleanup must survive cancellation")
					}
					if deleted {
						return ipTestResponse(404, ""), nil
					}
					return ipTestResponse(200, fmt.Sprintf(`{"id":"candidate%d","lifecycleState":"AVAILABLE"}`, creates)), nil
				case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, fmt.Sprintf("/candidate%d", creates)):
					deletes++
					deleted = true
					return ipTestResponse(204, ""), nil
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
					return nil, fmt.Errorf("unexpected request")
				}
			})
			tool.qualityHTTP.Transport = ipTestTransport(func(r *http.Request) (*http.Response, error) {
				score := 0
				if creates == 1 {
					score = 80
				}
				return ipTestResponse(200, fmt.Sprintf(`{"score":"%d"}`, score)), nil
			})
			var prefixes []string
			maxRisk := 100
			if mode == "prefix" {
				prefixes = []string{"8.8.8.2"}
			}
			if mode == "score" {
				maxRisk = 20
			}
			result, err := tool.handleRoll(ctx, prefixes, maxRisk, false)
			if mode == "cancel" {
				if err == nil || creates != 1 || deletes != 1 {
					t.Fatalf("creates=%d deletes=%d err=%v", creates, deletes, err)
				}
			} else if err != nil || creates != 2 || deletes != 1 || !strings.Contains(result.Text, "已释放此前候选：1") {
				t.Fatalf("creates=%d deletes=%d err=%v result=%+v", creates, deletes, err, result)
			}
		})
	}
}

func TestIPTCPDiagnosticPartialAndFailed(t *testing.T) {
	for _, reachable := range []bool{false, true} {
		t.Run(fmt.Sprint(reachable), func(t *testing.T) {
			client := &http.Client{Transport: ipTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost {
					var body struct{ MeasurementOptions struct{ Port int } }
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.MeasurementOptions.Port != 8443 {
						t.Fatalf("wrong port: %+v %v", body, err)
					}
					return ipTestResponse(202, `{"id":"test"}`), nil
				}
				rcv := 0
				if reachable {
					rcv = 2
				}
				return ipTestResponse(200, fmt.Sprintf(`{"status":"finished","results":[{"result":{"status":"failed"}},{"result":{"status":"finished","stats":{"rcv":%d,"avg":20}}}]}`, rcv)), nil
			})}
			status, _, err := checkGlobalpingTCP(context.Background(), client, "8.8.8.8", 8443)
			if reachable && (err != nil || !strings.Contains(status, "1/2")) {
				t.Fatalf("%s %v", status, err)
			}
			if !reachable && (err == nil || !strings.Contains(status, "不能据此判断被墙")) {
				t.Fatalf("%s %v", status, err)
			}
		})
	}
}

func TestOCIIPTool_SyncDNS_MissingConfig(t *testing.T) {
	tool := newIPTestTool(t)
	// 1. Nil cfConfig
	res, err := tool.handleSyncDNS(context.Background(), "1.2.3.4")
	if err != nil || !strings.Contains(res.Text, "未在配置文件中配置 cloudflare 模块") {
		t.Fatalf("expected missing config notice, got %v, res=%+v", err, res)
	}

	// 2. Missing APIToken
	tool.cfConfig = &config.CloudflareConfig{SyncRecord: "vps.example.com", Zone: "example.com"}
	res, err = tool.handleSyncDNS(context.Background(), "1.2.3.4")
	if err != nil || !strings.Contains(res.Text, "api_token 为空") {
		t.Fatalf("expected empty token notice, got %v, res=%+v", err, res)
	}

	// 3. Missing SyncRecord
	tool.cfConfig = &config.CloudflareConfig{APIToken: "token", Zone: "example.com"}
	res, err = tool.handleSyncDNS(context.Background(), "1.2.3.4")
	if err != nil || !strings.Contains(res.Text, "sync_record 为空") {
		t.Fatalf("expected empty sync_record notice, got %v, res=%+v", err, res)
	}

	// 4. Missing Zone
	tool.cfConfig = &config.CloudflareConfig{APIToken: "token", SyncRecord: "vps.example.com"}
	res, err = tool.handleSyncDNS(context.Background(), "1.2.3.4")
	if err != nil || !strings.Contains(res.Text, "zone 为空") {
		t.Fatalf("expected empty zone notice, got %v, res=%+v", err, res)
	}
}
