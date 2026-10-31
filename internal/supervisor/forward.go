package supervisor

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Forward serves one Sandbox port on a unix socket in the shared run mount.
// It runs inside the Sandbox, so dialing 127.0.0.1:<port> stays in the
// Sandbox's network namespace; the Agent on the host just connects to the
// socket (api.md §6, ADR-0017).
func Forward(portRaw, socket string) error {
	port, err := strconv.Atoi(portRaw)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid port %q", portRaw)
	}
	dir := filepath.Dir(socket)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	// The Agent runs unprivileged on the host; it must be able to connect and
	// to remove a stale socket after a restart.
	_ = os.Chmod(dir, 0o777)
	// A forwarder for this port may already be serving (a reconnect or a
	// second request); leave it alone.
	if probe, err := net.Dial("unix", socket); err == nil {
		probe.Close()
		return nil
	}
	_ = os.Remove(socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close() //nolint:errcheck // process lifetime
	if err := os.Chmod(socket, 0o777); err != nil {
		return err
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go forwardConn(conn, port)
	}
}

// Dial opens and closes one TCP connection from inside the Sandbox: the
// egress_policy self-check (runtime.md §13).
func Dial(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}

func forwardConn(conn net.Conn, port int) {
	defer conn.Close() //nolint:errcheck // connection lifetime
	upstream, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return
	}
	defer upstream.Close() //nolint:errcheck // connection lifetime
	Bridge(conn, upstream)
}

// Bridge copies bytes both ways until either side closes.
func Bridge(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}
