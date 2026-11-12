//go:build linux

package llm

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

func openPTY(cols, rows uint16) (master, slave *os.File, err error) {
	mFd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx failed: %w", err)
	}

	unlock := 0
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(mFd), uintptr(unix.TIOCSPTLCK), uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		_ = unix.Close(mFd)
		return nil, nil, fmt.Errorf("ioctl TIOCSPTLCK failed: %v", errno)
	}

	var ptn int32
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(mFd), uintptr(unix.TIOCGPTN), uintptr(unsafe.Pointer(&ptn))); errno != 0 {
		_ = unix.Close(mFd)
		return nil, nil, fmt.Errorf("ioctl TIOCGPTN failed: %v", errno)
	}

	slavePath := fmt.Sprintf("/dev/pts/%d", ptn)
	sFd, err := unix.Open(slavePath, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Close(mFd)
		return nil, nil, fmt.Errorf("open slave %s failed: %w", slavePath, err)
	}

	if cols > 0 && rows > 0 {
		ws := unix.Winsize{Row: rows, Col: cols}
		_ = unix.IoctlSetWinsize(sFd, unix.TIOCSWINSZ, &ws)
	}
	_ = unix.SetNonblock(mFd, true)

	return os.NewFile(uintptr(mFd), "/dev/ptmx"), os.NewFile(uintptr(sFd), slavePath), nil
}
