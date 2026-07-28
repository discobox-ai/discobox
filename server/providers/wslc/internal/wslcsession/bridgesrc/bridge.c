// Minimal stdio <-> socket splice, running inside a wslc guest VM.
//
// wslc's guest image ships no tool capable of bridging a process's stdio to
// an arbitrary unix socket or TCP address (no socat/ncat/python3, and
// BusyBox's nc has no -U support) - this fills that one gap so a Windows
// host process can reach anything inside the guest's network/mount
// namespace via CreateRootNamespaceProcess's stdio handles, without needing
// a public TCP port anywhere.
//
// Usage:
//   bridge unix:/path/to/socket
//   bridge tcp:host:port
#include <errno.h>
#include <netdb.h>
#include <poll.h>
#include <stdio.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

static int write_all(int fd, const char *buf, ssize_t n) {
    ssize_t off = 0;
    while (off < n) {
        ssize_t w = write(fd, buf + off, n - off);
        if (w <= 0) return -1;
        off += w;
    }
    return 0;
}

static int connect_unix(const char *path) {
    int sock = socket(AF_UNIX, SOCK_STREAM, 0);
    if (sock < 0) return -1;

    struct sockaddr_un addr;
    memset(&addr, 0, sizeof(addr));
    addr.sun_family = AF_UNIX;
    strncpy(addr.sun_path, path, sizeof(addr.sun_path) - 1);

    if (connect(sock, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
        close(sock);
        return -1;
    }
    return sock;
}

static int connect_tcp(const char *hostport) {
    char host[256];
    const char *colon = strrchr(hostport, ':');
    if (colon == NULL || colon == hostport) {
        fprintf(stderr, "invalid tcp target: %s\n", hostport);
        return -1;
    }

    size_t hostlen = (size_t)(colon - hostport);
    if (hostlen >= sizeof(host)) {
        fprintf(stderr, "host too long: %s\n", hostport);
        return -1;
    }
    memcpy(host, hostport, hostlen);
    host[hostlen] = '\0';
    const char *port = colon + 1;

    struct addrinfo hints;
    memset(&hints, 0, sizeof(hints));
    hints.ai_family = AF_UNSPEC;
    hints.ai_socktype = SOCK_STREAM;

    struct addrinfo *result = NULL;
    if (getaddrinfo(host, port, &hints, &result) != 0) {
        return -1;
    }

    int sock = -1;
    for (struct addrinfo *rp = result; rp != NULL; rp = rp->ai_next) {
        sock = socket(rp->ai_family, rp->ai_socktype, rp->ai_protocol);
        if (sock < 0) continue;
        if (connect(sock, rp->ai_addr, rp->ai_addrlen) == 0) break;
        close(sock);
        sock = -1;
    }

    freeaddrinfo(result);
    return sock;
}

int main(int argc, char **argv) {
    if (argc < 2) {
        fprintf(stderr, "usage: %s unix:<path> | tcp:<host>:<port>\n", argv[0]);
        return 1;
    }

    int sock;
    if (strncmp(argv[1], "unix:", 5) == 0) {
        sock = connect_unix(argv[1] + 5);
    } else if (strncmp(argv[1], "tcp:", 4) == 0) {
        sock = connect_tcp(argv[1] + 4);
    } else {
        fprintf(stderr, "target must start with unix: or tcp:\n");
        return 1;
    }

    if (sock < 0) {
        perror("connect");
        return 1;
    }

    char buf[65536];
    struct pollfd fds[2];
    fds[0].fd = 0;
    fds[0].events = POLLIN;
    fds[1].fd = sock;
    fds[1].events = POLLIN;

    // Each direction is torn down independently on its own EOF; the whole
    // relay only ends once BOTH are done (or a real write error occurs).
    // Treating either side's EOF as "stop everything" - the original,
    // buggy version of this loop did exactly that for the sock->stdout
    // direction - discards a peer's still-unread final bytes whenever the
    // stdin side finishes first, and drops any not-yet-sent stdin data
    // whenever the peer (e.g. a target that responds and half-closes
    // early) finishes first. Both orderings are legal TCP behavior.
    for (;;) {
        if (fds[0].fd == -1 && fds[1].fd == -1) break;

        int r = poll(fds, 2, -1);
        if (r < 0) {
            if (errno == EINTR) continue;
            break;
        }

        if (fds[0].fd != -1 && (fds[0].revents & (POLLIN | POLLHUP | POLLERR))) {
            ssize_t n = read(0, buf, sizeof(buf));
            if (n <= 0) {
                shutdown(sock, SHUT_WR);
                fds[0].fd = -1;
            } else if (write_all(sock, buf, n) < 0) {
                break; // real error, not a clean close - abort both directions
            }
        }

        if (fds[1].fd != -1 && (fds[1].revents & (POLLIN | POLLHUP | POLLERR))) {
            ssize_t n = read(sock, buf, sizeof(buf));
            if (n <= 0) {
                // Peer done sending. Actually close(1) here, not just stop
                // polling it: whoever is reading our stdout (the Windows
                // side, via recv() on the relayed handle) is very likely
                // blocked waiting for EOF right now - e.g. an io.ReadAll-
                // style "write a request, then read the full response"
                // caller that never bothers to half-close its own write
                // side for a one-shot request. If we only mark fds[1] done
                // without closing fd 1, that reader hangs forever waiting
                // for a signal we're never going to send while we sit here
                // still forwarding an already-finished stdin. Closing fd 1
                // immediately unblocks it, independently of whether stdin
                // still has more to send to the target.
                close(1);
                fds[1].fd = -1;
            } else if (write_all(1, buf, n) < 0) {
                break; // real error
            }
        }
    }

    close(sock);
    return 0;
}
