package ports

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// sockDiagRecvTimeout bounds each read of a dump's answer, so a kernel that
// never finishes one cannot stall the watcher: the scan reads procfs instead.
const sockDiagRecvTimeout = time.Second

// inetDiagReqV2Len is sizeof(struct inet_diag_req_v2): family, protocol, ext
// and pad bytes, the states mask, then a zeroed 48-byte inet_diag_sockid.
// x/sys/unix does not define it.
const inetDiagReqV2Len = 56

// sockDiagDump asks the kernel for every TCP socket of family in TCP_LISTEN, in
// this process's network namespace, and returns each answer's inet_diag_msg.
// A kernel that does not do sock_diag for TCP answers errSockDiagUnsupported.
func sockDiagDump(family uint8) ([][]byte, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return nil, sockDiagErr("socket", err)
	}
	defer unix.Close(fd)
	tv := unix.NsecToTimeval(sockDiagRecvTimeout.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return nil, fmt.Errorf("sock_diag: %w", err)
	}

	const seq = 1
	req := make([]byte, unix.NLMSG_HDRLEN+inetDiagReqV2Len)
	binary.NativeEndian.PutUint32(req[0:4], uint32(len(req)))
	binary.NativeEndian.PutUint16(req[4:6], unix.SOCK_DIAG_BY_FAMILY)
	binary.NativeEndian.PutUint16(req[6:8], unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	binary.NativeEndian.PutUint32(req[8:12], seq)
	body := req[unix.NLMSG_HDRLEN:]
	body[0] = family
	body[1] = unix.IPPROTO_TCP
	binary.NativeEndian.PutUint32(body[4:8], 1<<tcpListen)
	if err := unix.Sendto(fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, sockDiagErr("send", err)
	}

	var out [][]byte
	buf := make([]byte, 64<<10)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return nil, sockDiagErr("receive", err)
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return nil, fmt.Errorf("sock_diag: %w", err)
		}
		for _, m := range msgs {
			if m.Header.Seq != seq {
				continue
			}
			switch m.Header.Type {
			case unix.NLMSG_DONE:
				return out, nil
			case unix.NLMSG_ERROR:
				if len(m.Data) < 4 {
					return nil, errors.New("sock_diag: a short error message")
				}
				errno := -int32(binary.NativeEndian.Uint32(m.Data[:4]))
				if errno == 0 {
					continue
				}
				return nil, sockDiagErr("dump", syscall.Errno(errno))
			case unix.SOCK_DIAG_BY_FAMILY:
				// m.Data is buf's, which the next read overwrites.
				out = append(out, slices.Clone(m.Data))
			}
		}
	}
}

// sockDiagErr is err from step, marked errSockDiagUnsupported when it is the
// kernel saying it has no sock_diag for this: no NETLINK_SOCK_DIAG family
// (EPROTONOSUPPORT), no inet_diag handler for the protocol (ENOENT), a request
// it does not understand (EINVAL), or one a seccomp policy refuses outright.
func sockDiagErr(step string, err error) error {
	switch {
	case errors.Is(err, unix.EPROTONOSUPPORT),
		errors.Is(err, unix.EAFNOSUPPORT),
		errors.Is(err, unix.ENOENT),
		errors.Is(err, unix.EINVAL),
		errors.Is(err, unix.EOPNOTSUPP),
		errors.Is(err, unix.ENOSYS),
		errors.Is(err, unix.EPERM),
		errors.Is(err, unix.EACCES):
		return fmt.Errorf("sock_diag %s: %w: %w", step, errSockDiagUnsupported, err)
	}
	return fmt.Errorf("sock_diag %s: %w", step, err)
}
