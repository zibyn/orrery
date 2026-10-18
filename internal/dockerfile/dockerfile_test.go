package dockerfile

import (
	"reflect"
	"testing"
)

func TestLogicalLines(t *testing.T) {
	text := "# syntax=docker/dockerfile:1\r\n" +
		"FROM alpine@sha256:aaaa AS base\n" +
		"RUN echo one \\\n" +
		"    && echo two\n" +
		"\n" +
		"  # indented comment\n" +
		"COPY --from=base /x /y\n" +
		"RUN trailing \\"
	want := []string{
		"FROM alpine@sha256:aaaa AS base",
		"RUN echo one && echo two",
		"COPY --from=base /x /y",
		"RUN trailing",
	}
	if got := LogicalLines(text); !reflect.DeepEqual(got, want) {
		t.Fatalf("LogicalLines = %#v, want %#v", got, want)
	}
}
