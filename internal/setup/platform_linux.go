//go:build linux

package setup

import (
	"fmt"
	"syscall"
)

const (
	linuxMountReadOnly = 0x1
	linuxMountNoExec   = 0x8

	extFamilyMagic = 0x0000ef53
	xfsMagic       = 0x58465342
	btrfsMagic     = 0x9123683e
	f2fsMagic      = 0xf2f52010
	nilfsMagic     = 0x00003434
	bcachefsMagic  = 0xca451a4e
	zfsMagic       = 0x2fc12fc1
	jfsMagic       = 0x3153464a
	reiserfsMagic  = 0x52654973
	jffs2Magic     = 0x000072b6
	overlayfsMagic = 0x794c7630
	aufsMagic      = 0x61756673

	nfsMagic     = 0x00006969
	smb2Magic    = 0xfe534d42
	cephMagic    = 0x00c36400
	unknownMagic = 0xdeadbeef
)

var supportedLinuxFilesystems = map[uint64]struct{}{
	extFamilyMagic: {},
	xfsMagic:       {},
	btrfsMagic:     {},
	f2fsMagic:      {},
	nilfsMagic:     {},
	bcachefsMagic:  {},
	zfsMagic:       {},
	jfsMagic:       {},
	reiserfsMagic:  {},
	jffs2Magic:     {},
	overlayfsMagic: {},
	aufsMagic:      {},
}

func extendedACL(path string) bool {
	if TestFilesystem == "acl" {
		return true
	}
	n, err := syscall.Listxattr(path, nil)
	if err == syscall.ENOTSUP {
		return false
	}
	if err != nil {
		return true
	}
	if n == 0 {
		return false
	}
	buf := make([]byte, n)
	n, err = syscall.Listxattr(path, buf)
	if err != nil {
		return true
	}
	for _, name := range splitNUL(buf[:n]) {
		if name == "system.posix_acl_access" || name == "system.posix_acl_default" {
			return true
		}
	}
	return false
}

func splitNUL(buf []byte) []string {
	var out []string
	start := 0
	for i, b := range buf {
		if b == 0 {
			out = append(out, string(buf[start:i]))
			start = i + 1
		}
	}
	return out
}

func localFilesystem(path string) error {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return trust(path, "cannot inspect mount policy: "+err.Error())
	}
	typeID, flags := uint64(uint32(fs.Type)), uint64(fs.Flags)
	switch TestFilesystem {
	case "unsupported":
		typeID = unknownMagic
	case "read-only":
		flags |= linuxMountReadOnly
	case "noexec":
		flags |= linuxMountNoExec
	case "ext4":
		typeID = extFamilyMagic
	case "xfs":
		typeID = xfsMagic
	case "overlayfs":
		typeID = overlayfsMagic
	case "aufs":
		typeID = aufsMagic
	case "nfs":
		typeID = nfsMagic
	case "smb2":
		typeID = smb2Magic
	case "ceph":
		typeID = cephMagic
	case "unknown":
		typeID = unknownMagic
	}
	return validateLinuxMount(path, typeID, flags)
}

func validateLinuxMount(path string, typeID, flags uint64) error {
	if flags&linuxMountReadOnly != 0 {
		return trust(path, "publication filesystem is read-only")
	}
	if flags&linuxMountNoExec != 0 {
		return trust(path, "publication filesystem is mounted noexec")
	}
	if _, ok := supportedLinuxFilesystems[typeID]; !ok {
		return trust(path, fmt.Sprintf("filesystem type 0x%x is not in the supported local allowlist", typeID))
	}
	return nil
}
