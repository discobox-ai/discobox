//go:build linux || darwin

package proxyagent

import "syscall"

const gateConnectionRefused = syscall.ECONNREFUSED
