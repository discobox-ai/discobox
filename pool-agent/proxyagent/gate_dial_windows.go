package proxyagent

import "golang.org/x/sys/windows"

// net.Dial returns Winsock errors, not the POSIX compatibility errno values.
const gateConnectionRefused = windows.WSAECONNREFUSED
