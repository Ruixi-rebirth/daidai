package core

import (
	"sync"
	"sync/atomic"
)

const (
	WarpSocks5Addr      = "127.0.0.1:40000"
	WarpHTTPAddr        = "127.0.0.1:40001"
	maxWarpProfileBytes = 1 << 20
)

var (
	ipGuardMu    sync.RWMutex
	sensitiveIPs = make(map[string]struct{})
	sortedIPList []string

	warpReady    atomic.Bool
	warpEgressMu sync.RWMutex
	warpEgressIP string
)
