package oci

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type listeningPort struct {
	Protocol string
	Address  string
	Port     int
}

type portLister func(context.Context) ([]listeningPort, error)

func systemListeningPorts(ctx context.Context) ([]listeningPort, error) {
	ssPath, err := exec.LookPath("ss")
	if err != nil {
		ssPath = "/run/current-system/sw/bin/ss"
	}
	cmd := exec.CommandContext(ctx, ssPath, "-H", "-n", "-l", "-t", "-u")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("读取本机 TCP/UDP 监听端口失败: %w", err)
	}

	seen := make(map[string]bool)
	ports := make([]listeningPort, 0)
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		protocol := strings.ToLower(fields[0])
		if protocol != "tcp" && protocol != "udp" {
			continue
		}
		address, portText, err := net.SplitHostPort(fields[4])
		if err != nil {
			continue
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			continue
		}
		entry := listeningPort{Protocol: protocol, Address: address, Port: port}
		key := fmt.Sprintf("%s|%s|%d", entry.Protocol, entry.Address, entry.Port)
		if seen[key] {
			continue
		}
		seen[key] = true
		ports = append(ports, entry)
	}
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].Protocol != ports[j].Protocol {
			return ports[i].Protocol < ports[j].Protocol
		}
		if ports[i].Port != ports[j].Port {
			return ports[i].Port < ports[j].Port
		}
		return ports[i].Address < ports[j].Address
	})
	return ports, nil
}

func listenerAppliesTo(entry listeningPort, privateIP string) bool {
	if entry.Address == "" || entry.Address == "*" || entry.Address == "0.0.0.0" || entry.Address == "::" {
		return true
	}
	return net.ParseIP(entry.Address).Equal(net.ParseIP(privateIP))
}

func formatListeningPorts(ports []listeningPort) string {
	if len(ports) == 0 {
		return "无"
	}
	parts := make([]string, 0, len(ports))
	for _, entry := range ports {
		address := entry.Address
		if address == "" || address == "*" || address == "0.0.0.0" || address == "::" {
			address = "所有网卡"
		}
		parts = append(parts, fmt.Sprintf("%s/%d (%s)", strings.ToUpper(entry.Protocol), entry.Port, address))
	}
	return strings.Join(parts, "、")
}

func (t *OCIIPTool) probeListeningPorts(ctx context.Context, publicIP, privateIP string, requestedPort *int) (string, error) {
	var listeners []listeningPort
	var listErr error
	if t.portLister != nil {
		listeners, listErr = t.portLister(ctx)
	} else {
		listErr = fmt.Errorf("监听端口读取器未配置")
	}

	tcpPorts := make(map[int]bool)
	listenerTCPPorts := make(map[int]bool)
	var tcpListeners, udpListeners []listeningPort
	var applicableTCP, applicableUDP []listeningPort
	for _, entry := range listeners {
		switch entry.Protocol {
		case "tcp":
			tcpListeners = append(tcpListeners, entry)
		case "udp":
			udpListeners = append(udpListeners, entry)
		}
		if privateIP != "" && listenerAppliesTo(entry, privateIP) {
			switch entry.Protocol {
			case "tcp":
				tcpPorts[entry.Port] = true
				listenerTCPPorts[entry.Port] = true
				applicableTCP = append(applicableTCP, entry)
			case "udp":
				applicableUDP = append(applicableUDP, entry)
			}
		}
	}
	if requestedPort != nil {
		tcpPorts[*requestedPort] = true
	}

	ports := make([]int, 0, len(tcpPorts))
	for port := range tcpPorts {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	results := make([]probeResult, len(ports))
	var wg sync.WaitGroup
	workers := len(ports)
	if workers > 3 {
		workers = 3
	}
	jobs := make(chan int)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				results[index] = checkGlobalpingTCPProbe(ctx, t.qualityHTTP, publicIP, ports[index])
			}
		}()
	}
	for index := range ports {
		jobs <- index
	}
	close(jobs)
	wg.Wait()

	lines := []string{}
	if listErr != nil {
		lines = append(lines, "本机监听端口：读取失败（"+listErr.Error()+"）")
	} else if privateIP == "" {
		lines = append(lines, "本机监听端口：目标 IP 不属于当前 OCI 网卡，无法与本机监听清单对应")
	} else {
		lines = append(lines, "本机 TCP 监听："+formatListeningPorts(tcpListeners))
		lines = append(lines, "本机 UDP 绑定套接字（不等于对外服务）："+formatListeningPorts(udpListeners))
		lines = append(lines, "适用于目标私网 IP 的 TCP 监听："+formatListeningPorts(applicableTCP))
		lines = append(lines, "绑定到目标私网 IP 或通配地址的 UDP 套接字："+formatListeningPorts(applicableUDP))
	}
	if len(results) == 0 {
		lines = append(lines, "国内 TCP 探测：没有适用于该地址的监听端口")
	} else {
		for index, result := range results {
			label := fmt.Sprintf("TCP/%d", ports[index])
			if requestedPort != nil && *requestedPort == ports[index] && !listenerTCPPorts[ports[index]] {
				label += "（指定端口）"
			}
			lines = append(lines, label+"："+result.Summary)
		}
	}
	if len(applicableUDP) > 0 {
		lines = append(lines, "国内 UDP 服务验证：未执行通用握手；已列出本机监听端口，需按对应 UDP 协议验证请求与响应")
	}
	return strings.Join(lines, "\n"), nil
}
