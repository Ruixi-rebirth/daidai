package tool

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"daidai/internal/core"
)

var screenshotProxySem = make(chan struct{}, 64)

// startProtectedScreenshotProxy provides Chromium a per-run SOCKS5 endpoint.
// Every CONNECT destination is resolved, checked against the SSRF denylist,
// and pinned before it is sent through WARP.
func startProtectedScreenshotProxy(ctx context.Context) (net.Listener, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			select {
			case screenshotProxySem <- struct{}{}:
				go func() {
					defer func() { <-screenshotProxySem }()
					handleProtectedScreenshotSOCKS5(ctx, conn)
				}()
			default:
				_ = conn.Close()
			}
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-acceptDone:
		}
	}()
	return listener, nil
}

func handleProtectedScreenshotSOCKS5(ctx context.Context, client net.Conn) {
	if ctx == nil {
		ctx = context.Background()
	}
	defer func() { _ = client.Close() }()
	stopCancellation := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stopCancellation()
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	var greeting [2]byte
	if _, err := io.ReadFull(client, greeting[:]); err != nil || greeting[0] != 5 || greeting[1] == 0 {
		return
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(client, methods); err != nil {
		return
	}
	noAuth := false
	for _, method := range methods {
		if method == 0 {
			noAuth = true
			break
		}
	}
	if !noAuth {
		_, _ = client.Write([]byte{5, 0xff})
		return
	}
	if _, err := client.Write([]byte{5, 0}); err != nil {
		return
	}
	var request [4]byte
	if _, err := io.ReadFull(client, request[:]); err != nil || request[0] != 5 || request[1] != 1 || request[2] != 0 {
		writeScreenshotSOCKSReply(client, 7)
		return
	}
	host, err := readScreenshotSOCKSHost(client, request[3])
	if err != nil {
		writeScreenshotSOCKSReply(client, 8)
		return
	}
	var portBytes [2]byte
	if _, err := io.ReadFull(client, portBytes[:]); err != nil {
		return
	}
	port := fmt.Sprint(int(portBytes[0])<<8 | int(portBytes[1]))
	_ = client.SetDeadline(time.Time{})
	upstream, err := core.DialProtectedExternalTCP(ctx, host, port)
	if err != nil {
		writeScreenshotSOCKSReply(client, 2)
		return
	}
	defer func() { _ = upstream.Close() }()
	if _, err := client.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	finished := make(chan struct{}, 2)
	copyStream := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if closeWriter, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = closeWriter.CloseWrite()
		}
		finished <- struct{}{}
	}
	go copyStream(upstream, client)
	go copyStream(client, upstream)
	for copied := 0; copied < 2; copied++ {
		select {
		case <-ctx.Done():
			return
		case <-finished:
		}
	}
}

func readScreenshotSOCKSHost(conn net.Conn, addressType byte) (string, error) {
	switch addressType {
	case 1:
		address := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(conn, address); err != nil {
			return "", err
		}
		return net.IP(address).String(), nil
	case 3:
		var length [1]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return "", err
		}
		address := make([]byte, int(length[0]))
		if len(address) == 0 {
			return "", fmt.Errorf("empty domain")
		}
		if _, err := io.ReadFull(conn, address); err != nil {
			return "", err
		}
		return string(address), nil
	case 4:
		address := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(conn, address); err != nil {
			return "", err
		}
		return net.IP(address).String(), nil
	default:
		return "", fmt.Errorf("unsupported address type %d", addressType)
	}
}

func writeScreenshotSOCKSReply(conn net.Conn, status byte) {
	_, _ = conn.Write([]byte{5, status, 0, 1, 0, 0, 0, 0, 0, 0})
}
