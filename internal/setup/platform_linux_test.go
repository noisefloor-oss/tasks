//go:build linux

package setup

import (
	"strings"
	"testing"
)

func TestLinuxMountPolicyAllowsOnlySupportedLocalExecutableFilesystems(t *testing.T) {
	accepted := []uint64{
		extFamilyMagic, xfsMagic, btrfsMagic, f2fsMagic, nilfsMagic,
		bcachefsMagic, zfsMagic, jfsMagic, reiserfsMagic, jffs2Magic,
		overlayfsMagic, aufsMagic,
	}
	for _, typeID := range accepted {
		if err := validateLinuxMount("/install", typeID, 0); err != nil {
			t.Errorf("supported type 0x%x refused: %v", typeID, err)
		}
	}

	refused := []uint64{nfsMagic, smb2Magic, cephMagic, unknownMagic}
	for _, typeID := range refused {
		err := validateLinuxMount("/install", typeID, 0)
		if err == nil || !strings.Contains(err.Error(), "/install") || !strings.Contains(err.Error(), "supported local allowlist") {
			t.Errorf("unsupported type 0x%x error = %v", typeID, err)
		}
	}
}

func TestLinuxMountPolicyRequiresWritableExecutableMount(t *testing.T) {
	tests := []struct {
		name  string
		flags uint64
		want  string
	}{
		{"read-only", linuxMountReadOnly, "read-only"},
		{"noexec", linuxMountNoExec, "noexec"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateLinuxMount("/install", extFamilyMagic, tt.flags)
			if err == nil || !strings.Contains(err.Error(), "/install") || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
