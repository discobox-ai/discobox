package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"dagger/pipeline/internal/dagger"
)

// This integration test uses a disposable registry service. It never receives
// production credentials and never addresses GHCR.
func TestRegistryPublication(t *testing.T) {
	if os.Getenv("DISCOBOX_PIPELINE_INTEGRATION") != "1" {
		t.Skip("set DISCOBOX_PIPELINE_INTEGRATION=1 for disposable registry test")
	}
	ctx := context.Background()
	registry := dag.Container().From("registry:3").WithExposedPort(5000).AsService()
	registry, err := registry.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = registry.Stop(context.Background()) })
	address := "localhost:5000"
	opts := dagger.ContainerPublishOpts{RegistryService: registry, Protocol: dagger.RegistryProtocolHttp}
	amd := dag.Container(dagger.ContainerOpts{Platform: "linux/amd64"}).WithNewFile("/marker", "amd64")
	arm := dag.Container(dagger.ContainerOpts{Platform: "linux/arm64"}).WithNewFile("/marker", "arm64")
	opts.PlatformVariants = []*dagger.Container{arm}
	ref, err := amd.Publish(ctx, address+"/parity/product:v1.2.3", opts)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := amd.Publish(ctx, address+"/parity/product:latest", opts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Split(ref, "@")[1] != strings.Split(latest, "@")[1] {
		t.Fatal("latest digest differs from version")
	}
	client := (&Pipeline{}).Toolchain().WithServiceBinding("registry", registry)
	read := func(path string) map[string]any {
		t.Helper()
		out, err := client.WithExec([]string{"curl", "--fail-with-body", "--show-error", "--noproxy", "*", "-H", "Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json", "http://registry:5000/v2/" + path}).Stdout(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err = json.Unmarshal([]byte(out), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	index := read("parity/product/manifests/v1.2.3")
	manifests, ok := index["manifests"].([]any)
	if !ok || len(manifests) != 2 {
		t.Fatalf("not a two-platform index: %v", index)
	}
	seen := map[string]bool{}
	for _, raw := range manifests {
		platform := raw.(map[string]any)["platform"].(map[string]any)
		seen[platform["os"].(string)+"/"+platform["architecture"].(string)] = true
	}
	if !seen["linux/amd64"] || !seen["linux/arm64"] {
		t.Fatal(seen)
	}
	opts.PlatformVariants = nil
	if _, err = amd.Publish(ctx, address+"/parity/neutral:v1.2.3", opts); err != nil {
		t.Fatal(err)
	}
	neutral := read("parity/neutral/manifests/v1.2.3")
	if neutral["manifests"] != nil || neutral["config"] == nil {
		t.Fatalf("neutral bundle wrapped in index: %v", neutral)
	}
}
