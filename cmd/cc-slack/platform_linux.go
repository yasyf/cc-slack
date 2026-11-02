package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func socketPeerPID(socket string) (int, error) {
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Connect(fd, &unix.SockaddrUnix{Name: socket}); err != nil {
		return 0, fmt.Errorf("connect %s: %w", socket, err)
	}
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return int(cred.Pid), nil
}

func executablePath(pid int) (string, error) {
	path, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return "", fmt.Errorf("read pid %d executable: %w", pid, err)
	}
	return path, nil
}

func bornAt(info *syscall.Stat_t) time.Time {
	return time.Unix(info.Ctim.Unix())
}
