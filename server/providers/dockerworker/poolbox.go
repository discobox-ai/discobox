package dockerworker

// A Docker pool is made the way a pool makes a sandbox (ADR 26-10-24-953
// §§2, 4): its container runs the sandbox base image, systemd as PID 1, and
// poolruntime.ProvisionBox makes it the pool's box over the container as a
// machine.Guest (provisioner.ContainerGuest), as a VM pool's machine is made
// over disco-vm's.
