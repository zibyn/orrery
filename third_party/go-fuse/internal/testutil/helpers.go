// Copyright 2018 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package testutil

import (
	"log"
	"runtime"
	"testing"
)

// PanicHandler returns a fuse.MountOptions.PanicHandler that logs the
// panic, fails the test, and returns errno. The generic return type
// avoids a dependency on the fuse package, which would create an
// import cycle for tests within that package.
func PanicHandler[Status any](t *testing.T, errno Status) func(e any) Status {
	return func(e any) Status {
		buf := make([]byte, 64<<10)
		buf = buf[:runtime.Stack(buf, false)]
		log.Printf("panic in FS handler: %v\n%s", e, buf)
		t.Fail()
		return errno
	}
}
