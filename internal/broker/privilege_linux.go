//go:build linux

package broker

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	nsGetUserNS = 0xb701
	nsGetNSType = 0xb703
)

// EnterPID1MountNamespace re-enters PID 1's mount namespace and immediately
// re-executes the pinned broker executable. It requires privileges and is
// attempted only before the privilege drop.
func EnterPID1MountNamespace() error {
	fd, err := unix.Open("/proc/1/ns/mnt", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return refuse("cannot open pid 1 mount namespace")
	}
	defer unix.Close(fd)
	executable, err := openCurrentExecutable()
	if err != nil {
		return refuse("cannot pin broker executable")
	}
	defer unix.Close(executable)
	return enterMountNamespaceAndReexec(fd, executable, -1, "cannot enter pid 1 mount namespace")
}

// AlreadyInPID1MountNamespace prevents re-exec loops without trusting a
// caller-controlled environment marker.
func AlreadyInPID1MountNamespace() bool {
	pid1, err := namespaceIdentity("/proc/1/ns/mnt")
	if err != nil {
		return false
	}
	current, err := namespaceIdentity("/proc/self/ns/mnt")
	return err == nil && sameNamespaceIdentity(&pid1, &current)
}

func namespaceIdentity(path string) (unix.Stat_t, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return unix.Stat_t{}, err
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	err = unix.Fstat(fd, &stat)
	return stat, err
}

func sameNamespaceIdentity(a, b *unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino
}

// IsMountNamespaceFD reports whether fd is an nsfs mount-namespace handle.
func IsMountNamespaceFD(fd int) bool {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(fd, &stat); err != nil || stat.Type != unix.NSFS_MAGIC {
		return false
	}
	kind, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), nsGetNSType, 0)
	return errno == 0 && int(kind) == unix.CLONE_NEWNS
}

// TryEnterNamespaceFD authenticates the inherited namespace, pins the current
// executable before entry, and immediately re-executes through that descriptor.
func TryEnterNamespaceFD(fd int, capability []byte) error {
	if !IsMountNamespaceFD(fd) {
		return refuse("inherited namespace descriptor is invalid")
	}
	if err := authenticateNamespaceUser(fd); err != nil {
		return err
	}
	executable, err := openCurrentExecutable()
	if err != nil {
		return refuse("cannot pin broker executable")
	}
	defer unix.Close(executable)
	staged, err := stageCapability(capability)
	if err != nil {
		return refuse("cannot stage capability for namespace re-entry")
	}
	defer unix.Close(staged)
	return enterMountNamespaceAndReexec(fd, executable, staged, "inherited namespace descriptor is invalid")
}

func authenticateNamespaceUser(fd int) error {
	owner, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), nsGetUserNS, 0)
	if errno != 0 {
		return refuse("cannot authenticate inherited namespace")
	}
	ownerFD := int(owner)
	defer unix.Close(ownerFD)
	initial, err := namespaceIdentity("/proc/1/ns/user")
	if err != nil {
		return refuse("cannot authenticate inherited namespace")
	}
	var ownerStat unix.Stat_t
	if err := unix.Fstat(ownerFD, &ownerStat); err != nil {
		return refuse("cannot authenticate inherited namespace")
	}
	if !sameNamespaceIdentity(&initial, &ownerStat) {
		return refuse("inherited namespace is owned by another user namespace")
	}
	return nil
}

func openCurrentExecutable() (int, error) {
	fd, err := unix.Open("/proc/self/exe", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		if err != nil {
			return -1, err
		}
		return -1, unix.EINVAL
	}
	return fd, nil
}

// StageCapabilityForReexec replaces a consumed document channel with a
// rewound anonymous descriptor, including when the original channel was a pipe.
func StageCapabilityForReexec(fd int, capability []byte) error {
	staged, err := stageCapability(capability)
	if err != nil {
		return refuse("cannot stage capability for namespace re-entry")
	}
	if staged == fd {
		if _, err := unix.FcntlInt(uintptr(staged), unix.F_SETFD, 0); err != nil {
			unix.Close(staged)
			return refuse("cannot preserve capability across namespace re-entry")
		}
		return nil
	}
	defer unix.Close(staged)
	if err := unix.Dup3(staged, fd, 0); err != nil {
		return refuse("cannot preserve capability across namespace re-entry")
	}
	return nil
}

func stageCapability(capability []byte) (int, error) {
	fd, err := unix.MemfdCreate("noise-worktree-broker-capability", unix.MFD_CLOEXEC)
	if err != nil {
		return -1, err
	}
	for len(capability) > 0 {
		n, err := unix.Write(fd, capability)
		if err != nil {
			unix.Close(fd)
			return -1, err
		}
		capability = capability[n:]
	}
	if _, err := unix.Seek(fd, 0, 0); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func enterMountNamespaceAndReexec(fd, executableFd, capabilityFd int, setnsRefusal string) error {
	runtime.LockOSThread()
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		return refuse("%s", setnsRefusal)
	}
	if err := unix.Setns(fd, unix.CLONE_NEWNS); err != nil {
		return refuse("%s", setnsRefusal)
	}
	if err := unix.Chdir("/"); err != nil {
		return refuse("cannot reset working directory after namespace re-entry")
	}
	if capabilityFd >= 0 {
		if err := unix.Dup3(capabilityFd, fd, 0); err != nil {
			return refuse("cannot preserve capability across namespace re-entry")
		}
	}
	if err := execveat(executableFd, os.Args, os.Environ()); err != nil {
		return refuse("cannot re-execute in host mount namespace")
	}
	return nil
}

func execveat(fd int, argv, env []string) error {
	argvPtr, err := syscall.SlicePtrFromStrings(argv)
	if err != nil {
		return err
	}
	envPtr, err := syscall.SlicePtrFromStrings(env)
	if err != nil {
		return err
	}
	path, err := unix.BytePtrFromString("")
	if err != nil {
		return err
	}
	var envp unsafe.Pointer
	if len(envPtr) > 0 {
		envp = unsafe.Pointer(&envPtr[0])
	}
	_, _, errno := unix.Syscall6(
		unix.SYS_EXECVEAT,
		uintptr(fd),
		uintptr(unsafe.Pointer(path)),
		uintptr(unsafe.Pointer(&argvPtr[0])),
		uintptr(envp),
		unix.AT_EMPTY_PATH,
		0,
	)
	runtime.KeepAlive(argvPtr)
	runtime.KeepAlive(envPtr)
	runtime.KeepAlive(path)
	if errno != 0 {
		return errno
	}
	return nil
}

// DropPrivilege permanently drops to uid/gid: supplementary groups are
// cleared and all three copies of each id are set, so the drop is
// irreversible. An unprivileged caller must already run as the bound
// identity, which invocation checks enforce.
func DropPrivilege(uid, gid int) error {
	if os.Geteuid() == 0 {
		if err := unix.Setgroups(nil); err != nil {
			return refuse("cannot clear supplementary groups")
		}
		if err := unix.Setresgid(gid, gid, gid); err != nil {
			return refuse("cannot drop group privilege")
		}
		if err := unix.Setresuid(uid, uid, uid); err != nil {
			return refuse("cannot drop user privilege")
		}
	}
	return verifyDroppedIdentity(uid, gid)
}

func verifyDroppedIdentity(uid, gid int) error {
	if os.Getuid() != uid || os.Getgid() != gid || os.Geteuid() != uid || os.Getegid() != gid {
		return refuse("privilege drop did not take effect")
	}
	return nil
}
