//go:build darwin

package setup

import (
	"syscall"
	"unsafe"
)

func extendedACL(path string) bool {
	if TestFilesystem == "acl" {
		return true
	}
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return true
	}
	n, _, errno := syscall.Syscall6(syscall.SYS_LISTXATTR, uintptr(unsafe.Pointer(p)), 0, 0, 0, 0, 0)
	if errno == syscall.ENOTSUP {
		return false
	}
	if errno != 0 {
		return true
	}
	if n == 0 {
		return false
	}
	buf := make([]byte, n)
	n, _, errno = syscall.Syscall6(syscall.SYS_LISTXATTR, uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0, 0)
	if errno != 0 {
		return true
	}
	start := 0
	for i, b := range buf[:n] {
		if b == 0 {
			if string(buf[start:i]) == "com.apple.system.Security" {
				return true
			}
			start = i + 1
		}
	}
	return false
}
func localFilesystem(path string) error {
	if TestFilesystem == "unsupported" {
		return trust(path, "publication filesystem is not a supported local filesystem")
	}
	if TestFilesystem == "read-only" {
		return trust(path, "publication filesystem is read-only")
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return trust(path, "cannot inspect mount policy: "+err.Error())
	}
	if fs.Flags&0x1000 == 0 { // MNT_LOCAL
		return trust(path, "publication filesystem is not local")
	}
	if fs.Flags&1 != 0 { // MNT_RDONLY
		return trust(path, "publication filesystem is read-only")
	}
	return nil
}
