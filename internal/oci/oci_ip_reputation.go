package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
)

const maxIPQualityResponseBytes = 1 << 20

type ipReputationReport struct {
	IP         string
	Passed     bool
	Reasons    []string
	Country    string
	City       string
	ASN        string
	IsProxy    bool
	ProxyKnown bool
	Incomplete bool
	FraudScore int
}

// checkIPReputation evaluates only requested reputation filters, never connectivity.
// A missing required observation stops rolling; it is not a rejected IP.
func (t *OCIIPTool) checkIPReputation(ctx context.Context, ip string, maxRisk int, noAbuser bool) *ipReputationReport {
	rep := &ipReputationReport{IP: ip, Passed: true, FraudScore: -1}
	parsedIP := net.ParseIP(strings.TrimSpace(ip))
	if parsedIP == nil || !parsedIP.IsGlobalUnicast() || parsedIP.IsPrivate() {
		rep.Passed, rep.Incomplete = false, true
		rep.Reasons = append(rep.Reasons, "目标不是有效的公网单播 IP 地址")
		return rep
	}
	rep.IP = parsedIP.String()
	if noAbuser {
		body, err := t.fetchIPObservation(ctx, "http://ip-api.com/json/"+rep.IP+"?fields=status,country,countryCode,city,as,proxy")
		var data struct {
			Status      string
			Country     string
			CountryCode string
			City        string
			AS          string
			Proxy       *bool
		}
		if err == nil {
			if json.Unmarshal(body, &data) != nil || data.Status != "success" || data.Proxy == nil {
				err = fmt.Errorf("响应缺少有效代理标记")
			}
		}
		if err != nil {
			rep.Passed, rep.Incomplete = false, true
			rep.Reasons = append(rep.Reasons, "ip-api 代理画像不可用："+err.Error())
		} else {
			rep.ProxyKnown = true
			rep.Country, rep.City, rep.ASN, rep.IsProxy = data.Country+" ("+data.CountryCode+")", data.City, data.AS, *data.Proxy
			if rep.IsProxy {
				rep.Passed = false
				rep.Reasons = append(rep.Reasons, "ip-api 标记 proxy=true，不满足排除公开代理的要求")
			}
		}
	}
	if maxRisk < 100 {
		body, err := t.fetchIPObservation(ctx, "https://scamalytics.com/ip/"+rep.IP)
		if err == nil {
			score, ok := parseScamalyticsScore(body)
			if !ok {
				err = fmt.Errorf("响应中没有有效评分")
			} else {
				rep.FraudScore = score
				if score > maxRisk {
					rep.Passed = false
					rep.Reasons = append(rep.Reasons, fmt.Sprintf("Scamalytics 欺诈分 %d > 上限 %d", score, maxRisk))
				}
			}
		}
		if err != nil {
			rep.Passed, rep.Incomplete = false, true
			rep.Reasons = append(rep.Reasons, "Scamalytics 评分不可用："+err.Error())
		}
	}
	return rep
}

func (t *OCIIPTool) fetchIPObservation(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; daidai/oci-ip)")
	resp, err := t.qualityHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return readLimitedBody(resp.Body, maxIPQualityResponseBytes)
}

func parseScamalyticsScore(body []byte) (int, bool) {
	const key = `"score"`
	for offset := 0; offset < len(body); {
		match := bytes.Index(body[offset:], []byte(key))
		if match < 0 {
			return 0, false
		}
		index := offset + match + len(key)
		for index < len(body) && isJSONSpace(body[index]) {
			index++
		}
		if index >= len(body) || body[index] != ':' {
			offset = index
			continue
		}
		index++
		for index < len(body) && isJSONSpace(body[index]) {
			index++
		}
		if index >= len(body) || body[index] != '"' {
			offset = index
			continue
		}
		index++
		digits := 0
		score := 0
		for index < len(body) && body[index] >= '0' && body[index] <= '9' {
			if score <= 100 {
				score = score*10 + int(body[index]-'0')
				if score > 100 {
					score = 101
				}
			}
			digits++
			index++
		}
		if digits > 0 && index < len(body) && body[index] == '"' && score <= 100 {
			return score, true
		}
		offset = index
	}
	return 0, false
}

func isJSONSpace(char byte) bool {
	return char == ' ' || char == '\t' || char == '\r' || char == '\n'
}
