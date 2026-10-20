// Package tunnel turns a Tunnel gRPC stream into a net.Conn: raw bytes in both
// directions, the reverse-dialled half of ADR-0018.
package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
)

// Stream is what both Node_TunnelClient and Node_TunnelServer provide.
type Stream interface {
	Context() context.Context
	Send(*nodev1.TunnelFrame) error
	Recv() (*nodev1.TunnelFrame, error)
}

// Conn carries one tunnel's bytes. Read and Write may run concurrently; Close
// wakes the owner (the Tunnel RPC handler) and makes later writes fail.
//
// Recv is owned by one pump goroutine because net/http hijacking needs a
// working read deadline: it aborts its background read by calling
// SetReadDeadline, which cannot interrupt a gRPC Recv directly. Read therefore
// waits on the pump's output and on deadline changes.
type Conn struct {
	id     string
	stream Stream

	incoming chan []byte
	pumpErr  error // set before incoming is closed

	readMu  sync.Mutex
	pending []byte

	deadlineMu sync.Mutex
	deadline   time.Time
	wake       chan struct{} // closed and replaced whenever the deadline changes

	sendMu sync.Mutex

	done chan struct{}
	once sync.Once
}

func NewConn(id string, stream Stream) *Conn {
	c := &Conn{
		id:       id,
		stream:   stream,
		incoming: make(chan []byte, 16),
		wake:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go c.pump()
	return c
}

func (c *Conn) pump() {
	defer close(c.incoming)
	for {
		frame, err := c.stream.Recv()
		if err != nil {
			c.pumpErr = err
			return
		}
		if id := frame.GetTunnelId(); id != "" && id != c.id {
			c.pumpErr = errors.New("tunnel frame for a different tunnel")
			return
		}
		select {
		case c.incoming <- frame.GetData():
		case <-c.done:
			return
		}
	}
}

// Done closes when the Conn is closed. The server-side Tunnel handler waits on
// it and then returns, which ends the RPC.
func (c *Conn) Done() <-chan struct{} { return c.done }

func (c *Conn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for len(c.pending) == 0 {
		c.deadlineMu.Lock()
		deadline, wake := c.deadline, c.wake
		c.deadlineMu.Unlock()
		var timer *time.Timer
		var timeout <-chan time.Time
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(remaining)
			timeout = timer.C
		}
		select {
		case data, ok := <-c.incoming:
			stopTimer(timer)
			if !ok {
				if c.pumpErr != nil {
					return 0, c.pumpErr
				}
				return 0, io.EOF
			}
			c.pending = data
		case <-timeout:
			return 0, os.ErrDeadlineExceeded
		case <-wake: // the deadline changed; re-evaluate it
			stopTimer(timer)
		case <-c.done:
			stopTimer(timer)
			return 0, net.ErrClosed
		}
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func stopTimer(timer *time.Timer) {
	if timer != nil {
		timer.Stop()
	}
}

func (c *Conn) Write(p []byte) (int, error) {
	select {
	case <-c.done:
		return 0, net.ErrClosed
	default:
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	data := append([]byte(nil), p...)
	if err := c.stream.Send(&nodev1.TunnelFrame{TunnelId: c.id, Data: data}); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *Conn) Close() error {
	c.once.Do(func() {
		close(c.done)
		// The client half also tells the Control Plane no more bytes follow.
		if closer, ok := c.stream.(interface{ CloseSend() error }); ok {
			_ = closer.CloseSend()
		}
	})
	return nil
}

func (c *Conn) LocalAddr() net.Addr  { return addr(c.id) }
func (c *Conn) RemoteAddr() net.Addr { return addr(c.id) }

func (c *Conn) SetDeadline(t time.Time) error { return c.SetReadDeadline(t) }

// SetReadDeadline wakes a blocked Read so net/http can take the connection
// over; gRPC has no per-read timeout of its own.
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	c.deadline = t
	close(c.wake)
	c.wake = make(chan struct{})
	c.deadlineMu.Unlock()
	return nil
}

func (c *Conn) SetWriteDeadline(time.Time) error { return nil }

type addr string

func (a addr) Network() string { return "tunnel" }
func (a addr) String() string  { return string(a) }
