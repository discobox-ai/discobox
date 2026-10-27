package main

import (
	"context"

	"dagger/pipeline/internal/dagger"
)

// Validate the image's own distro roots and the platform's actual core
// provisioning script. Test runtime HTTPS using the production materializer;
// no certificates are copied from the pipeline toolchain or the host.
func (m *Pipeline) TrustCheck(ctx context.Context) (*dagger.File, error) {
	platform, err := dag.DefaultPlatform(ctx)
	if err != nil {
		return nil, err
	}
	base, err := m.Image(ctx, "sandbox-base", string(platform))
	if err != nil {
		return nil, err
	}
	bundle, err := m.Image(ctx, "platform", string(platform))
	if err != nil {
		return nil, err
	}
	checker := m.work().WithFile("/tmp/trust-check.go", dag.CurrentModule().Source().File("testdata/trust-check/main.go")).
		WithEnvVariable("CGO_ENABLED", "0").WithExec([]string{"go", "build", "-o", "/trust-check", "/tmp/trust-check.go"})
	// Inspect the saved filesystem from a separate container: executing inside
	// the image can temporarily install the builder's CA and mask missing files.
	verify := checker.WithDirectory("/image", base.Rootfs()).WithExec([]string{"/trust-check", "-root=/image", "-bundle=/etc/ssl/certs/ca-certificates.crt"})
	if _, err = verify.Sync(ctx); err != nil {
		return nil, err
	}
	provisioned := base.WithFile("/check/prebuild-ca-store.sh", bundle.File("/features/core/prebuild-ca-store.sh")).
		WithExec([]string{"sh", "/check/prebuild-ca-store.sh"})
	exported := verify.WithDirectory("/provisioned", provisioned.Rootfs()).
		WithExec([]string{"/trust-check", "-root=/provisioned"})
	if _, err = exported.Sync(ctx); err != nil {
		return nil, err
	}
	runtime := provisioned.WithFile("/check/trust-check", checker.File("/trust-check")).
		WithExec([]string{"/check/trust-check", "-runtime"})
	report, err := runtime.Stdout(ctx)
	if err != nil {
		return nil, err
	}
	return dag.File("trust.json", report), nil
}
