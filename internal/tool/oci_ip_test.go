package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"daidai/internal/config"
	"daidai/internal/entity"
)

type ipTestTransport func(*http.Request) (*http.Response, error)

func (f ipTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func ipTestResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
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

func TestIPCheckUnboundSkipsProbe(t *testing.T) {
	tool := newIPTestTool(t)
	tool.client.httpClient.Transport = ipTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected mutation: %s", r.Method)
		}
		return ipTestResponse(200, `[{"id":"manual","ipAddress":"8.8.8.8","lifecycleState":"AVAILABLE"}]`), nil
	})
	tool.qualityHTTP.Transport = ipTestTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "ip-api.com":
			return ipTestResponse(200, `{"status":"success","proxy":false}`), nil
		case "scamalytics.com":
			return ipTestResponse(200, `{"score":"0"}`), nil
		default:
			t.Fatalf("must not probe unbound address: %s", r.URL)
			return nil, fmt.Errorf("unexpected probe")
		}
	})
	result, err := tool.handleCheck(context.Background(), "8.8.8.8", 443)
	if err != nil || result == nil || !strings.Contains(result.Text, "未绑定：不做") {
		t.Fatalf("%v %+v", err, result)
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
			if !reachable && (err == nil || strings.Contains(status, "被墙")) {
				t.Fatalf("%s %v", status, err)
			}
		})
	}
}
