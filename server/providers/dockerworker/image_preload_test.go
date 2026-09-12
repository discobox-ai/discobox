package dockerworker

import "context"

// preloadDriver hands the engine a Docker client onto the fake daemon, which is
// the only part of a driver preloading uses.
type preloadDriver struct {
	Driver
	url string
	// locality is what this driver says about the daemon it hands over, which
	// is what decides whether an image is loaded into it or pulled.
	locality DaemonLocality
}

func (d *preloadDriver) AcquireDockerClient(context.Context, string) (*DockerClientLease, error) {
	cli, err := testDockerClient(d.url)
	if err != nil {
		return nil, err
	}
	return NewDockerClientLease(cli, d.locality, func() { _ = cli.Close() }), nil
}
