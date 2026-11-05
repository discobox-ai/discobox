package endpoint

import (
	"encoding/binary"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func runtimeTestACL(mask uint16) []byte {
	data := make([]byte, 4+5*8)
	binary.LittleEndian.PutUint32(data, 2)
	for i, entry := range [][3]uint32{{1, 7, 0xffffffff}, {2, 4, 12345}, {4, 0, 0xffffffff}, {16, uint32(mask), 0xffffffff}, {32, 5, 0xffffffff}} {
		pos := 4 + i*8
		binary.LittleEndian.PutUint16(data[pos:], uint16(entry[0]))
		binary.LittleEndian.PutUint16(data[pos+2:], uint16(entry[1]))
		binary.LittleEndian.PutUint32(data[pos+4:], entry[2])
	}
	return data
}

func TestRuntimeDirectoryRepairACL(t *testing.T) {
	for _, mask := range []uint16{0, 4} {
		t.Run(string(rune('0'+mask)), func(t *testing.T) {
			_, child := repairTree(t)
			if err := unix.Setxattr(child, "system.posix_acl_access", runtimeTestACL(mask), 0); err == unix.ENOTSUP {
				t.Skip("ACLs unavailable")
			} else if err != nil {
				t.Fatal(err)
			}
			plan, err := InspectRuntimeDirectories(DefaultEndpoint())
			if mask != 0 {
				if err == nil || plan != nil {
					t.Fatal("effective named ACL grant accepted")
				}
				return
			}
			if err != nil || plan == nil {
				t.Fatalf("masked ACL: %v %v", plan, err)
			}
			defer plan.Close()
			if err := plan.Repair(); err != nil {
				t.Fatal(err)
			}
			st, _ := os.Stat(child)
			if st.Mode().Perm() != 0700 {
				t.Fatal("not private")
			}
		})
	}
}

func TestRuntimeDirectoryRepairACLChangedDuringConsent(t *testing.T) {
	_, child := repairTree(t)
	_ = os.Chmod(child, 0755)
	plan, err := InspectRuntimeDirectories(DefaultEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	if err := unix.Setxattr(child, "system.posix_acl_access", runtimeTestACL(0), 0); err == unix.ENOTSUP {
		t.Skip("ACLs unavailable")
	} else if err != nil {
		t.Fatal(err)
	}
	// Preserve the original mode while adding an effective named grant.
	if err := os.Chmod(child, 0755); err != nil {
		t.Fatal(err)
	}
	if err := plan.Repair(); err == nil {
		t.Fatal("changed ACL accepted")
	}
}
