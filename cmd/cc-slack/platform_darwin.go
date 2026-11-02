package main

import (
	"bytes"
	"fmt"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func socketPeerPID(socket string) (int, error) {
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return 0, err
	}
	unix.CloseOnExec(fd)
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Connect(fd, &unix.SockaddrUnix{Name: socket}); err != nil {
		return 0, fmt.Errorf("connect %s: %w", socket, err)
	}
	return unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID)
}

func executablePath(pid int) (string, error) {
	args, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", fmt.Errorf("read pid %d arguments: %w", pid, err)
	}
	if len(args) < 4 {
		return "", fmt.Errorf("pid %d arguments: %d bytes", pid, len(args))
	}
	path, _, ok := bytes.Cut(args[4:], []byte{0})
	if !ok || !filepath.IsAbs(string(path)) {
		return "", fmt.Errorf("pid %d exec path %q is not absolute", pid, path)
	}
	return string(path), nil
}

func bornAt(info *syscall.Stat_t) time.Time {
	return time.Unix(info.Birthtimespec.Unix())
}
