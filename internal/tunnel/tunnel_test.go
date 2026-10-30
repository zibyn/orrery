package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
)

type fakeStream struct {
	in  chan *nodev1.TunnelFrame
	out chan *nodev1.TunnelFrame
}

func newFakeStream() *fakeStream {
	return &fakeStream{in: make(chan *nodev1.TunnelFrame, 8), out: make(chan *nodev1.TunnelFrame, 8)}
}

func (f *fakeStream) Context() context.Context { return context.Background() }

func (f *fakeStream) Send(frame *nodev1.TunnelFrame) error {
	f.out <- frame
	return nil
}

func (f *fakeStream) Recv() (*nodev1.TunnelFrame, error) {
	frame, ok := <-f.in
	if !ok {
		return nil, io.EOF
	}
	return frame, nil
}

func TestConnCarriesBytesInBothDirections(t *testing.T) {
	stream := newFakeStream()
	conn := NewConn("t1", stream)

	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	sent := <-stream.out
	if sent.TunnelId != "t1" || string(sent.Data) != "hello" {
		t.Fatalf("sent frame = %+v", sent)
	}

	stream.in <- &nodev1.TunnelFrame{Data: []byte("ab")}
	stream.in <- &nodev1.TunnelFrame{TunnelId: "t1", Data: []byte("cdef")}
	buf := make([]byte, 2)
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != "ab" {
		t.Fatalf("read = %q, %v", buf[:n], err)
	}
	rest := make([]byte, 4)
	if _, err := io.ReadFull(conn, rest); err != nil || string(rest) != "cdef" {
		t.Fatalf("rest = %q, %v", rest, err)
	}

	other := &nodev1.TunnelFrame{TunnelId: "other", Data: []byte("x")}
	stream.in <- other
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("a frame for another tunnel must be rejected")
	}
}

// The Agent cancels a Tunnel only after the Control Plane ends it (#172), so
// Ended must wait for the peer, not for the local Close.
func TestEndedWaitsForThePeerAfterClose(t *testing.T) {
	stream := newFakeStream()
	conn := NewConn("t9", stream)
	conn.Close() //nolint:errcheck // local half done
	select {
	case <-conn.Ended():
		t.Fatal("Ended before the peer ended the stream")
	case <-time.After(50 * time.Millisecond):
	}
	close(stream.in) // the peer ends the RPC
	select {
	case <-conn.Ended():
	case <-time.After(time.Second):
		t.Fatal("Ended did not close after the peer ended the stream")
	}
}

func TestReadDeadlineWakesABlockedRead(t *testing.T) {
	stream := newFakeStream()
	conn := NewConn("t3", stream)
	errs := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		errs <- err
	}()
	// net/http aborts a background read this way when it hijacks the conn.
	if err := conn.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errs:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Read = %v, want a deadline error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SetReadDeadline did not wake Read")
	}

	// The stream is still usable: the pump kept the frame.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	stream.in <- &nodev1.TunnelFrame{TunnelId: "t3", Data: []byte("after")}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "after" {
		t.Fatalf("Read after deadline = %q, %v", buf, err)
	}
}

func TestConnRejectsWritesAfterClose(t *testing.T) {
	conn := NewConn("t2", newFakeStream())
	_ = conn.Close()
	if _, err := conn.Write([]byte("x")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Write after Close = %v", err)
	}
	select {
	case <-conn.Done():
	default:
		t.Fatal("Done is not closed")
	}
}
