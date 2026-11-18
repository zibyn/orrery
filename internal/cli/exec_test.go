package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestExecRunArgv(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Argv []string `json:"argv"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		got = body.Argv
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"e1","sandbox_id":"sb","state":"pending"}`))
	}))
	defer srv.Close()

	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"sb", "--", "ls", "/"}, []string{"ls", "/"}},
		{[]string{"sb", "ls", "--", "x"}, []string{"ls", "--", "x"}},
		{[]string{"sb", "--", "--", "x"}, []string{"--", "x"}},
	} {
		got = nil
		args := append([]string{"--api-url", srv.URL, "--token", "t", "exec", "run", "--no-follow"}, tc.args...)
		if code, _, stderr := runCLI(t, nil, args...); code != 0 {
			t.Fatalf("%v: exit %d: %s", tc.args, code, stderr)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%v: argv %q, want %q", tc.args, got, tc.want)
		}
	}

	got = nil
	if code, _, _ := runCLI(t, nil, "--api-url", srv.URL, "--token", "t", "exec", "run", "sb", "--"); code == 0 || got != nil {
		t.Errorf("`sb --` with no command: exit %d, sent %q", code, got)
	}
}
