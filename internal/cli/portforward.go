package cli

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"golang.org/x/net/websocket"
)

// runPortForward serves a local TCP port that is tunneled to a Sandbox port
// over the API WebSocket (api.md §6). One WebSocket is opened per accepted
// connection.
func runPortForward(c *client, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: orrery port-forward <sandbox-id> [LOCAL:]REMOTE")
	}
	sandboxID := args[0]
	local, remoteRaw, colon := strings.Cut(args[1], ":")
	if !colon {
		local, remoteRaw = "", args[1]
	}
	remote, err := strconv.Atoi(remoteRaw)
	if err != nil || remote < 1 || remote > 65535 {
		return fmt.Errorf("remote port must be an integer in 1-65535")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", local))
	if err != nil {
		return err
	}
	defer listener.Close() //nolint:errcheck // process lifetime
	fmt.Fprintf(c.out, "forwarding 127.0.0.1:%d -> sandbox %s port %d\n",
		listener.Addr().(*net.TCPAddr).Port, sandboxID, remote)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go forwardToSandbox(c, sandboxID, remote, conn)
	}
}

func forwardToSandbox(c *client, sandboxID string, port int, local net.Conn) {
	defer local.Close() //nolint:errcheck // connection lifetime
	config, err := websocket.NewConfig(c.TunnelURL(sandboxID, port), c.Origin())
	if err != nil {
		return
	}
	config.Protocol = []string{"sandbox.v1"}
	config.Header.Set("Authorization", "Bearer "+c.Token())
	ws, err := websocket.DialConfig(config)
	if err != nil {
		return
	}
	defer ws.Close() //nolint:errcheck // connection lifetime

	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		for {
			n, err := local.Read(buf)
			if n > 0 {
				if err := websocket.Message.Send(ws, append([]byte(nil), buf[:n]...)); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			var data []byte
			if err := websocket.Message.Receive(ws, &data); err != nil {
				return
			}
			if _, err := local.Write(data); err != nil {
				return
			}
		}
	}()
	<-done
}
