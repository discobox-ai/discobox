package agentlayer

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

const (
	platformDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	harnessDigest  = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	layerDigest    = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func testPins() Pins {
	return Pins{{Name: "discobox-platform", Digest: platformDigest}, {Name: "discobox-harness-shell", Digest: harnessDigest}}
}

func testAgent(body string) Agent {
	return Agent{Digest: layerDigest, Size: int64(len(body)), Open: func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}}
}

type entry struct {
	header *tar.Header
	body   string
}

func readArchive(t *testing.T, data []byte) ([]string, map[string]entry) {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	var order []string
	entries := map[string]entry{}
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return order, entries
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, header.Name)
		entries[header.Name] = entry{header, string(body)}
	}
}

// A placement holds the agent under its layer's digest, the marker that says
// the layer is whole last of the layer's entries, the link the units run it
// through, the units and their enablement, and the pins.
func TestArchivePlacesTheAgentItsUnitsAndPins(t *testing.T) {
	var buf bytes.Buffer
	if err := Archive(&buf, testAgent("agent-binary"), false, testPins()); err != nil {
		t.Fatal(err)
	}
	order, entries := readArchive(t, buf.Bytes())
	hex := Hex(layerDigest)
	agent := entries["opt/discobox/bundles/layers/"+hex+"/"+AgentFile]
	if agent.body != "agent-binary" || agent.header.Mode != 0o755 {
		t.Fatalf("agent = %+v, want the binary, executable", agent)
	}
	marker := "opt/discobox/bundles/layers/" + hex + ".complete"
	markerAt, agentAt := -1, -1
	for i, name := range order {
		switch name {
		case marker:
			markerAt = i
		case "opt/discobox/bundles/layers/" + hex + "/" + AgentFile:
			agentAt = i
		}
	}
	if markerAt < 0 || markerAt < agentAt {
		t.Fatalf("marker at %d, agent at %d: the marker must follow the layer", markerAt, agentAt)
	}
	if link := entries["opt/discobox/current"]; link.header == nil || link.header.Linkname != "bundles/layers/"+hex {
		t.Fatalf("current = %+v, want a link to the layer", link)
	}
	pins, err := ParsePins([]byte(entries["opt/discobox/bundles/pins.json"].body))
	if err != nil || !slices.Equal(pins, testPins()) {
		t.Fatalf("pins = %+v, %v", pins, err)
	}
	wire := entries["etc/systemd/system/discobox-wire.service"]
	if !strings.Contains(wire.body, "ExecStart="+AgentPath+" wire") {
		t.Fatalf("wire unit does not run the placed agent:\n%s", wire.body)
	}
	if link := entries["etc/systemd/system/sysinit.target.requires/discobox-wire.service"]; link.header == nil || link.header.Linkname != "/etc/systemd/system/discobox-wire.service" {
		t.Fatalf("wire is not required by sysinit.target: %+v", link)
	}
	if link := entries["etc/systemd/system/paths.target.wants/discobox-sandbox-agent.path"]; link.header == nil {
		t.Fatal("the agent's path unit is not enabled")
	}
	if _, ok := entries["etc/systemd/system/multi-user.target.wants/discobox-sandbox-agent.service"]; ok {
		t.Fatal("the agent's service is enabled; its path unit starts it")
	}
}

// A box that already holds the layer is sent everything but it.
func TestArchiveLeavesOutALayerTheBoxHolds(t *testing.T) {
	var buf bytes.Buffer
	agent := testAgent("agent-binary")
	agent.Open = func() (io.ReadCloser, error) { t.Fatal("opened a layer the box holds"); return nil, nil }
	if err := Archive(&buf, agent, true, testPins()); err != nil {
		t.Fatal(err)
	}
	_, entries := readArchive(t, buf.Bytes())
	for name := range entries {
		if strings.HasPrefix(name, "opt/discobox/bundles/layers/"+Hex(layerDigest)) {
			t.Errorf("placed %s, which the box holds", name)
		}
	}
	if _, ok := entries["opt/discobox/current"]; !ok {
		t.Error("no current link")
	}
}

func TestArchiveRefusesPinsNamingNothing(t *testing.T) {
	for name, pins := range map[string]Pins{
		"no bundle":       {},
		"a bad digest":    {{Name: "discobox-platform", Digest: "sha256:nope"}},
		"a path for name": {{Name: "../x", Digest: platformDigest}},
		"an empty pin":    {{Name: "discobox-platform", Digest: platformDigest}, {}},
		"a bundle twice":  {{Name: "discobox-platform", Digest: platformDigest}, {Name: "discobox-platform", Digest: harnessDigest}},
	} {
		if err := Archive(io.Discard, testAgent("x"), false, pins); err == nil {
			t.Errorf("%s: placed", name)
		}
	}
}

func layerTar(t *testing.T, entries ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, header := range entries {
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := tw.Write(bytes.Repeat([]byte("x"), int(header.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The agent layer is the agent's alone: a layer holding anything else beside
// it, or not holding it, is not one.
func TestAgentLayerIsTheAgentAlone(t *testing.T) {
	dirs := []*tar.Header{{Typeflag: tar.TypeDir, Name: "features/"}, {Typeflag: tar.TypeDir, Name: "features/core/"}}
	agent := &tar.Header{Typeflag: tar.TypeReg, Name: AgentFile, Size: 3, Mode: 0o755}
	if err := AgentLayer(bytes.NewReader(layerTar(t, append(dirs, agent)...))); err != nil {
		t.Fatalf("the agent's layer: %v", err)
	}
	for name, layer := range map[string][]byte{
		"another file beside it": layerTar(t, append(dirs, agent, &tar.Header{Typeflag: tar.TypeReg, Name: "features/core/discobox-access", Size: 1})...),
		"no agent":               layerTar(t, append(dirs, &tar.Header{Typeflag: tar.TypeReg, Name: "features/review/discobox-review", Size: 1})...),
		"the agent as a link":    layerTar(t, append(dirs, &tar.Header{Typeflag: tar.TypeSymlink, Name: AgentFile, Linkname: "/bin/sh"})...),
	} {
		if err := AgentLayer(bytes.NewReader(layer)); !errors.Is(err, ErrNotAgentLayer) {
			t.Errorf("%s: %v, want ErrNotAgentLayer", name, err)
		}
	}
}
