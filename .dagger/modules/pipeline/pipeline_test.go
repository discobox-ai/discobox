package main

import (
	"errors"
	"strings"
	"testing"
)

func TestReleaseVersionPolicy(t *testing.T) {
	for _, tag := range []string{"v1.2.3", "v0.0.0-preview", "v1.2.3-rc.1"} {
		if err := validateVersion(tag); err != nil {
			t.Errorf("%s: %v", tag, err)
		}
	}
	for _, tag := range []string{"", "main", "v1.2", "1.2.3", "v1.2.3/evil", "v1.2.3; echo leak", "v1.2.3\n"} {
		if validateVersion(tag) == nil {
			t.Errorf("accepted invalid release %q", tag)
		}
	}
	if dotVersion.MatchString("v1.2.3-rc.1") {
		t.Fatal("explicit prerelease entered latest/stable channel")
	}
}
func TestImageInventory(t *testing.T) {
	images := productImages()
	if len(images) != 11 {
		t.Fatalf("%d artifacts, want base + pool + agent + platform + 7 harnesses", len(images))
	}
	seen := map[string]bool{}
	for _, image := range images {
		if seen[image.Name] {
			t.Errorf("duplicate %s", image.Name)
		}
		seen[image.Name] = true
		if strings.HasPrefix(image.Name, "harness-") && (!image.Neutral || !image.Bundle || image.Metadata == "") {
			t.Errorf("harness lost its neutral bundle contract: %+v", image)
		}
	}
}
func TestNativePlansNeverClaimVerification(t *testing.T) {
	for _, platform := range []string{"darwin/arm64", "windows/amd64"} {
		report := native(platform)
		if !strings.HasPrefix(report.Status, "external:") {
			t.Fatalf("native report claims success: %+v", report)
		}
		if len(report.Operations) == 0 {
			t.Fatal("native job has no explicit contract")
		}
	}
}
func TestLinkFlagsPreserveReleaseContracts(t *testing.T) {
	for arch, loader := range map[string]string{"amd64": "/lib64/ld-linux-x86-64.so.2", "arm64": "/lib/ld-linux-aarch64.so.1"} {
		flags := linkFlags("v1.2.3", "linux", arch)
		if !strings.Contains(flags, "-I "+loader) || !strings.Contains(flags, "Version=v1.2.3") {
			t.Fatalf("invalid flags: %s", flags)
		}
	}
	if strings.Contains(linkFlags("", "windows", "amd64"), "-I ") {
		t.Fatal("ELF interpreter passed to Windows linker")
	}
}
func TestWorkspacePackages(t *testing.T) {
	got := packages([]string{".", "./server"})
	if strings.Join(got, " ") != "./... ./server/..." {
		t.Fatal(got)
	}
}

func TestParallelReportsEveryFailure(t *testing.T) {
	first := errors.New("lint failed")
	second := errors.New("tests failed")
	err := parallel(func() error { return first }, func() error { return second }, func() error { return nil })
	if !errors.Is(err, first) || !errors.Is(err, second) {
		t.Fatalf("lost independent failure: %v", err)
	}
}

func TestPromotionStatePolicy(t *testing.T) {
	for _, tc := range []struct {
		name, version, state string
		edit, fail           bool
	}{
		{"prerelease", "v1.2.3", `{"tagName":"v1.2.3","isPrerelease":true,"isDraft":false}`, true, false},
		{"already-stable", "v1.2.3", `{"tagName":"v1.2.3","isPrerelease":false,"isDraft":false}`, false, false},
		{"draft", "v1.2.3", `{"tagName":"v1.2.3","isPrerelease":true,"isDraft":true}`, false, true},
		{"wrong-tag", "v1.2.3", `{"tagName":"v1.2.2","isPrerelease":true}`, false, true},
		{"explicit-rc", "v1.2.3-rc.1", `{"tagName":"v1.2.3-rc.1","isPrerelease":true}`, false, true},
		{"malformed", "v1.2.3", `not json`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edit, err := promotionRequired(tc.version, tc.state)
			if (err != nil) != tc.fail || edit != tc.edit {
				t.Fatalf("edit=%v err=%v", edit, err)
			}
		})
	}
}

func TestBuildArgumentsDoNotLeakIntoRuntime(t *testing.T) {
	source := "ARG BASE=debian\nFROM ${BASE}\nARG DOWNLOAD_VERSION=1\nARG RETAINED=2\nARG LEGACY=3\nENV FIRST=value RETAINED=$RETAINED\nENV LEGACY $LEGACY\n"
	got := strings.Join(buildOnlyArgs(source), ",")
	if got != "BASE,DOWNLOAD_VERSION" {
		t.Fatalf("build-only arguments: %s", got)
	}
}

func TestNativeReceiptRequiresSameSourceAndSigning(t *testing.T) {
	r := buildReceipt{Version: "v1.2.3", Revision: strings.Repeat("a", 40), Repository: "test/repo", Platform: "darwin/arm64", Status: "built", Checks: map[string]bool{}}
	if r.validate(r.Version, r.Revision, r.Repository, r.Platform) == nil {
		t.Fatal("accepted unsigned native receipt")
	}
	for _, key := range []string{"portable-inputs", "codesign-server", "codesign-cli", "virtualization-entitlement", "smoke"} {
		r.Checks[key] = true
	}
	if err := r.validate(r.Version, r.Revision, r.Repository, r.Platform); err != nil {
		t.Fatal(err)
	}
	if r.validate(r.Version, strings.Repeat("b", 40), r.Repository, r.Platform) == nil {
		t.Fatal("accepted different source revision")
	}
	if r.validate(r.Version, r.Revision, "other/repo", r.Platform) == nil {
		t.Fatal("accepted different repository")
	}
	r.Dirty = true
	if r.validate(r.Version, r.Revision, r.Repository, r.Platform) == nil {
		t.Fatal("accepted dirty native artifacts")
	}
	r.Dirty = false
	r.Status = "stubbed"
	if r.validate(r.Version, r.Revision, r.Repository, r.Platform) == nil {
		t.Fatal("accepted stubbed receipt")
	}
}
