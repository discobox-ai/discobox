package guestimage

// DefaultVMImage is the published pool VM guest image: one artifact set, built
// from one Dockerfile (vm-image/Dockerfile), booted by every VM backend that
// has no guest of its own.
//
// It is released and versioned on its own line, independently of the discobox
// release, because it is a distribution userland that boots dockerd and changes
// when the distribution does rather than when Discobox does (ADR 0062 §3). A
// release pins this to a digest; a tag here would mean whoever runs the server
// decides which build they get.
//
// It lives here rather than in a driver because the image is shared. `vz` boots
// its linux/arm64 variant and `libkrun` its linux/amd64 one, and two constants
// would be two things to re-pin after one publish — which is exactly how a
// backend ends up quietly booting last release's guest.
//
// Published as discobox-vm, with no backend in the name, and deliberately not
// called a pool image: that already means the pool-agent container
// (dockerworker.DefaultPoolImage), which every VM provider exposes as
// workerImage alongside this one.
const DefaultVMImage = "ghcr.io/discobox-ai/discobox-vm@sha256:af1d6ee4ac0b833c7432f61651b29dacaad7a9c71e7e080ac3dfcdc1c8e46a48"
