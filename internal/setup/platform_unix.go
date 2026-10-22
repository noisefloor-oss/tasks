//go:build linux || darwin

package setup

import (
	"fmt"
	"os"
	"syscall"
)

func flock(file *os.File) error { return syscall.Flock(int(file.Fd()), syscall.LOCK_EX) }
func funlock(file *os.File)     { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN) }

func openLock(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func sameLocalFilesystem(parent, stage string) error {
	p, err := os.Stat(parent)
	if err != nil {
		return trust(parent, "cannot inspect publication filesystem: "+err.Error())
	}
	s, err := os.Stat(stage)
	if err != nil {
		return trust(stage, "cannot inspect staging filesystem: "+err.Error())
	}
	ps, pok := p.Sys().(*syscall.Stat_t)
	ss, sok := s.Sys().(*syscall.Stat_t)
	if !pok || !sok || ps.Dev != ss.Dev {
		return trust(stage, fmt.Sprintf("staged root is not on the publication filesystem at %s", parent))
	}
	return localFilesystem(parent)
}
