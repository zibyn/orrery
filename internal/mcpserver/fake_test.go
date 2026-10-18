package mcpserver_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zibyn/stars-sandbox/client"
	"github.com/zibyn/stars-sandbox/internal/mcpserver"
)

// fakeServer is an httptest server with route registration.
type fakeServer struct {
	*httptest.Server
	*fakeREST
}

func fakeAPI(t *testing.T) *fakeServer {
	t.Helper()
	api := &fakeREST{}
	api.handle("GET", "/api/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "next_cursor": nil})
	})
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	api.base = srv.URL
	return &fakeServer{Server: srv, fakeREST: api}
}

func clientAPI(api *fakeServer) *client.Client {
	return client.New(api.URL, "test")
}

func newServerFor(api *fakeServer) *sdk.Server {
	return mcpserver.New(clientAPI(api), nil)
}

type fakeREST struct {
	base    string
	routes  []fakeRoute
	request *http.Request
}

type fakeRoute struct {
	method  string
	prefix  string
	handler func(w http.ResponseWriter, r *http.Request)
}

func (f *fakeREST) handle(method, prefix string, h func(http.ResponseWriter, *http.Request)) {
	route := fakeRoute{method, prefix, h}
	for i := range f.routes {
		if f.routes[i].method == method && f.routes[i].prefix == prefix {
			f.routes[i] = route
			return
		}
	}
	f.routes = append(f.routes, route)
}

func (f *fakeREST) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.request = r
	var match *fakeRoute
	for i := range f.routes {
		route := &f.routes[i]
		if r.Method != route.method || !strings.HasPrefix(r.URL.Path, route.prefix) {
			continue
		}
		if match == nil || len(route.prefix) > len(match.prefix) {
			match = route
		}
	}
	if match != nil {
		match.handler(w, r)
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"code": "not_found", "detail": "no such route"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
