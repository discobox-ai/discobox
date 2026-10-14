// Package controlplane contains shared control-plane endpoint defaults.
package controlplane

import (
	"net"
	"strconv"
)

// DefaultPort is the default TCP port for the local control plane.
const DefaultPort = 18080

// DefaultURL returns the control-plane URL clients should use for host and port.
func DefaultURL(host string, port int) string {
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port))
}
