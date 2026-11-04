package krunvm

import (
	"strings"
	"testing"
)

func TestMissingSymbolError(t *testing.T) {
	for _, tc := range []struct{ symbol, feature string }{
		{"krun_add_disk3", "block-device"},
		{"krun_set_root_disk_remount", "block-device"},
		{"krun_add_net_unixstream", "network"},
		{"krun_create_ctx", ""},
	} {
		t.Run(tc.symbol, func(t *testing.T) {
			message := missingSymbolError("/opt/lib/libkrun.so.1", tc.symbol).Error()
			for _, want := range []string{"/opt/lib/libkrun.so.1", tc.symbol, "1.19.7", "1.x"} {
				if !strings.Contains(message, want) {
					t.Errorf("message %q lacks %q", message, want)
				}
			}
			if tc.feature != "" {
				for _, want := range []string{tc.feature + " support", "BLK=1 NET=1", "libkrun.override { withBlk = true; withNet = true; }"} {
					if !strings.Contains(message, want) {
						t.Errorf("message %q lacks %q", message, want)
					}
				}
			}
			for _, unwanted := range []string{"too old", "nix develop"} {
				if strings.Contains(message, unwanted) {
					t.Errorf("message %q contains %q", message, unwanted)
				}
			}
		})
	}
}
