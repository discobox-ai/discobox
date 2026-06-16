# Docker warm-pool provider

The `docker` provider is a local/test VM provider that launches Docker
containers through the generic provider VM abstraction. It is
intended to exercise the same warm worker pool control-plane path as real VM
providers without requiring cloud infrastructure.

## Provider type

```text
docker
```

## Behavior

- Creates one Docker container per warm worker.
- Passes VM boot metadata as environment variables using `vm.BuildBootConfig`.
- Can run a systemd-capable image with `/sbin/init` as PID 1.
- Exposes the worker agent port through a localhost Docker port binding.
- Reports container lifecycle state through the VM `Instance` abstraction.
- Is treated as a warm-pool provider by the service layer, so sandbox start
  claims a ready/schedulable worker instead of creating one container per
  sandbox.

## Worker agent image

`worker-agent/Dockerfile` builds a systemd-capable Docker image with the real
`discobox-worker-agent` binary installed as a systemd service. The agent reads the
VM boot metadata from environment variables, registers with the control plane,
marks the worker ready/schedulable, and serves health metadata on the configured
agent port.

Build it locally with:

```bash
task build:worker-agent-image
# or
docker build -f worker-agent/Dockerfile -t discobox-worker-agent:local .
```

## Example provider config

For the systemd worker-agent image:

```json
{
  "controlPlaneUrl": "http://host.docker.internal:8080",
  "image": "discobox-worker-agent:local",
  "poolSize": 1,
  "systemd": true,
  "privileged": true,
  "cgroupNsMode": "host"
}
```

For a simpler non-systemd local test image:

```json
{
  "controlPlaneUrl": "http://host.docker.internal:8080",
  "image": "alpine:3.20",
  "poolSize": 1,
  "systemd": false,
  "command": ["sleep", "300"]
}
```

## Running Docker provider integration tests

The integration tests are skipped by default. When enabled, they build local test
images from Dockerfiles under `providers/sandbox/vm/docker/testdata` instead of
pulling a project-published image.

To run the Docker lifecycle tests:

```bash
cd providers && DISCOBOX_DOCKER_INTEGRATION=1 go test ./sandbox/vm/docker -run Integration -count=1 -v
```

The systemd test builds `testdata/systemd/Dockerfile` automatically. It requires
a Docker environment that allows privileged containers and cgroup mounts. You can
override the image by setting `DISCOBOX_DOCKER_SYSTEMD_IMAGE` if you want to use
a prebuilt local image.
