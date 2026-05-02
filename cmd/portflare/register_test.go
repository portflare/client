package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestRunCLIRegisterPostsToServerAndPrintsExports(t *testing.T) {
	var got struct {
		UserName string `json:"user_name"`
		Email    string `json:"email"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/register" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(RegistrationResponse{UserName: "alice", PublicUserLabel: "alice", Email: "alice@example.test", APIKey: "pf_testkey"})
	}))
	defer server.Close()

	stdout := captureStdout(t, func() {
		code := runCLI([]string{"register", "--server", server.URL, "--user", "alice", "--email", "alice@example.test"})
		if code != 0 {
			t.Fatalf("expected success, got code %d", code)
		}
	})

	if got.UserName != "alice" || got.Email != "alice@example.test" {
		t.Fatalf("unexpected registration request: %#v", got)
	}
	if !strings.Contains(stdout, "registered user alice") || !strings.Contains(stdout, "export PORTFLARE_CLIENT_KEY=\"pf_testkey\"") || !strings.Contains(stdout, "export PORTFLARE_SERVER_URL=\""+server.URL+"\"") {
		t.Fatalf("unexpected stdout: %s", stdout)
	}
}

func TestRunCLIRegisterRequiresUser(t *testing.T) {
	code := runCLI([]string{"register", "--server", "http://example.test"})
	if code != 1 {
		t.Fatalf("expected usage error, got %d", code)
	}
}

func TestRunCLIRegisterReturnsServerErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "registration closed", http.StatusForbidden)
	}))
	defer server.Close()

	code := runCLI([]string{"register", "--server", server.URL, "--user", "alice"})
	if code != 1 {
		t.Fatalf("expected server error code, got %d", code)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	fn()
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
