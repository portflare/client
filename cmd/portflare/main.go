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

	protocoltypes "github.com/portflare/protocol/types"
	protocolvalidation "github.com/portflare/protocol/validation"

	"github.com/portflare/client/internal/buildinfo"
)

type Config struct {
	ServerURL        string
	ClientKey        string
	LocalAPIAddr     string
	StatePath        string
	ReconnectDelay   time.Duration
	HTTPTimeout      time.Duration
	DiscoverEnabled  bool
	DiscoverInterval time.Duration
	DiscoverGrace    time.Duration
	DiscoverAllow    []portRange
	DiscoverDeny     []portRange
	DiscoverNaming   DiscoveryNamingConfig
}

type discoveryNameTemplate string

const (
	discoveryNameTemplateAppPort             discoveryNameTemplate = "app-port"
	discoveryNameTemplateDescriptorPort      discoveryNameTemplate = "descriptor-port"
	discoveryNameTemplateDescriptorProtoPort discoveryNameTemplate = "descriptor-proto-port"
	discoveryNameTemplateDescriptorProto     discoveryNameTemplate = "descriptor-proto"
)

type DiscoveryNamingConfig struct {
	ExactNameByPort map[int]string
	ProtocolByPort  map[int]string
	Descriptor      string
	NameTemplate    discoveryNameTemplate
}

type AppRegistration = protocoltypes.AppRegistration

type ConnectMessage = protocoltypes.ConnectMessage

type RegistrationResponse struct {
	UserName        string `json:"user_name"`
	PublicUserLabel string `json:"public_user_label"`
	Email           string `json:"email,omitempty"`
	APIKey          string `json:"api_key"`
}

type clientState struct {
	Apps map[string]*AppRegistration `json:"apps"`
}

type discoverCandidate struct {
	AppName   string
	TargetURL string
	Port      int
	Protocol  string
}

type portRange struct {
	Start int
	End   int
}

type clientStats struct {
	StartedAt              time.Time `json:"started_at"`
	LastServerConnectAt    time.Time `json:"last_server_connect_at,omitempty"`
	LastServerDisconnectAt time.Time `json:"last_server_disconnect_at,omitempty"`
	LastRequestAt          time.Time `json:"last_request_at,omitempty"`
	ServerConnects         uint64    `json:"server_connects"`
	ServerDisconnects      uint64    `json:"server_disconnects"`
	RequestsTotal          uint64    `json:"requests_total"`
	RequestsSucceeded      uint64    `json:"requests_succeeded"`
	RequestsFailed         uint64    `json:"requests_failed"`
	BytesIn                uint64    `json:"bytes_in"`
	BytesOut               uint64    `json:"bytes_out"`
}

type Service struct {
	cfg    Config
	logger *slog.Logger

	mu          sync.RWMutex
	writeMu     sync.Mutex
	statsMu     sync.Mutex
	apps        map[string]*AppRegistration
	conn        *websocket.Conn
	connected   bool
	currentUser string
	stats       clientStats
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "version", "--version", "-version", "-v":
			fmt.Println(buildinfo.Summary("portflare"))
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
	fmt.Fprintln(w, "  portflare daemon")
	fmt.Fprintln(w, "  portflare register --server <url> --user <name> [--email <email>]")
	fmt.Fprintln(w, "  portflare expose --app <name> --target <url> [--public-port <port>]")
	fmt.Fprintln(w, "  portflare list")
	fmt.Fprintln(w, "  portflare version")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "discovery naming env:")
	fmt.Fprintln(w, "  PORTFLARE_CLIENT_DISCOVER_DESCRIPTOR=<descriptor>")
	fmt.Fprintln(w, "  PORTFLARE_CLIENT_DISCOVER_NAME_TEMPLATE=app-port|descriptor-port|descriptor-proto-port|descriptor-proto")
	fmt.Fprintln(w, "  PORTFLARE_CLIENT_DISCOVER_PROTOCOLS=3000=http,6379=redis")
}

func runCLI(args []string) int {
	if len(args) == 0 {
		printUsage(os.Stderr)
		return 1
	}

	localAPI := env("PORTFLARE_CLIENT_API", "http://127.0.0.1:9901")
	switch args[0] {
	case "register":
		serverURL := strings.TrimRight(env("PORTFLARE_SERVER_URL", "http://host.docker.internal:8080"), "/")
		userName := ""
		email := ""
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--server":
				i++
				if i < len(args) {
					serverURL = strings.TrimRight(args[i], "/")
				}
			case "--user":
				i++
				if i < len(args) {
					userName = args[i]
				}
			case "--email":
				i++
				if i < len(args) {
					email = args[i]
				}
			}
		}
		if serverURL == "" || userName == "" {
			printUsage(os.Stderr)
			return 1
		}
		payload, _ := json.Marshal(map[string]string{"user_name": userName, "email": email})
		resp, err := http.Post(serverURL+"/api/register", "application/json", bytes.NewReader(payload))
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
		var registration RegistrationResponse
		if err := json.Unmarshal(body, &registration); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("registered user %s\n", registration.UserName)
		fmt.Printf("public label: %s\n", registration.PublicUserLabel)
		fmt.Printf("client key: %s\n", registration.APIKey)
		fmt.Printf("\nexport PORTFLARE_SERVER_URL=%q\n", serverURL)
		fmt.Printf("export PORTFLARE_CLIENT_KEY=%q\n", registration.APIKey)
		return 0
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
		fmt.Println(buildinfo.Summary("portflare"))
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
		ServerURL:        env("PORTFLARE_SERVER_URL", "http://host.docker.internal:8080"),
		ClientKey:        env("PORTFLARE_CLIENT_KEY", ""),
		LocalAPIAddr:     env("PORTFLARE_CLIENT_LISTEN_ADDR", "127.0.0.1:9901"),
		StatePath:        env("PORTFLARE_CLIENT_STATE_PATH", "/tmp/portflare-client/state.json"),
		ReconnectDelay:   envDuration("PORTFLARE_CLIENT_RECONNECT_DELAY", time.Second),
		HTTPTimeout:      envDuration("PORTFLARE_CLIENT_HTTP_TIMEOUT", 60*time.Second),
		DiscoverEnabled:  envBool("PORTFLARE_CLIENT_DISCOVER", false),
		DiscoverInterval: envDuration("PORTFLARE_CLIENT_DISCOVER_INTERVAL", 5*time.Second),
		DiscoverGrace:    envDuration("PORTFLARE_CLIENT_DISCOVER_GRACE", 10*time.Minute),
		DiscoverAllow:    mustParsePortRanges(env("PORTFLARE_CLIENT_DISCOVER_ALLOW", "")),
		DiscoverDeny:     mustParsePortRanges(env("PORTFLARE_CLIENT_DISCOVER_DENY", "22,2375,2376")),
		DiscoverNaming:   mustLoadDiscoveryNamingConfig(),
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	version, commit, buildDate := buildinfo.Effective()
	logger.Info("portflare starting", "version", version, "commit", commit, "build_date", buildDate)
	if cfg.ClientKey != "" && !protocolvalidation.IsValidClientKey(strings.TrimSpace(cfg.ClientKey)) {
		logger.Error("invalid client key format", "message", "PORTFLARE_CLIENT_KEY must start with pf_")
		os.Exit(1)
	}
	svc := &Service{cfg: cfg, logger: logger, apps: map[string]*AppRegistration{}, stats: clientStats{StartedAt: time.Now().UTC()}}
	if err := svc.loadState(); err != nil {
		logger.Error("failed to load client state", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		sig := <-signals
		logger.Info("shutdown signal received", "signal", sig.String())
		cancel()
		sig = <-signals
		logger.Warn("second shutdown signal received; exiting immediately", "signal", sig.String())
		os.Exit(1)
	}()

	go func() {
		if err := svc.serveLocalAPI(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("local api failed", "error", err)
			cancel()
		}
	}()

	if cfg.DiscoverEnabled {
		go svc.runDiscovery(ctx)
	}
	go svc.logStats(ctx)
	svc.run(ctx)
	logger.Info("portflare stopped")
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

func (s *Service) serveLocalAPI(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/apps", s.handleApps)
	mux.HandleFunc("/apps/", s.handleAppByName)
	mux.HandleFunc("/stats", s.handleStats)
	mux.HandleFunc("/discovery/rescan", s.handleDiscoveryRescan)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("/readyz", handleReadyz("portflare"))

	server := &http.Server{Addr: s.cfg.LocalAPIAddr, Handler: mux}
	go func() {
		<-ctx.Done()
		s.logger.Info("shutting down portflare api")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			s.logger.Warn("portflare api shutdown failed", "error", err)
			_ = server.Close()
			return
		}
		s.logger.Info("portflare api stopped")
	}()

	s.logger.Info("portflare api listening", "addr", s.cfg.LocalAPIAddr)
	return server.ListenAndServe()
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
			_ = s.send(ConnectMessage{Type: protocoltypes.MessageTypeRegister, AppName: req.AppName, PublicPort: req.PublicPort})
		}

		writeJSON(w, http.StatusCreated, map[string]any{
			"app_name":           req.AppName,
			"target_url":         req.TargetURL,
			"public_port":        req.PublicPort,
			"source":             req.Source,
			"local_dashboard":    "http://" + s.cfg.LocalAPIAddr + "/apps",
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

func (s *Service) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	s.mu.RLock()
	connected := s.connected
	currentUser := s.currentUser
	appCount := len(s.apps)
	s.mu.RUnlock()

	stats := s.snapshotStats()
	writeJSON(w, http.StatusOK, map[string]any{"connected": connected, "user": currentUser, "apps": appCount, "stats": stats})
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
	candidates, err := discoverListeningHTTPCandidates(s.cfg.DiscoverAllow, s.cfg.DiscoverDeny, s.cfg.DiscoverNaming)
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
			_ = s.sendIfConnected(ConnectMessage{Type: protocoltypes.MessageTypeRegister, AppName: appName})
			s.logger.Info("discovered app", "app", appName, "port", port, "protocol", candidate.Protocol, "discovery_template", s.cfg.DiscoverNaming.NameTemplate, "discovery_descriptor", s.cfg.DiscoverNaming.Descriptor)
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
			_ = s.sendIfConnected(ConnectMessage{Type: protocoltypes.MessageTypeRegister, AppName: appName, PublicPort: app.PublicPort})
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

func (s *Service) logStats(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.RLock()
			connected := s.connected
			user := s.currentUser
			appCount := len(s.apps)
			s.mu.RUnlock()
			stats := s.snapshotStats()
			s.logger.Info("portflare stats", "connected", connected, "user", user, "apps", appCount, "server_connects", stats.ServerConnects, "server_disconnects", stats.ServerDisconnects, "requests_total", stats.RequestsTotal, "requests_succeeded", stats.RequestsSucceeded, "requests_failed", stats.RequestsFailed, "bytes_in", stats.BytesIn, "bytes_out", stats.BytesOut)
		}
	}
}

func (s *Service) snapshotStats() clientStats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.stats
}

func (s *Service) recordServerConnected() {
	s.statsMu.Lock()
	s.stats.ServerConnects++
	s.stats.LastServerConnectAt = time.Now().UTC()
	s.statsMu.Unlock()
}

func (s *Service) recordServerDisconnected() {
	s.statsMu.Lock()
	s.stats.ServerDisconnects++
	s.stats.LastServerDisconnectAt = time.Now().UTC()
	s.statsMu.Unlock()
}

func (s *Service) recordProxyRequest(statusCode int, bytesIn int, bytesOut int, failed bool) {
	_ = statusCode
	s.statsMu.Lock()
	s.stats.RequestsTotal++
	if failed {
		s.stats.RequestsFailed++
	} else {
		s.stats.RequestsSucceeded++
	}
	if bytesIn > 0 {
		s.stats.BytesIn += uint64(bytesIn)
	}
	if bytesOut > 0 {
		s.stats.BytesOut += uint64(bytesOut)
	}
	s.stats.LastRequestAt = time.Now().UTC()
	s.statsMu.Unlock()
}

func (s *Service) run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if s.cfg.ClientKey == "" {
			s.logger.Warn("PORTFLARE_CLIENT_KEY is not set; waiting before retry")
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		if err := s.connectAndServe(ctx); err != nil && ctx.Err() == nil {
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

	s.logger.Info("connecting to portflare server", "url", redactedURL(wsURL))
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL.String(), nil)
	if err != nil {
		return err
	}
	s.logger.Info("server websocket connected", "url", redactedURL(wsURL))
	if ctx.Err() != nil {
		_ = conn.Close()
		return nil
	}
	defer conn.Close()

	closeOnCancel := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			s.logger.Info("closing server connection")
			_ = conn.SetReadDeadline(time.Now())
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutdown"), time.Now().Add(time.Second))
			_ = conn.Close()
		case <-closeOnCancel:
		}
	}()
	defer close(closeOnCancel)

	s.mu.Lock()
	s.conn = conn
	s.connected = true
	apps := make([]*AppRegistration, 0, len(s.apps))
	for _, app := range s.apps {
		apps = append(apps, app)
	}
	s.mu.Unlock()
	s.recordServerConnected()
	s.logger.Info("registering apps with server", "apps", len(apps))

	for _, app := range apps {
		if ctx.Err() != nil {
			return nil
		}
		if err := s.send(ConnectMessage{Type: protocoltypes.MessageTypeRegister, AppName: app.AppName, PublicPort: app.PublicPort}); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}

	for {
		if ctx.Err() != nil {
			return nil
		}
		var msg ConnectMessage
		if err := conn.ReadJSON(&msg); err != nil {
			s.mu.Lock()
			s.conn = nil
			s.connected = false
			s.currentUser = ""
			s.mu.Unlock()
			s.recordServerDisconnected()
			s.logger.Warn("server websocket disconnected", "error", err)
			if ctx.Err() != nil || websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return nil
			}
			return err
		}
		switch msg.Type {
		case "hello":
			s.mu.Lock()
			s.currentUser = msg.UserName
			s.mu.Unlock()
			s.logger.Info("connected", "user", msg.UserName)
		case protocoltypes.MessageTypeRegisterAck:
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
	started := time.Now()
	bodyBytes := base64.StdEncoding.DecodedLen(len(msg.BodyBase64))
	s.logger.Info("proxy request received", "request_id", msg.RequestID, "app", msg.AppName, "method", msg.Method, "url", msg.URL, "body_bytes", bodyBytes)

	s.mu.RLock()
	app, ok := s.apps[msg.AppName]
	s.mu.RUnlock()
	if !ok {
		s.respondProxyError(msg.RequestID, msg.AppName, msg.Method, msg.URL, started, bodyBytes, "app is not registered on this client")
		return
	}
	if app.Offline {
		s.respondProxyError(msg.RequestID, msg.AppName, msg.Method, msg.URL, started, bodyBytes, "app is currently offline on this client")
		return
	}

	reqURL, err := url.Parse(strings.TrimSpace(app.TargetURL))
	if err != nil {
		s.respondProxyError(msg.RequestID, msg.AppName, msg.Method, msg.URL, started, bodyBytes, "invalid target URL")
		return
	}
	forwarded, err := url.Parse(msg.URL)
	if err != nil {
		s.respondProxyError(msg.RequestID, msg.AppName, msg.Method, msg.URL, started, bodyBytes, "invalid forwarded URL")
		return
	}
	reqURL.Path = forwarded.Path
	reqURL.RawPath = forwarded.RawPath
	reqURL.RawQuery = forwarded.RawQuery

	body, err := base64.StdEncoding.DecodeString(msg.BodyBase64)
	if err != nil {
		s.respondProxyError(msg.RequestID, msg.AppName, msg.Method, msg.URL, started, bodyBytes, "invalid request body")
		return
	}
	bodyBytes = len(body)

	req, err := http.NewRequest(msg.Method, reqURL.String(), bytes.NewReader(body))
	if err != nil {
		s.respondProxyError(msg.RequestID, msg.AppName, msg.Method, msg.URL, started, bodyBytes, err.Error())
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
		s.respondProxyError(msg.RequestID, msg.AppName, msg.Method, msg.URL, started, bodyBytes, err.Error())
		return
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		s.respondProxyError(msg.RequestID, msg.AppName, msg.Method, msg.URL, started, bodyBytes, err.Error())
		return
	}

	err = s.send(ConnectMessage{
		Type:       protocoltypes.MessageTypeResponse,
		RequestID:  msg.RequestID,
		StatusCode: resp.StatusCode,
		Headers:    cloneHeader(resp.Header),
		BodyBase64: base64.StdEncoding.EncodeToString(responseBody),
	})
	if err != nil {
		s.recordProxyRequest(resp.StatusCode, bodyBytes, len(responseBody), true)
		s.logger.Warn("proxy response send failed", "request_id", msg.RequestID, "app", msg.AppName, "method", msg.Method, "url", msg.URL, "status", resp.StatusCode, "duration_ms", time.Since(started).Milliseconds(), "error", err)
		return
	}
	s.recordProxyRequest(resp.StatusCode, bodyBytes, len(responseBody), false)
	s.logger.Info("proxy request completed", "request_id", msg.RequestID, "app", msg.AppName, "method", msg.Method, "url", msg.URL, "target", reqURL.String(), "status", resp.StatusCode, "duration_ms", time.Since(started).Milliseconds(), "bytes_in", bodyBytes, "bytes_out", len(responseBody))
}

func (s *Service) respondProxyError(requestID, appName, method, requestURL string, started time.Time, bytesIn int, message string) {
	_ = s.send(ConnectMessage{Type: protocoltypes.MessageTypeResponse, RequestID: requestID, Error: message})
	s.recordProxyRequest(0, bytesIn, 0, true)
	s.logger.Warn("proxy request failed", "request_id", requestID, "app", appName, "method", method, "url", requestURL, "duration_ms", time.Since(started).Milliseconds(), "error", message)
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

func redactedURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	cp := *u
	q := cp.Query()
	if q.Has("key") {
		q.Set("key", "redacted")
	}
	cp.RawQuery = q.Encode()
	return cp.String()
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

func handleReadyz(application string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		writeJSON(w, http.StatusOK, buildinfo.Ready(application))
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
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

func mustLoadDiscoveryNamingConfig() DiscoveryNamingConfig {
	template, err := parseDiscoveryNameTemplate(env("PORTFLARE_CLIENT_DISCOVER_NAME_TEMPLATE", ""))
	if err != nil {
		panic(err)
	}
	names, err := parsePortNameMap(env("PORTFLARE_CLIENT_DISCOVER_NAMES", ""))
	if err != nil {
		panic(err)
	}
	protocols, err := parsePortProtocolMap(env("PORTFLARE_CLIENT_DISCOVER_PROTOCOLS", ""))
	if err != nil {
		panic(err)
	}
	return DiscoveryNamingConfig{
		ExactNameByPort: names,
		ProtocolByPort:  protocols,
		Descriptor:      slug(env("PORTFLARE_CLIENT_DISCOVER_DESCRIPTOR", "")),
		NameTemplate:    template,
	}
}

func discoverListeningHTTPCandidates(allow, deny []portRange, naming DiscoveryNamingConfig) ([]discoverCandidate, error) {
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
	return buildDiscoverCandidates(ordered, naming), nil
}

type discoveryNameCandidate struct {
	discoverCandidate
	exact bool
}

func buildDiscoverCandidates(ports []int, naming DiscoveryNamingConfig) []discoverCandidate {
	ordered := append([]int(nil), ports...)
	sort.Ints(ordered)

	generated := make([]discoveryNameCandidate, 0, len(ordered))
	nameCounts := map[string]int{}
	exactNames := map[string]struct{}{}
	for _, port := range ordered {
		candidate, exact := discoveryCandidateForPort(port, naming)
		generated = append(generated, discoveryNameCandidate{discoverCandidate: candidate, exact: exact})
		nameCounts[candidate.AppName]++
		if exact {
			exactNames[candidate.AppName] = struct{}{}
		}
	}

	out := make([]discoverCandidate, 0, len(generated))
	usedNames := map[string]struct{}{}
	for name := range exactNames {
		usedNames[name] = struct{}{}
	}
	for _, candidate := range generated {
		if !candidate.exact {
			baseName := candidate.AppName
			if nameCounts[baseName] > 1 || nameIsUsed(candidate.AppName, usedNames) {
				candidate.AppName = slug(fmt.Sprintf("%s-%d", baseName, candidate.Port))
			}
			for suffix := 2; ; suffix++ {
				if !nameIsUsed(candidate.AppName, usedNames) {
					break
				}
				candidate.AppName = slug(fmt.Sprintf("%s-%d-%d", baseName, candidate.Port, suffix))
			}
			usedNames[candidate.AppName] = struct{}{}
		}
		out = append(out, candidate.discoverCandidate)
	}
	return normalizeDiscoverCandidates(out)
}

func nameIsUsed(name string, used map[string]struct{}) bool {
	_, exists := used[name]
	return exists
}

func discoveryCandidateForPort(port int, naming DiscoveryNamingConfig) (discoverCandidate, bool) {
	protocol := slug(naming.ProtocolByPort[port])
	appName := ""
	exact := false
	if configured, ok := naming.ExactNameByPort[port]; ok && configured != "" {
		appName = configured
		exact = true
	} else {
		appName = generatedDiscoveryName(port, protocol, naming)
	}
	appName = slug(appName)
	if appName == "" {
		appName = fmt.Sprintf("app-%d", port)
	}
	return discoverCandidate{
		AppName:   appName,
		TargetURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		Port:      port,
		Protocol:  protocol,
	}, exact
}

func generatedDiscoveryName(port int, protocol string, naming DiscoveryNamingConfig) string {
	descriptor := slug(naming.Descriptor)
	switch naming.NameTemplate {
	case discoveryNameTemplateDescriptorPort:
		if descriptor == "" {
			return fmt.Sprintf("app-%d", port)
		}
		return fmt.Sprintf("%s-%d", descriptor, port)
	case discoveryNameTemplateDescriptorProtoPort:
		if descriptor == "" {
			return fmt.Sprintf("app-%d", port)
		}
		if protocol == "" {
			return fmt.Sprintf("%s-%d", descriptor, port)
		}
		return fmt.Sprintf("%s-%s-%d", descriptor, protocol, port)
	case discoveryNameTemplateDescriptorProto:
		if descriptor == "" {
			return fmt.Sprintf("app-%d", port)
		}
		if protocol == "" {
			return fmt.Sprintf("%s-%d", descriptor, port)
		}
		return fmt.Sprintf("%s-%s", descriptor, protocol)
	default:
		return fmt.Sprintf("app-%d", port)
	}
}

func normalizeDiscoverCandidates(in []discoverCandidate) []discoverCandidate {
	out := make([]discoverCandidate, 0, len(in))
	for _, candidate := range in {
		candidate.AppName = slug(candidate.AppName)
		candidate.Protocol = slug(candidate.Protocol)
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

func parseDiscoveryNameTemplate(raw string) (discoveryNameTemplate, error) {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "", "port", "app-port":
		return discoveryNameTemplateAppPort, nil
	case string(discoveryNameTemplateDescriptorPort):
		return discoveryNameTemplateDescriptorPort, nil
	case string(discoveryNameTemplateDescriptorProtoPort):
		return discoveryNameTemplateDescriptorProtoPort, nil
	case string(discoveryNameTemplateDescriptorProto):
		return discoveryNameTemplateDescriptorProto, nil
	default:
		return "", fmt.Errorf("invalid discovery name template %q", raw)
	}
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
	nameByPort := map[string]int{}
	for port, name := range out {
		if existingPort, ok := nameByPort[name]; ok && existingPort != port {
			return nil, fmt.Errorf("duplicate app name %q in mapping for ports %d and %d", name, existingPort, port)
		}
		nameByPort[name] = port
	}
	return out, nil
}

func parsePortProtocolMap(raw string) (map[int]string, error) {
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
		portRaw, protocolRaw, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("invalid port protocol mapping %q", part)
		}
		port, err := strconv.Atoi(strings.TrimSpace(portRaw))
		if err != nil || port <= 0 {
			return nil, fmt.Errorf("invalid port in mapping %q", part)
		}
		protocol := slug(protocolRaw)
		if protocol == "" {
			return nil, fmt.Errorf("invalid protocol label in mapping %q", part)
		}
		out[port] = protocol
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
