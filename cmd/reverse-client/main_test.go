package main

import (
  "encoding/json"
  "log/slog"
  "net/http"
  "net/http/httptest"
  "os"
  "path/filepath"
  "reflect"
  "testing"
  "time"

  protocolvalidation "github.com/portflare/protocol/validation"
)

func TestIsValidClientKey(t *testing.T) {
  if !protocolvalidation.IsValidClientKey("pf_123") {
    t.Fatal("expected pf_ prefix to be accepted")
  }
  if protocolvalidation.IsValidClientKey("abc123") {
    t.Fatal("expected missing pf_ prefix to be rejected")
  }
}

func TestToWebSocketURL(t *testing.T) {
  got, err := toWebSocketURL("https://reverse.example.test")
  if err != nil {
    t.Fatalf("unexpected error: %v", err)
  }
  if got.String() != "wss://reverse.example.test" {
    t.Fatalf("unexpected ws url: %s", got.String())
  }
}

func TestServiceStatePersistence(t *testing.T) {
  dir := t.TempDir()
  svc := &Service{
    cfg: Config{StatePath: filepath.Join(dir, "state.json")},
    apps: map[string]*AppRegistration{
      "web": {AppName: "web", TargetURL: "http://127.0.0.1:3000"},
    },
  }

  if err := svc.saveState(); err != nil {
    t.Fatalf("saveState failed: %v", err)
  }

  next := &Service{cfg: Config{StatePath: filepath.Join(dir, "state.json")}, apps: map[string]*AppRegistration{}}
  if err := next.loadState(); err != nil {
    t.Fatalf("loadState failed: %v", err)
  }

  app, ok := next.apps["web"]
  if !ok || app.TargetURL != "http://127.0.0.1:3000" {
    t.Fatalf("unexpected loaded app: %#v", app)
  }

  if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
    t.Fatalf("expected state file to exist: %v", err)
  }
}

func TestParsePortRanges(t *testing.T) {
  got, err := parsePortRanges("3000,8080,9000-9002")
  if err != nil {
    t.Fatalf("unexpected error: %v", err)
  }
  want := []portRange{{Start: 3000, End: 3000}, {Start: 8080, End: 8080}, {Start: 9000, End: 9002}}
  if !reflect.DeepEqual(got, want) {
    t.Fatalf("unexpected ranges: %#v", got)
  }
}

func TestParsePortNameMap(t *testing.T) {
  got, err := parsePortNameMap("3000=web-ui,8080=admin")
  if err != nil {
    t.Fatalf("unexpected error: %v", err)
  }
  want := map[int]string{3000: "web-ui", 8080: "admin"}
  if !reflect.DeepEqual(got, want) {
    t.Fatalf("unexpected map: %#v", got)
  }
}

func TestPortAllowedAndDenied(t *testing.T) {
  allow := []portRange{{Start: 3000, End: 3000}, {Start: 8000, End: 8100}}
  deny := []portRange{{Start: 8080, End: 8080}}
  if !portAllowed(3000, allow) || !portAllowed(8080, allow) || portAllowed(9000, allow) {
    t.Fatal("unexpected port allow evaluation")
  }
  if !portDenied(8080, deny) || portDenied(8081, deny) {
    t.Fatal("unexpected port deny evaluation")
  }
}

func TestDiscoverCandidatesApplyNamingAndDeny(t *testing.T) {
  candidates := []discoverCandidate{
    {Port: 3000, AppName: "web-ui", TargetURL: "http://127.0.0.1:3000"},
    {Port: 8081, AppName: "app-8081", TargetURL: "http://127.0.0.1:8081"},
  }
  got := normalizeDiscoverCandidates(candidates)
  want := []discoverCandidate{
    {Port: 3000, AppName: "web-ui", TargetURL: "http://127.0.0.1:3000"},
    {Port: 8081, AppName: "app-8081", TargetURL: "http://127.0.0.1:8081"},
  }
  if !reflect.DeepEqual(got, want) {
    t.Fatalf("unexpected normalized candidates: %#v", got)
  }
}

func TestRefreshDiscoveryMarksMissingAppOfflineAfterGrace(t *testing.T) {
  svc := &Service{
    cfg:    Config{DiscoverGrace: time.Millisecond, DiscoverAllow: []portRange{{Start: 65535, End: 65535}}},
    logger: slog.Default(),
    apps: map[string]*AppRegistration{
      "app-65535": {
        AppName:        "app-65535",
        TargetURL:      "http://127.0.0.1:65535",
        Source:         "discovery",
        DiscoveredPort: 65535,
        LastSeenAt:     time.Now().Add(-time.Second),
      },
    },
  }

  svc.refreshDiscovery()

  if !svc.apps["app-65535"].Offline {
    t.Fatal("expected discovered app to be marked offline")
  }
}

func TestHandleAppsGroupsBySource(t *testing.T) {
  svc := &Service{apps: map[string]*AppRegistration{
    "web":      {AppName: "web", Source: "manual"},
    "app-3000": {AppName: "app-3000", Source: "discovery"},
  }}

  req := httptest.NewRequest(http.MethodGet, "/apps", nil)
  rr := httptest.NewRecorder()
  svc.handleApps(rr, req)

  var body struct {
    Apps          []AppRegistration `json:"apps"`
    ManualApps    []AppRegistration `json:"manual_apps"`
    DiscoveryApps []AppRegistration `json:"discovery_apps"`
  }
  if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
    t.Fatalf("decode response: %v", err)
  }
  if len(body.Apps) != 2 || len(body.ManualApps) != 1 || len(body.DiscoveryApps) != 1 {
    t.Fatalf("unexpected grouping: %#v", body)
  }
}

func TestHandleAppByNameDelete(t *testing.T) {
  dir := t.TempDir()
  svc := &Service{
    cfg: Config{StatePath: filepath.Join(dir, "state.json")},
    apps: map[string]*AppRegistration{
      "web": {AppName: "web", Source: "manual"},
    },
  }
  if err := svc.saveState(); err != nil {
    t.Fatalf("saveState failed: %v", err)
  }

  req := httptest.NewRequest(http.MethodDelete, "/apps/web", nil)
  rr := httptest.NewRecorder()
  svc.handleAppByName(rr, req)

  if rr.Code != http.StatusOK {
    t.Fatalf("unexpected status: %d body=%s", rr.Code, rr.Body.String())
  }
  if _, ok := svc.apps["web"]; ok {
    t.Fatal("expected app to be deleted")
  }
}

func TestHandleDiscoveryRescanDisabled(t *testing.T) {
  svc := &Service{}
  req := httptest.NewRequest(http.MethodPost, "/discovery/rescan", nil)
  rr := httptest.NewRecorder()
  svc.handleDiscoveryRescan(rr, req)
  if rr.Code != http.StatusBadRequest {
    t.Fatalf("unexpected status: %d", rr.Code)
  }
}
