//go:build linux

package core

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func hostResourceSummary() string {
	var lines []string
	if memory := linuxMemorySummary(); memory != "" {
		lines = append(lines, memory)
	}
	if cpu := linuxCPUPercentSummary(); cpu != "" {
		lines = append(lines, cpu)
	}
	if disk := linuxRootDiskSummary(); disk != "" {
		lines = append(lines, disk)
	}
	if uptime := linuxUptimeSummary(); uptime != "" {
		lines = append(lines, uptime)
	}
	if load := linuxLoadSummary(); load != "" {
		lines = append(lines, load)
	}
	if len(lines) == 0 {
		return ""
	}
	return "### 主机资源\n" + strings.Join(lines, "\n")
}

func linuxCPUPercentSummary() string {
	firstTotal, firstIdle, ok := linuxCPUCounters()
	if !ok {
		return ""
	}
	time.Sleep(100 * time.Millisecond)
	secondTotal, secondIdle, ok := linuxCPUCounters()
	if !ok || secondTotal <= firstTotal || secondIdle < firstIdle {
		return ""
	}
	totalDelta := secondTotal - firstTotal
	idleDelta := secondIdle - firstIdle
	if idleDelta > totalDelta {
		return ""
	}
	usage := float64(totalDelta-idleDelta) * 100 / float64(totalDelta)
	return fmt.Sprintf("- CPU 使用率（100ms 采样）：%.1f%%", usage)
}

func linuxCPUCounters() (total, idle uint64, ok bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	firstLine := strings.SplitN(string(data), "\n", 2)[0]
	fields := strings.Fields(firstLine)
	if len(fields) < 9 || fields[0] != "cpu" {
		return 0, 0, false
	}
	for i, field := range fields[1:9] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		total += value
		if i == 3 || i == 4 {
			idle += value
		}
	}
	return total, idle, true
}

func linuxMemorySummary() string {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return ""
	}
	var totalKB, availableKB uint64
	var hasTotal, hasAvailable bool
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			totalKB = value
			hasTotal = true
		case "MemAvailable":
			availableKB = value
			hasAvailable = true
		}
	}
	if !hasTotal || !hasAvailable || totalKB == 0 || availableKB > totalKB {
		return ""
	}
	usedMiB := float64(totalKB-availableKB) / 1024
	totalMiB := float64(totalKB) / 1024
	usedPercent := float64(totalKB-availableKB) * 100 / float64(totalKB)
	return fmt.Sprintf("- 主机内存：已用 %.1f / %.1f MiB（%.1f%%）", usedMiB, totalMiB, usedPercent)
}

func linuxRootDiskSummary() string {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil || stat.Blocks == 0 || stat.Bsize <= 0 {
		return ""
	}
	total := uint64(stat.Blocks) * uint64(stat.Bsize)
	free := uint64(stat.Bfree) * uint64(stat.Bsize)
	if free > total {
		return ""
	}
	used := total - free
	usedPercent := float64(used) * 100 / float64(total)
	return fmt.Sprintf("- 根分区：已用 %.2f / %.2f GiB（%.1f%%）", float64(used)/(1<<30), float64(total)/(1<<30), usedPercent)
}

func linuxUptimeSummary() string {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return ""
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || seconds < 0 {
		return ""
	}
	days := int(seconds) / 86400
	hours := (int(seconds) % 86400) / 3600
	minutes := (int(seconds) % 3600) / 60
	return fmt.Sprintf("- 系统运行时间：%d 天 %d 小时 %d 分钟", days, hours, minutes)
}

func linuxLoadSummary() string {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return ""
	}
	for _, value := range fields[:3] {
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			return ""
		}
	}
	return fmt.Sprintf("- 系统负载（1 / 5 / 15 分钟）：%s / %s / %s", fields[0], fields[1], fields[2])
}
