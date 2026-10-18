package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFollowOutputWritesFramesAndReturnsFinalState(t *testing.T) {
	var offsets []string
	frames := []Frame{
		{Seq: 0, Stream: "stdout", Data: "hello "},
		{Seq: 11, Stream: "stderr", Data: "oops"},
		{Seq: 20, Stream: "stdout", Data: base64.StdEncoding.EncodeToString([]byte{0xff}), Encoding: "base64"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/execs/e1/output":
			after := r.URL.Query().Get("after")
			offsets = append(offsets, after)
			if after == "0" {
				_ = json.NewEncoder(w).Encode(frames)
				return
			}
			_ = json.NewEncoder(w).Encode([]Frame{})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/execs/e1":
			code := int32(7)
			_ = json.NewEncoder(w).Encode(Exec{
				ID: "e1", State: "finished",
				Result: &ExecResult{Cause: "exited", ExitCode: &code},
			})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	final, err := New(srv.URL, "tok").FollowOutput(context.Background(), "e1", &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.Bytes(), append([]byte("hello "), 0xff); !bytes.Equal(got, want) {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.String() != "oops" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if final.State != "finished" || final.Result == nil || final.Result.Cause != "exited" || *final.Result.ExitCode != 7 {
		t.Fatalf("final Exec = %+v", final)
	}
	if len(offsets) != 2 || offsets[0] != "0" {
		t.Fatalf("offsets = %v", offsets)
	}
	// "hello " ends at seq 0 + header 5 + 6 = 11; "oops" at 11 + 5 + 4 = 20;
	// the base64 0xff frame at 20 + 5 + 1 = 26.
	if offsets[1] != "26" {
		t.Fatalf("second offset = %q, want 26", offsets[1])
	}
}

func TestFrameEndAndSizeDecodeBase64(t *testing.T) {
	frame := Frame{Seq: 7, Stream: "stdout", Data: base64.StdEncoding.EncodeToString([]byte{0xff, 0x00}), Encoding: "base64"}
	if got := frame.Size(); got != 2 {
		t.Fatalf("Size() = %d, want 2", got)
	}
	if got := frame.End(); got != 7+5+2 {
		t.Fatalf("End() = %d, want 14", got)
	}
	if got := (Frame{Seq: 0, Data: "hi"}).End(); got != 7 {
		t.Fatalf("plain End() = %d, want 7", got)
	}
}

func TestRunExecWithoutWaitReturnsImmediately(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Exec{ID: "e1", State: "pending"})
	}))
	defer srv.Close()

	e, err := New(srv.URL, "tok").RunExec(context.Background(), "sb1", ExecRequest{
		Argv: []string{"true"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "e1" || e.State != "pending" {
		t.Fatalf("Exec = %+v", e)
	}
}
