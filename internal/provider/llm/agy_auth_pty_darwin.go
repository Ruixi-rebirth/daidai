//go:build darwin

package llm

import (
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

func openPTY(cols, rows uint16) (master, slave *os.File, err error) {
	mFd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx failed: %w", err)
	}
	closeMaster := func() { _ = unix.Close(mFd) }

	if err := unix.IoctlSetInt(mFd, unix.TIOCPTYGRANT, 0); err != nil {
		closeMaster()
		return nil, nil, fmt.Errorf("grant PTY slave failed: %w", err)
	}
	if err := unix.IoctlSetInt(mFd, unix.TIOCPTYUNLK, 0); err != nil {
		closeMaster()
		return nil, nil, fmt.Errorf("unlock PTY slave failed: %w", err)
	}

	var slaveName [128]byte
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(mFd), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&slaveName[0]))); errno != 0 {
		closeMaster()
		return nil, nil, fmt.Errorf("read PTY slave name failed: %v", errno)
	}
	nameEnd := strings.IndexByte(string(slaveName[:]), 0)
	if nameEnd < 0 {
		closeMaster()
		return nil, nil, fmt.Errorf("PTY slave name is not terminated")
	}
	slavePath := string(slaveName[:nameEnd])
	sFd, err := unix.Open(slavePath, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		closeMaster()
		return nil, nil, fmt.Errorf("open slave %s failed: %w", slavePath, err)
	}

	if cols > 0 && rows > 0 {
		ws := unix.Winsize{Row: rows, Col: cols}
		_ = unix.IoctlSetWinsize(sFd, unix.TIOCSWINSZ, &ws)
	}
	_ = unix.SetNonblock(mFd, true)

	return os.NewFile(uintptr(mFd), "/dev/ptmx"), os.NewFile(uintptr(sFd), slavePath), nil
}
