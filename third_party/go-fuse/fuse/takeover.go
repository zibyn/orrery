package fuse

// Handover support for a read-only server whose state is deterministic: the
// next process takes the live session fd and serves it without a new INIT.

import (
	"os"
	"path/filepath"
	"runtime"
	"unsafe"
)

// NewServerFromFD serves fs on an already initialized FUSE session. init and
// negotiated are the previous server's KernelSettings() and NegotiatedFlags().
func NewServerFromFD(fs RawFileSystem, fd int, mountPoint string, opts *MountOptions, init InitIn, negotiated uint64) (*Server, error) {
	o := *opts
	o.setDefaults(fs)
	maxReaders := min(max(runtime.GOMAXPROCS(0), minMaxReaders), maxMaxReaders)
	readBufSize, readBufBytes, _ := requestAccountingSizes(o.MaxWrite)
	ms := &Server{
		protocolServer: protocolServer{
			fileSystem:  fs,
			retrieveTab: make(map[uint64]*retrieveCacheRequest),
			opts:        &o,
		},
		opts:         &o,
		maxReaders:   maxReaders,
		singleReader: useSingleReader,
		ready:        make(chan error, 1),
	}
	ms.reqPool.New = func() any {
		return &requestAlloc{request: request{cancel: make(chan struct{})}}
	}
	ms.readPool.New = func() any {
		buf := make([]byte, readBufBytes)
		return alignSlice(buf, unsafe.Sizeof(WriteIn{}), logicalBlockSize, uintptr(readBufSize))
	}
	ms.mountPoint = filepath.Clean(mountPoint)
	var err error
	if ms.fuseFD, err = ms.newFuseFD(fd); err != nil {
		return nil, err
	}
	ms.protocolServer.writev = ms.fuseFD.writev
	ms.kernelSettings = init
	ms.negotiatedFlags = negotiated
	if init.Minor >= 13 {
		ms.setSplice()
	}
	ms.fileSystem.Init(ms)
	ms.fuseFD.loops.Add(1)
	ms.ready <- nil
	return ms, nil
}

// NegotiatedFlags returns the capability flags agreed at INIT.
func (ms *Server) NegotiatedFlags() uint64 { return ms.negotiatedFlags }

// SessionFile returns the FUSE session file, to pass to the next server.
func (ms *Server) SessionFile() *os.File { return ms.fuseFD.file }

// StopReading makes every reader exit after answering its current request.
func (ms *Server) StopReading() { ms.stopping.Store(true) }

// LoopsDone returns a channel closed when every reader has exited.
func (ms *Server) LoopsDone() <-chan struct{} {
	ch := make(chan struct{})
	go func() { ms.fuseFD.loops.Wait(); close(ch) }()
	return ch
}
