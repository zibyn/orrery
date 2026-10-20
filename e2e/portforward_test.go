//go:build e2e

package e2e

import (
	"bufio"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestPortForward is the "port-forward" acceptance scenario (#39 M1 item 16):
// the owner reaches an HTTP service through its subdomain, everyone else 404s,
// and the Sandbox publishes no port of its own.
func TestPortForward(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	sandboxID := readyExecSandbox(t, admin, 128<<20)
	domain := appsDomain()

	// A plain HTTP service in the Sandbox, reachable on 127.0.0.1:8000.
	// Alpine's busybox has nc but no httpd; the loop serves one request at a
	// time. The Exec stays running for the rest of the test.
	serve := "while true; do printf 'HTTP/1.1 200 OK\\r\\nContent-Length: 10\\r\\nConnection: close\\r\\n\\r\\nhello-app\\n' | nc -l -p 8000; done"
	createExec(t, admin, sandboxID, map[string]any{"argv": []string{"sh", "-c", serve}})
	// The first app request waits for the listener, not the test clock.
	probe := createExec(t, admin, sandboxID, map[string]any{"argv": []string{"sh", "-c",
		"for i in $(seq 1 50); do wget -qO- http://127.0.0.1:8000/ && exit 0; sleep 0.1; done; exit 1"}})
	assertExitedWith(t, waitExecFinished(t, admin, probe.ID, 15*time.Second), "exited")

	host := "8000--" + sandboxID + "." + domain

	// Only the owner can mint a ticket.
	status, data := call(t, http.MethodPost, "/api/v1/sandboxes/"+sandboxID+"/ports/8000/ticket", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("ticket = %d %s", status, data)
	}
	var ticket struct {
		URL string `json:"url"`
	}
	mustJSON(t, data, &ticket)
	u, err := url.Parse(ticket.URL)
	if err != nil || u.Host != host || u.Query().Get("ticket") == "" {
		t.Fatalf("ticket url = %q (%v), want host %q", ticket.URL, err, host)
	}

	// The ticket is exchanged for a cookie; the redirect drops it from the URL.
	resp := gatewayRequest(t, host, "/?"+u.RawQuery, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("ticket exchange = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/" {
		t.Fatalf("redirect Location = %q, want /", got)
	}
	var grant *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "orrery_port" {
			grant = c
		}
	}
	if grant == nil || grant.Value == "" || !grant.HttpOnly {
		t.Fatalf("ticket exchange cookies = %v", resp.Cookies())
	}

	// With the cookie the app answers over the Gateway.
	resp = gatewayRequest(t, host, "/", grant)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "hello-app") {
		t.Fatalf("gateway GET = %d %q", resp.StatusCode, body)
	}

	// A WebSocket to the same app forwards too: the CLI uses this path.
	cliForward(t, admin, sandboxID, "hello-app")

	// No ticket, a forged ticket, and a non-owner all see 404.
	if resp := gatewayRequest(t, host, "/", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("no cookie = %d, want 404", resp.StatusCode)
	}
	if resp := gatewayRequest(t, host, "/?ticket=forged", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("forged ticket = %d, want 404", resp.StatusCode)
	}

	username := "port-" + time.Now().Format("150405.000000000")
	adminCLI := newCLIRunner(t, admin)
	adminCLI.run(t, "port-password-123\n", "user", "create",
		"--name", username, "--login-type", "password", "--password-stdin")
	other, status, data := login(t, username, "port-password-123", "port-"+time.Now().Format("150405.000000000"), "user")
	if status != http.StatusCreated {
		t.Fatalf("login as %s: %d %s", username, status, data)
	}
	status, data = call(t, http.MethodPost, "/api/v1/sandboxes/"+sandboxID+"/ports/8000/ticket", other, nil)
	if status != http.StatusNotFound {
		t.Fatalf("non-owner ticket = %d %s, want 404", status, data)
	}
	req, _ := http.NewRequest(http.MethodGet, apiURL()+"/api/v1/sandboxes/"+sandboxID+"/ports/8000/tunnel", nil)
	req.Header.Set("Authorization", "Bearer "+other)
	tunnelResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	tunnelResp.Body.Close()
	if tunnelResp.StatusCode != http.StatusNotFound {
		t.Fatalf("non-owner tunnel = %d, want 404", tunnelResp.StatusCode)
	}

	// An admin may use the port too: the check is owner-or-admin, as for every
	// other Sandbox endpoint.
	adminName := "port-admin-" + time.Now().Format("150405.000000000")
	adminCLI.run(t, "port-password-123\n", "user", "create",
		"--name", adminName, "--login-type", "password", "--password-stdin", "--is-admin")
	otherAdmin, status, data := login(t, adminName, "port-password-123", "port-admin-"+time.Now().Format("150405.000000000"), "admin")
	if status != http.StatusCreated {
		t.Fatalf("login as %s: %d %s", adminName, status, data)
	}
	status, data = call(t, http.MethodPost, "/api/v1/sandboxes/"+sandboxID+"/ports/8000/ticket", otherAdmin, nil)
	if status != http.StatusOK {
		t.Fatalf("admin ticket = %d %s, want 200", status, data)
	}
	cliForward(t, otherAdmin, sandboxID, "hello-app")

	// The Sandbox itself publishes nothing on the host.
	out, err := exec.Command("docker", "inspect", "--format", "{{json .HostConfig.PortBindings}}",
		"orrery-"+sandboxID).Output()
	if err != nil {
		t.Fatalf("docker inspect: %v", err)
	}
	if bindings := strings.TrimSpace(string(out)); bindings != "{}" && bindings != "null" {
		t.Fatalf("Sandbox publishes ports: %s", bindings)
	}
}

// gatewayRequest sends a request to the API listener with a port subdomain's
// Host header, as a wildcard-DNS deployment would route it.
func gatewayRequest(t *testing.T, host, path string, cookie *http.Cookie) *http.Response {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, err := http.NewRequest(http.MethodGet, apiURL()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// cliForward runs `port-forward` and reads the app through the local port.
func cliForward(t *testing.T, admin, sandboxID, want string) {
	t.Helper()
	cmd := exec.Command(cliPath(t), "--api-url", apiURL(), "--token", admin,
		"port-forward", sandboxID, "0:8000")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("port-forward output: %v", err)
	}
	fields := strings.Fields(line) // forwarding 127.0.0.1:PORT -> ...
	if len(fields) < 2 {
		t.Fatalf("port-forward output = %q", line)
	}
	var body []byte
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get("http://" + fields[1] + "/")
		if err == nil {
			body, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			if err == nil && resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("port-forward HTTP: %v (%q)", err, body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(string(body), want) {
		t.Fatalf("port-forward body = %q, want %q", body, want)
	}
}

func appsDomain() string {
	if v := os.Getenv("ORRERY_APPS_DOMAIN"); v != "" {
		return v
	}
	return "apps.test"
}
