package main

import (
  "bufio"
  "bytes"
  "context"
  "encoding/base64"
  "encoding/json"
  "errors"
  "fmt"
  "io"
  "log/slog"
  "net/http"
  "net/url"
  "os"
  "os/signal"
  "path/filepath"
  "sort"
  "strconv"
  "strings"
  "sync"
  "syscall"
  "time"

  "github.com/gorilla/websocket"

  "github.com/portflare/client/internal/buildinfo"
)

type Config struct {
  ServerURL         string
  ClientKey         string
  LocalAPIAddr      string
  StatePath         string
  ReconnectDelay    time.Duration
  HTTPTimeout       time.Duration
  DiscoverEnabled   bool
  DiscoverInterval  time.Duration
  DiscoverGrace     time.Duration
  DiscoverAllow     []portRange
  DiscoverDeny      []portRange
  DiscoverNameByPort map[int]string
}

type AppRegistration struct {
  AppName       string    `json:"app_name"`
  TargetURL     string    `json:"target_url"`
  PublicPort    int       `json:"public_port,omitempty"`
  Approved      bool      `json:"approved"`
  Source        string    `json:"source,omitempty"`
  DiscoveredPort int      `json:"discovered_port,omitempty"`
  Offline       bool      `json:"offline,omitempty"`
  LastSeenAt    time.Time `json:"last_seen_at,omitempty"`
  CreatedAt     time.Time `json:"created_at"`
  UpdatedAt     time.Time `json:"updated_at"`
}

type ConnectMessage struct {
  Type        string              `json:"type"`
  RequestID   string              `json:"request_id,omitempty"`
  AppName     string              `json:"app_name,omitempty"`
  PublicPort  int                 `json:"public_port,omitempty"`
  Method      string              `json:"method,omitempty"`
  URL         string              `json:"url,omitempty"`
  Headers     map[string][]string `json:"headers,omitempty"`
  BodyBase64  string              `json:"body_base64,omitempty"`
  StatusCode  int                 `json:"status_code,omitempty"`
  Error       string              `json:"error,omitempty"`
  Approved    bool                `json:"approved,omitempty"`
  UserName    string              `json:"user_name,omitempty"`
  Message     string              `json:"message,omitempty"`
}

type clientState struct {
  Apps map[string]*AppRegistration `json:"apps"`
}

type discoverCandidate struct {
  AppName   string
  TargetURL string
  Port      int
}

type portRange struct {
  Start int
  End   int
}

type Service struct {
  cfg    Config
  logger *slog.Logger

  mu          sync.RWMutex
  writeMu     sync.Mutex
  apps        map[string]*AppRegistration
  conn        *websocket.Conn
  connected   bool
  currentUser string
}

func main() {
  args := os.Args[1:]
  if len(args) > 0 {
    switch args[0] {
    case "version", "--version", "-version", "-v":
      fmt.Println(buildinfo.Summary("reverse-client"))
      return
    case "help", "--help", "-h":
      printUsage(os.Stdout)
      return
    }
  }
  if len(args) > 0 && args[0] != "daemon" {
    os.Exit(runCLI(args))
  }
  runDaemon()
}

func printUsage(w io.Writer) {
  fmt.Fprintln(w, "usage:")
  fmt.Fprintln(w, "  reverse-client daemon")
  fmt.Fprintln(w, "  reverse-client expose --app <name> --target <url> [--public-port <port>]")
  fmt.Fprintln(w, "  reverse-client list")
  fmt.Fprintln(w, "  reverse-client version")
}

func runCLI(args []string) int {
  if len(args) == 0 {
    printUsage(os.Stderr)
    return 1
  }

  localAPI := env("REVERSE_CLIENT_API", "http://127.0.0.1:9901")
  switch args[0] {
  case "expose":
    app := ""
    target := ""
    publicPort := 0
    for i := 1; i < len(args); i++ {
      switch args[i] {
      case "--app":
        i++
        if i < len(args) {
          app = args[i]
        }
      case "--target":
        i++
        if i < len(args) {
          target = args[i]
        }
      case "--public-port":
        i++
        if i < len(args) {
          p, _ := strconv.Atoi(args[i])
          publicPort = p
        }
      }
    }
    if app == "" || target == "" {
      printUsage(os.Stderr)
      return 1
    }
    payload, _ := json.Marshal(map[string]any{"app_name": app, "target_url": target, "public_port": publicPort})
    resp, err := http.Post(localAPI+"/apps", "application/json", bytes.NewReader(payload))
    if err != nil {
      fmt.Fprintln(os.Stderr, err)
      return 1
    }
    defer resp.Body.Close()
    body, _ := io.ReadAll(resp.Body)
    if resp.StatusCode >= 300 {
      fmt.Fprintln(os.Stderr, string(body))
      return 1
    }
    fmt.Println(string(body))
    return 0
  case "list":
    resp, err := http.Get(localAPI + "/apps")
    if err != nil {
      fmt.Fprintln(os.Stderr, err)
      return 1
    }
    defer resp.Body.Close()
    body, _ := io.ReadAll(resp.Body)
    if resp.StatusCode >= 300 {
      fmt.Fprintln(os.Stderr, string(body))
      return 1
    }
    fmt.Println(string(body))
    return 0
  case "version":
    fmt.Println(buildinfo.Summary("reverse-client"))
    return 0
  case "help", "--help", "-h":
    printUsage(os.Stdout)
    return 0
  default:
    printUsage(os.Stderr)
    return 1
  }
}

func runDaemon() {
  cfg := Config{
    ServerURL:         env("REVERSE_SERVER_URL", "http://host.docker.internal:8080"),
    ClientKey:         env("REVERSE_CLIENT_KEY", ""),
    LocalAPIAddr:      env("REVERSE_CLIENT_LISTEN_ADDR", "127.0.0.1:9901"),
    StatePath:         env("REVERSE_CLIENT_STATE_PATH", "/tmp/portflare-client/state.json"),
    ReconnectDelay:    envDuration("REVERSE_CLIENT_RECONNECT_DELAY", time.Second),
    HTTPTimeout:       envDuration("REVERSE_CLIENT_HTTP_TIMEOUT", 60*time.Second),
    DiscoverEnabled:   envBool("REVERSE_CLIENT_DISCOVER", false),
    DiscoverInterval:  envDuration("REVERSE_CLIENT_DISCOVER_INTERVAL", 5*time.Second),
    DiscoverGrace:     envDuration("REVERSE_CLIENT_DISCOVER_GRACE", 10*time.Minute),
    DiscoverAllow:     mustParsePortRanges(env("REVERSE_CLIENT_DISCOVER_ALLOW", "")),
    DiscoverDeny:      mustParsePortRanges(env("REVERSE_CLIENT_DISCOVER_DENY", "22,2375,2376")),
    DiscoverNameByPort: mustParsePortNameMap(env("REVERSE_CLIENT_DISCOVER_NAMES", "")),
  }

  logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
  version, commit, buildDate := buildinfo.Effective()
  logger.Info("reverse client starting", "version", version, "commit", commit, "build_date", buildDate)
  if cfg.ClientKey != "" && !isValidClientKey(cfg.ClientKey) {
    logger.Error("invalid client key format", "message", "REVERSE_CLIENT_KEY must start with pf_")
    os.Exit(1)
  }
  svc := &Service{cfg: cfg, logger: logger, apps: map[string]*AppRegistration{}}
  if err := svc.loadState(); err != nil {
    logger.Error("failed to load client state", "error", err)
    os.Exit(1)
  }

  go func() {
    if err := svc.serveLocalAPI(); err != nil && !errors.Is(err, http.ErrServerClosed) {
      logger.Error("local api failed", "error", err)
      os.Exit(1)
    }
  }()

  ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
  defer stop()
  if cfg.DiscoverEnabled {
    go svc.runDiscovery(ctx)
  }
  svc.run(ctx)
}

func (s *Service) loadState() error {
  if err := os.MkdirAll(filepath.Dir(s.cfg.StatePath), 0o755); err != nil {
    return err
  }
  raw, err := os.ReadFile(s.cfg.StatePath)
  if err != nil {
    if errors.Is(err, os.ErrNotExist) {
      return s.saveState()
    }
    return err
  }

  var st clientState
  if err := json.Unmarshal(raw, &st); err != nil {
    return err
  }
  if st.Apps == nil {
    st.Apps = map[string]*AppRegistration{}
  }

  s.mu.Lock()
  s.apps = st.Apps
  s.mu.Unlock()
  return nil
}

func (s *Service) saveState() error {
  s.mu.RLock()
  st := clientState{Apps: map[string]*AppRegistration{}}
  for name, app := range s.apps {
    cp := *app
    st.Apps[name] = &cp
  }
  s.mu.RUnlock()

  raw, err := json.MarshalIndent(st, "", "  ")
  if err != nil {
    return err
  }
  tmp := s.cfg.StatePath + ".tmp"
  if err := os.WriteFile(tmp, raw, 0o600); err != nil {
    return err
  }
  return os.Rename(tmp, s.cfg.StatePath)
}

func (s *Service) serveLocalAPI() error {
  mux := http.NewServeMux()
  mux.HandleFunc("/apps", s.handleApps)
  mux.HandleFunc("/apps/", s.handleAppByName)
  mux.HandleFunc("/discovery/rescan", s.handleDiscoveryRescan)
  mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
    writeJSON(w, http.StatusOK, map[string]any{"ok": true})
  })
  s.logger.Info("reverse client api listening", "addr", s.cfg.LocalAPIAddr)
  return http.ListenAndServe(s.cfg.LocalAPIAddr, mux)
}

func (s *Service) handleApps(w http.ResponseWriter, r *http.Request) {
  switch r.Method {
  case http.MethodGet:
    sourceFilter := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("source")))
    s.mu.RLock()
    out := make([]*AppRegistration, 0, len(s.apps))
    manual := make([]*AppRegistration, 0)
    discovery := make([]*AppRegistration, 0)
    for _, app := range s.apps {
      cp := *app
      if sourceFilter != "" && cp.Source != sourceFilter {
        continue
      }
      out = append(out, &cp)
      if cp.Source == "discovery" {
        discovery = append(discovery, &cp)
      } else {
        manual = append(manual, &cp)
      }
    }
    connected := s.connected
    currentUser := s.currentUser
    s.mu.RUnlock()
    sort.Slice(out, func(i, j int) bool { return out[i].AppName < out[j].AppName })
    sort.Slice(manual, func(i, j int) bool { return manual[i].AppName < manual[j].AppName })
    sort.Slice(discovery, func(i, j int) bool { return discovery[i].AppName < discovery[j].AppName })
    writeJSON(w, http.StatusOK, map[string]any{"connected": connected, "user": currentUser, "apps": out, "manual_apps": manual, "discovery_apps": discovery})
  case http.MethodPost:
    body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
    if err != nil {
      writeError(w, http.StatusBadRequest, err.Error())
      return
    }
    var req AppRegistration
    if err := json.Unmarshal(body, &req); err != nil {
      writeError(w, http.StatusBadRequest, err.Error())
      return
    }
    req.AppName = slug(req.AppName)
    if req.AppName == "" || strings.TrimSpace(req.TargetURL) == "" {
      writeError(w, http.StatusBadRequest, "app_name and target_url are required")
      return
    }
    req.Source = "manual"
    req.DiscoveredPort = 0
    req.Offline = false
    if _, err := url.Parse(req.TargetURL); err != nil {
      writeError(w, http.StatusBadRequest, "invalid target_url")
      return
    }
    now := time.Now().UTC()
    s.mu.Lock()
    existing, ok := s.apps[req.AppName]
    if ok {
      existing.TargetURL = req.TargetURL
      if req.PublicPort > 0 {
        existing.PublicPort = req.PublicPort
      }
      existing.UpdatedAt = now
      req = *existing
    } else {
      req.CreatedAt = now
      req.UpdatedAt = now
      s.apps[req.AppName] = &req
    }
    conn := s.conn
    s.mu.Unlock()

    if err := s.saveState(); err != nil {
      writeError(w, http.StatusInternalServerError, err.Error())
      return
    }

    if conn != nil {
      _ = s.send(ConnectMessage{Type: "register", AppName: req.AppName, PublicPort: req.PublicPort})
    }

    writeJSON(w, http.StatusCreated, map[string]any{
      "app_name": req.AppName,
      "target_url": req.TargetURL,
      "public_port": req.PublicPort,
      "source": req.Source,
      "local_dashboard": "http://" + s.cfg.LocalAPIAddr + "/apps",
      "public_url_example": fmt.Sprintf("https://%s-<user-label>.reverse.example.test", req.AppName),
    })
  default:
    writeError(w, http.StatusMethodNotAllowed, "method not allowed")
  }
}

func (s *Service) handleAppByName(w http.ResponseWriter, r *http.Request) {
  appName := slug(strings.TrimPrefix(r.URL.Path, "/apps/"))
  if appName == "" {
    writeError(w, http.StatusBadRequest, "app name is required")
    return
  }

  switch r.Method {
  case http.MethodDelete:
    s.mu.Lock()
    app, ok := s.apps[appName]
    if !ok {
      s.mu.Unlock()
      writeError(w, http.StatusNotFound, "app not found")
      return
    }
    source := app.Source
    delete(s.apps, appName)
    s.mu.Unlock()

    if err := s.saveState(); err != nil {
      writeError(w, http.StatusInternalServerError, err.Error())
      return
    }
    writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "app_name": appName, "source": source})
  case http.MethodGet:
    s.mu.RLock()
    app, ok := s.apps[appName]
    s.mu.RUnlock()
    if !ok {
      writeError(w, http.StatusNotFound, "app not found")
      return
    }
    cp := *app
    writeJSON(w, http.StatusOK, cp)
  default:
    writeError(w, http.StatusMethodNotAllowed, "method not allowed")
  }
}

func (s *Service) handleDiscoveryRescan(w http.ResponseWriter, r *http.Request) {
  if r.Method != http.MethodPost {
    writeError(w, http.StatusMethodNotAllowed, "method not allowed")
    return
  }
  if !s.cfg.DiscoverEnabled {
    writeError(w, http.StatusBadRequest, "discovery is not enabled")
    return
  }
  s.refreshDiscovery()
  writeJSON(w, http.StatusOK, map[string]any{"rescanned": true})
}

func (s *Service) runDiscovery(ctx context.Context) {
  ticker := time.NewTicker(s.cfg.DiscoverInterval)
  defer ticker.Stop()

  s.refreshDiscovery()
  for {
    select {
    case <-ctx.Done():
      return
    case <-ticker.C:
      s.refreshDiscovery()
    }
  }
}

func (s *Service) refreshDiscovery() {
  candidates, err := discoverListeningHTTPCandidates(s.cfg.DiscoverAllow, s.cfg.DiscoverDeny, s.cfg.DiscoverNameByPort)
  if err != nil {
    s.logger.Warn("discovery scan failed", "error", err)
    return
  }

  now := time.Now().UTC()
  seen := map[int]struct{}{}
  for _, candidate := range candidates {
    port := candidate.Port
    appName := candidate.AppName
    targetURL := candidate.TargetURL
    seen[port] = struct{}{}

    s.mu.Lock()
    app, ok := s.apps[appName]
    if !ok {
      app = &AppRegistration{
        AppName:        appName,
        TargetURL:      targetURL,
        Source:         "discovery",
        DiscoveredPort: port,
        LastSeenAt:     now,
        CreatedAt:      now,
        UpdatedAt:      now,
      }
      s.apps[appName] = app
      s.mu.Unlock()
      _ = s.saveState()
      _ = s.sendIfConnected(ConnectMessage{Type: "register", AppName: appName})
      s.logger.Info("discovered app", "app", appName, "port", port)
      continue
    }

    changed := false
    if app.Source == "" {
      app.Source = "discovery"
      changed = true
    }
    if app.TargetURL != targetURL {
      app.TargetURL = targetURL
      changed = true
    }
    if app.DiscoveredPort != port {
      app.DiscoveredPort = port
      changed = true
    }
    if app.Offline {
      app.Offline = false
      changed = true
    }
    app.LastSeenAt = now
    app.UpdatedAt = now
    s.mu.Unlock()

    if changed {
      _ = s.saveState()
      _ = s.sendIfConnected(ConnectMessage{Type: "register", AppName: appName, PublicPort: app.PublicPort})
    }
  }

  s.mu.Lock()
  changed := false
  for _, app := range s.apps {
    if app.Source != "discovery" || app.DiscoveredPort == 0 {
      continue
    }
    if _, ok := seen[app.DiscoveredPort]; ok {
      continue
    }
    if app.LastSeenAt.IsZero() {
      app.LastSeenAt = now
    }
    if now.Sub(app.LastSeenAt) >= s.cfg.DiscoverGrace && !app.Offline {
      app.Offline = true
      app.UpdatedAt = now
      changed = true
      s.logger.Info("discovered app marked offline", "app", app.AppName, "port", app.DiscoveredPort)
    }
  }
  s.mu.Unlock()
  if changed {
    _ = s.saveState()
  }
}

func (s *Service) run(ctx context.Context) {
  for {
    if ctx.Err() != nil {
      return
    }
    if s.cfg.ClientKey == "" {
      s.logger.Warn("REVERSE_CLIENT_KEY is not set; waiting before retry")
      select {
      case <-ctx.Done():
        return
      case <-time.After(5 * time.Second):
      }
      continue
    }

    if err := s.connectAndServe(ctx); err != nil {
      s.logger.Error("connection failed", "error", err)
    }

    select {
    case <-ctx.Done():
      return
    case <-time.After(s.cfg.ReconnectDelay):
    }
  }
}

func (s *Service) connectAndServe(ctx context.Context) error {
  wsURL, err := toWebSocketURL(s.cfg.ServerURL)
  if err != nil {
    return err
  }
  wsURL.Path = "/connect"
  q := wsURL.Query()
  q.Set("key", s.cfg.ClientKey)
  wsURL.RawQuery = q.Encode()

  conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL.String(), nil)
  if err != nil {
    return err
  }
  defer conn.Close()

  s.mu.Lock()
  s.conn = conn
  s.connected = true
  apps := make([]*AppRegistration, 0, len(s.apps))
  for _, app := range s.apps {
    apps = append(apps, app)
  }
  s.mu.Unlock()

  for _, app := range apps {
    if err := s.send(ConnectMessage{Type: "register", AppName: app.AppName, PublicPort: app.PublicPort}); err != nil {
      return err
    }
  }

  for {
    var msg ConnectMessage
    if err := conn.ReadJSON(&msg); err != nil {
      s.mu.Lock()
      s.conn = nil
      s.connected = false
      s.currentUser = ""
      s.mu.Unlock()
      return err
    }
    switch msg.Type {
    case "hello":
      s.mu.Lock()
      s.currentUser = msg.UserName
      s.mu.Unlock()
      s.logger.Info("connected", "user", msg.UserName)
    case "register-ack":
      s.mu.Lock()
      if app, ok := s.apps[msg.AppName]; ok {
        app.Approved = msg.Approved
        if msg.PublicPort > 0 {
          app.PublicPort = msg.PublicPort
        }
        app.UpdatedAt = time.Now().UTC()
      }
      s.mu.Unlock()
      _ = s.saveState()
      s.logger.Info("app registration acknowledged", "app", msg.AppName, "approved", msg.Approved, "public_port", msg.PublicPort)
    case "request":
      go s.handleProxyRequest(msg)
    case "error":
      s.logger.Warn("server error", "message", msg.Error)
    }
  }
}

func (s *Service) handleProxyRequest(msg ConnectMessage) {
  s.mu.RLock()
  app, ok := s.apps[msg.AppName]
  s.mu.RUnlock()
  if !ok {
    _ = s.send(ConnectMessage{Type: "response", RequestID: msg.RequestID, Error: "app is not registered on this client"})
    return
  }
  if app.Offline {
    _ = s.send(ConnectMessage{Type: "response", RequestID: msg.RequestID, Error: "app is currently offline on this client"})
    return
  }

  reqURL, err := url.Parse(strings.TrimSpace(app.TargetURL))
  if err != nil {
    _ = s.send(ConnectMessage{Type: "response", RequestID: msg.RequestID, Error: "invalid target URL"})
    return
  }
  forwarded, err := url.Parse(msg.URL)
  if err != nil {
    _ = s.send(ConnectMessage{Type: "response", RequestID: msg.RequestID, Error: "invalid forwarded URL"})
    return
  }
  reqURL.Path = forwarded.Path
  reqURL.RawPath = forwarded.RawPath
  reqURL.RawQuery = forwarded.RawQuery

  body, err := base64.StdEncoding.DecodeString(msg.BodyBase64)
  if err != nil {
    _ = s.send(ConnectMessage{Type: "response", RequestID: msg.RequestID, Error: "invalid request body"})
    return
  }

  req, err := http.NewRequest(msg.Method, reqURL.String(), bytes.NewReader(body))
  if err != nil {
    _ = s.send(ConnectMessage{Type: "response", RequestID: msg.RequestID, Error: err.Error()})
    return
  }
  req.Header = make(http.Header, len(msg.Headers))
  for k, values := range msg.Headers {
    if strings.EqualFold(k, "host") || strings.EqualFold(k, "x-forwarded-host") {
      continue
    }
    for _, v := range values {
      req.Header.Add(k, v)
    }
  }
  req.Header.Set("X-Forwarded-Host", firstHeader(msg.Headers, "Host"))
  req.Header.Set("X-Forwarded-Proto", "https")
  req.Header.Set("X-Reverse-Helper-App", app.AppName)
  req.Header.Set("X-Reverse-Helper-Upstream", app.TargetURL)

  httpClient := &http.Client{Timeout: s.cfg.HTTPTimeout}
  resp, err := httpClient.Do(req)
  if err != nil {
    _ = s.send(ConnectMessage{Type: "response", RequestID: msg.RequestID, Error: err.Error()})
    return
  }
  defer resp.Body.Close()

  responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
  if err != nil {
    _ = s.send(ConnectMessage{Type: "response", RequestID: msg.RequestID, Error: err.Error()})
    return
  }

  _ = s.send(ConnectMessage{
    Type:       "response",
    RequestID:  msg.RequestID,
    StatusCode: resp.StatusCode,
    Headers:    cloneHeader(resp.Header),
    BodyBase64: base64.StdEncoding.EncodeToString(responseBody),
  })
}

func (s *Service) send(msg ConnectMessage) error {
  s.mu.RLock()
  conn := s.conn
  s.mu.RUnlock()
  if conn == nil {
    return errors.New("not connected")
  }
  s.writeMu.Lock()
  defer s.writeMu.Unlock()
  return conn.WriteJSON(msg)
}

func toWebSocketURL(raw string) (*url.URL, error) {
  parsed, err := url.Parse(strings.TrimSpace(raw))
  if err != nil {
    return nil, err
  }
  switch parsed.Scheme {
  case "http":
    parsed.Scheme = "ws"
  case "https":
    parsed.Scheme = "wss"
  case "ws", "wss":
  default:
    return nil, fmt.Errorf("unsupported scheme: %s", parsed.Scheme)
  }
  return parsed, nil
}

func cloneHeader(h http.Header) map[string][]string {
  out := make(map[string][]string, len(h))
  for k, values := range h {
    cp := make([]string, len(values))
    copy(cp, values)
    out[k] = cp
  }
  return out
}

func firstHeader(h map[string][]string, key string) string {
  for k, values := range h {
    if strings.EqualFold(k, key) && len(values) > 0 {
      return values[0]
    }
  }
  return ""
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
  w.Header().Set("Content-Type", "application/json")
  w.WriteHeader(status)
  _ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, msg string) {
  writeJSON(w, status, map[string]string{"error": msg})
}

func isValidClientKey(v string) bool {
  return strings.HasPrefix(strings.TrimSpace(v), "pf_")
}

func (s *Service) sendIfConnected(msg ConnectMessage) error {
  if err := s.send(msg); err != nil && !strings.Contains(strings.ToLower(err.Error()), "not connected") {
    return err
  }
  return nil
}

func env(key, fallback string) string {
  if v := strings.TrimSpace(os.Getenv(key)); v != "" {
    return v
  }
  return fallback
}

func envBool(key string, fallback bool) bool {
  raw := strings.TrimSpace(os.Getenv(key))
  if raw == "" {
    return fallback
  }
  parsed, err := strconv.ParseBool(raw)
  if err != nil {
    return fallback
  }
  return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
  raw := strings.TrimSpace(os.Getenv(key))
  if raw == "" {
    return fallback
  }
  parsed, err := time.ParseDuration(raw)
  if err != nil {
    return fallback
  }
  return parsed
}

func discoverListeningHTTPCandidates(allow, deny []portRange, names map[int]string) ([]discoverCandidate, error) {
  ports := map[int]struct{}{}
  for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
    entries, err := parseProcNetTCP(path)
    if err != nil {
      if errors.Is(err, os.ErrNotExist) {
        continue
      }
      return nil, err
    }
    for _, entry := range entries {
      if !entry.IsListen || !entry.IsHTTPCandidate {
        continue
      }
      if !portAllowed(entry.Port, allow) || portDenied(entry.Port, deny) {
        continue
      }
      ports[entry.Port] = struct{}{}
    }
  }
  ordered := make([]int, 0, len(ports))
  for port := range ports {
    ordered = append(ordered, port)
  }
  sort.Ints(ordered)

  out := make([]discoverCandidate, 0, len(ordered))
  for _, port := range ordered {
    appName := fmt.Sprintf("app-%d", port)
    if configured, ok := names[port]; ok && configured != "" {
      appName = configured
    }
    out = append(out, discoverCandidate{
      AppName:   appName,
      TargetURL: fmt.Sprintf("http://127.0.0.1:%d", port),
      Port:      port,
    })
  }
  return normalizeDiscoverCandidates(out), nil
}

func normalizeDiscoverCandidates(in []discoverCandidate) []discoverCandidate {
  out := make([]discoverCandidate, 0, len(in))
  for _, candidate := range in {
    candidate.AppName = slug(candidate.AppName)
    if candidate.AppName == "" {
      candidate.AppName = fmt.Sprintf("app-%d", candidate.Port)
    }
    out = append(out, candidate)
  }
  sort.Slice(out, func(i, j int) bool {
    if out[i].Port == out[j].Port {
      return out[i].AppName < out[j].AppName
    }
    return out[i].Port < out[j].Port
  })
  return out
}

type procNetEntry struct {
  Port            int
  IsListen        bool
  IsHTTPCandidate bool
}

func parseProcNetTCP(path string) ([]procNetEntry, error) {
  f, err := os.Open(path)
  if err != nil {
    return nil, err
  }
  defer f.Close()

  scanner := bufio.NewScanner(f)
  first := true
  entries := make([]procNetEntry, 0)
  for scanner.Scan() {
    line := strings.TrimSpace(scanner.Text())
    if first {
      first = false
      continue
    }
    fields := strings.Fields(line)
    if len(fields) < 4 {
      continue
    }
    localAddr := fields[1]
    state := fields[3]
    hostHex, portHex, ok := strings.Cut(localAddr, ":")
    if !ok {
      continue
    }
    port, err := strconv.ParseInt(portHex, 16, 32)
    if err != nil {
      continue
    }
    entries = append(entries, procNetEntry{
      Port:            int(port),
      IsListen:        state == "0A",
      IsHTTPCandidate: isLocalHostHex(hostHex),
    })
  }
  if err := scanner.Err(); err != nil {
    return nil, err
  }
  return entries, nil
}

func isLocalHostHex(v string) bool {
  v = strings.ToUpper(strings.TrimSpace(v))
  if v == "00000000" || v == "00000000000000000000000000000000" {
    return true
  }
  if v == "0100007F" || v == "0000000000000000000000000100007F" {
    return true
  }
  if strings.HasSuffix(v, "00000000000000000000FFFF0100007F") {
    return true
  }
  return false
}

func mustParsePortRanges(raw string) []portRange {
  ranges, err := parsePortRanges(raw)
  if err != nil {
    panic(err)
  }
  return ranges
}

func mustParsePortNameMap(raw string) map[int]string {
  names, err := parsePortNameMap(raw)
  if err != nil {
    panic(err)
  }
  return names
}

func parsePortRanges(raw string) ([]portRange, error) {
  raw = strings.TrimSpace(raw)
  if raw == "" {
    return nil, nil
  }
  parts := strings.Split(raw, ",")
  out := make([]portRange, 0, len(parts))
  for _, part := range parts {
    part = strings.TrimSpace(part)
    if part == "" {
      continue
    }
    if strings.Contains(part, "-") {
      startRaw, endRaw, ok := strings.Cut(part, "-")
      if !ok {
        return nil, fmt.Errorf("invalid port range %q", part)
      }
      start, err := strconv.Atoi(strings.TrimSpace(startRaw))
      if err != nil {
        return nil, fmt.Errorf("invalid port range %q", part)
      }
      end, err := strconv.Atoi(strings.TrimSpace(endRaw))
      if err != nil {
        return nil, fmt.Errorf("invalid port range %q", part)
      }
      if start <= 0 || end <= 0 || end < start {
        return nil, fmt.Errorf("invalid port range %q", part)
      }
      out = append(out, portRange{Start: start, End: end})
      continue
    }
    port, err := strconv.Atoi(part)
    if err != nil || port <= 0 {
      return nil, fmt.Errorf("invalid port %q", part)
    }
    out = append(out, portRange{Start: port, End: port})
  }
  return out, nil
}

func parsePortNameMap(raw string) (map[int]string, error) {
  raw = strings.TrimSpace(raw)
  if raw == "" {
    return map[int]string{}, nil
  }
  out := map[int]string{}
  for _, part := range strings.Split(raw, ",") {
    part = strings.TrimSpace(part)
    if part == "" {
      continue
    }
    portRaw, nameRaw, ok := strings.Cut(part, "=")
    if !ok {
      return nil, fmt.Errorf("invalid port name mapping %q", part)
    }
    port, err := strconv.Atoi(strings.TrimSpace(portRaw))
    if err != nil || port <= 0 {
      return nil, fmt.Errorf("invalid port in mapping %q", part)
    }
    name := slug(nameRaw)
    if name == "" {
      return nil, fmt.Errorf("invalid app name in mapping %q", part)
    }
    out[port] = name
  }
  return out, nil
}

func portAllowed(port int, allow []portRange) bool {
  if len(allow) == 0 {
    return true
  }
  for _, r := range allow {
    if port >= r.Start && port <= r.End {
      return true
    }
  }
  return false
}

func portDenied(port int, deny []portRange) bool {
  for _, r := range deny {
    if port >= r.Start && port <= r.End {
      return true
    }
  }
  return false
}

func slug(v string) string {
  v = strings.ToLower(strings.TrimSpace(v))
  if v == "" {
    return ""
  }
  var b strings.Builder
  dash := false
  for _, r := range v {
    valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
    if valid {
      b.WriteRune(r)
      dash = false
      continue
    }
    if !dash {
      b.WriteByte('-')
      dash = true
    }
  }
  return strings.Trim(b.String(), "-")
}
