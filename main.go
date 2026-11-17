package main

import (
	"context"
	"crypto/tls"
	"embed"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

//go:embed all:ui
var uiFS embed.FS

//go:embed schema/*.json
var schemaFS embed.FS

// App holds the shared pieces every listener uses.
type App struct {
	cfg     *StaticConfig
	store   *Store
	db      *DB
	sink    *LogSink
	hub     *Hub
	metrics *Metrics
	guard   *Guard
	outbox  *Outbox
	forward *Forwarder
	proxy   *ForwardProxy
	inbound *Inbound
	admin   *Admin
	started time.Time
	tracing bool
	env     hostEnv
	notes   []DeployNote
}

const usage = `fluid-agent — forward proxy, webhook relay and callback ingest for IP-allowlisted servers

Usage:
  fluid-agent [serve] [-config config.json]   run the agent (default)
  fluid-agent check   [-config config.json]   validate config.json and every dynamic file
  fluid-agent logs    [flags]                 query the SQLite log store (fluid-agent logs -h)
  fluid-agent healthcheck [-config ...]       exit 0 if the local agent is live (for Docker)
  fluid-agent version                         print the version
`

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "check":
		err = cmdCheck(args, os.Stdout)
	case "logs":
		err = cmdLogs(args, os.Stdout)
	case "healthcheck":
		err = cmdHealthcheck(args)
	case "version":
		fmt.Println(version)
	case "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "config.json", "static config file")
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.Parse(args)
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	cfg, err := loadStatic(*cfgPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	app, err := NewApp(ctx, cfg)
	if err != nil {
		return err
	}
	return app.Serve(ctx)
}

// NewApp opens the database, loads config and builds every component.
func NewApp(ctx context.Context, cfg *StaticConfig) (*App, error) {
	app := &App{cfg: cfg, hub: NewHub(), metrics: NewMetrics(), started: time.Now()}
	app.metrics.registerHelp()
	stdout := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(stdout))

	db, err := OpenDB(cfg.Database.Path)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	app.db = db
	app.sink = NewLogSink(db, levelByName[strings.ToLower(cfg.Database.LogLevel)], app.hub)
	slog.SetDefault(slog.New(NewTeeHandler(stdout, app.sink)))

	store, err := NewStore(cfg.DynamicDir)
	if err != nil {
		return nil, fmt.Errorf("dynamic config: %w", err)
	}
	store.onReload = func(ok bool) {
		// Pooled keep-alive connections skip the dial-time guard, so drop them
		// whenever rules change; revoked destinations stop working at once.
		if app.proxy != nil {
			app.proxy.tr.CloseIdleConnections()
			app.forward.client.CloseIdleConnections()
		}
		res := "ok"
		if !ok {
			res = "error"
		}
		app.metrics.Inc("fluid_config_reloads_total", "result", res)
	}
	app.store = store
	app.guard = &Guard{cfg: store.Proxy, metrics: app.metrics}
	app.outbox = NewOutbox(app)
	app.forward = NewForwarder(app)
	app.proxy = NewForwardProxy(app)
	app.inbound = NewInbound(app)
	app.admin = &Admin{app: app}

	app.metrics.Gauge("fluid_active_tunnels", "Open CONNECT tunnels.", func() float64 { return float64(app.proxy.tunnels.Load()) })
	app.metrics.Gauge("fluid_log_rows_dropped", "Log rows dropped because the write queue was full.", func() float64 { return float64(app.sink.Dropped()) })
	app.metrics.Gauge("fluid_outbox_pending", "Deliveries waiting to be sent.", func() float64 {
		c := app.outbox.Counts(context.Background())
		return float64(c["pending"] + c["inflight"])
	})
	app.metrics.Gauge("fluid_outbox_dead", "Deliveries that gave up (dead-lettered).", func() float64 {
		return float64(app.outbox.Counts(context.Background())["dead"])
	})
	app.metrics.Gauge("fluid_config_errors", "Dynamic config files that currently fail validation.", func() float64 {
		return float64(len(app.store.Errors()))
	})
	if err := app.outbox.Recover(ctx); err != nil {
		return nil, fmt.Errorf("outbox recovery: %w", err)
	}
	return app, nil
}

func (app *App) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := app.db.PingContext(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "error": "database: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ready": true})
}

// Serve runs all listeners and background loops until ctx is cancelled.
func (app *App) Serve(ctx context.Context) error {
	cfg := app.cfg
	shutdownTracing, err := setupTracing(ctx, cfg.Telemetry)
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	app.tracing = cfg.Telemetry.OTLPEndpoint != "" || os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
	app.env = detectEnv()
	app.notes = deploymentNotes(cfg, app.store.snap.Load(), app.env, len(os.Getenv("PROXY_ADMIN_TOKEN")) >= 16, time.Now())
	logDeploymentNotes(app.notes)

	bg, cancelBG := context.WithCancel(context.Background())
	var bgWG sync.WaitGroup
	stopWatch := make(chan struct{})
	bgWG.Add(3)
	go func() { defer bgWG.Done(); app.store.Watch(cfg.ReloadInterval.D(), stopWatch) }()
	go func() { defer bgWG.Done(); app.outbox.Run(bg) }()
	go func() { defer bgWG.Done(); app.housekeeping(bg) }()

	running, err := app.openListeners()
	if err != nil {
		cancelBG()
		return err
	}
	errc := make(chan error, len(running))
	for _, rs := range running {
		slog.Info("listening", "kind", "system", "server", rs.name, "addr", rs.ln.Addr().String(), "scheme", rs.scheme)
		go func(rs runningServer) {
			if err := rs.srv.Serve(rs.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s: %w", rs.name, err)
			}
		}(rs)
	}
	slog.Info("fluid-agent started", "kind", "system", "version", version)

	select {
	case <-ctx.Done():
	case err = <-errc:
		slog.Error("server failed", "kind", "system", "err", err)
	}
	slog.Info("shutting down", "kind", "system")
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, rs := range running {
		rs.srv.Shutdown(sctx)
	}
	close(stopWatch)
	cancelBG()
	bgWG.Wait()
	shutdownTracing(sctx)
	app.sink.Close()
	app.db.Close()
	return err
}

// housekeeping prunes old rows and idle rate-limit buckets.
func (app *App) housekeeping(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	sweep := time.NewTicker(5 * time.Minute)
	defer sweep.Stop()
	prune := func() {
		n, err := Prune(ctx, app.db, time.Duration(app.cfg.Database.RetentionDays)*24*time.Hour)
		if err != nil {
			slog.Error("prune failed", "kind", "system", "err", err)
		} else if n > 0 {
			slog.Info("pruned old rows", "kind", "system", "rows", n)
		}
	}
	prune()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			prune()
		case <-sweep.C:
			app.inbound.limiter.Sweep(10 * time.Minute)
		}
	}
}

// ---------- check ----------

func cmdCheck(args []string, out *os.File) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	cfgPath := fs.String("config", "config.json", "static config file")
	fs.Parse(args)
	problems := checkConfig(*cfgPath)
	for _, p := range problems {
		fmt.Fprintln(out, p)
	}
	if cfg, err := loadStatic(*cfgPath); err == nil {
		if _, err := os.Stat(cfg.DynamicDir); err == nil {
			ns := deploymentNotes(cfg, loadDir(cfg.DynamicDir, nil), detectEnv(), len(os.Getenv("PROXY_ADMIN_TOKEN")) >= 16, time.Now())
			fmt.Fprintln(out, "\nDeployment notes:")
			for _, n := range ns {
				fmt.Fprintf(out, "  %-5s [%s] %s\n", strings.ToUpper(n.Level), n.Topic, n.Message)
			}
		}
	}
	if len(problems) == 0 {
		fmt.Fprintln(out, "\nOK: all config files are valid")
		return nil
	}
	return fmt.Errorf("%d invalid file(s)", len(problems))
}

// checkConfig validates the static file and every dynamic file without
// touching the database; it returns "path: problem" lines.
func checkConfig(cfgPath string) []string {
	cfg, err := loadStatic(cfgPath)
	if err != nil {
		return []string{err.Error()}
	}
	if _, err := os.Stat(cfg.DynamicDir); err != nil {
		return []string{fmt.Sprintf("%s: %v", cfg.DynamicDir, err)}
	}
	snap := loadDir(cfg.DynamicDir, nil)
	var out []string
	for f, e := range snap.errors {
		out = append(out, f+": "+e)
	}
	sort.Strings(out)
	return out
}

// ---------- healthcheck ----------

func cmdHealthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	cfgPath := fs.String("config", "config.json", "static config file (to find the webhook port)")
	url := fs.String("url", "", "URL to probe (default http://127.0.0.1:<webhook port>/livez)")
	fs.Parse(args)
	target := *url
	if target == "" {
		addr := defaultStatic().WebhookAddr
		if cfg, err := loadStatic(*cfgPath); err == nil {
			addr = cfg.WebhookAddr
		}
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return err
		}
		target = "http://127.0.0.1:" + port + "/livez"
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(target)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", target, resp.Status)
	}
	return nil
}

// runningServer is a bound listener with the server that will serve it.
type runningServer struct {
	name   string
	scheme string
	srv    *http.Server
	ln     net.Listener
}

// Handlers with base paths applied, as served (also used by tests).
func (app *App) webhookHandler() http.Handler {
	return stripBasePath(app.cfg.WebhookBasePath, app.inbound.Handler())
}

func (app *App) adminHandler() http.Handler {
	return stripBasePath(app.cfg.AdminBasePath, app.admin.Handler())
}

// openListeners binds every listener (with TLS where configured) plus any
// ACME HTTP-01 challenge listeners. Nothing is served until Serve runs them.
func (app *App) openListeners() ([]runningServer, error) {
	cfg := app.cfg
	acmeCache := filepath.Join(filepath.Dir(cfg.Database.Path), "acme")
	specs := []struct {
		name string
		srv  *http.Server
		tls  *TLSConfig
		h1   bool
	}{
		// The proxy strips its base path itself: absolute-URL proxy requests
		// carry the destination's path, which must never be rewritten.
		{"proxy", &http.Server{Addr: cfg.ProxyAddr, Handler: app.proxy, ReadHeaderTimeout: 10 * time.Second}, cfg.ProxyTLS, true},
		{"webhook", &http.Server{Addr: cfg.WebhookAddr, Handler: app.webhookHandler(), ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}, cfg.WebhookTLS, false},
		{"admin", &http.Server{Addr: cfg.AdminAddr, Handler: app.adminHandler(), ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}, cfg.AdminTLS, false},
	}
	var out []runningServer
	fail := func(err error) ([]runningServer, error) {
		for _, rs := range out {
			rs.ln.Close()
		}
		return nil, err
	}
	for _, s := range specs {
		ts, err := buildTLS(s.tls, acmeCache, s.h1)
		if err != nil {
			return fail(fmt.Errorf("%s TLS: %w", s.name, err))
		}
		scheme := "http"
		if ts != nil {
			scheme = "https"
			s.srv.TLSConfig = ts.config
			if s.h1 { // keep the proxy on HTTP/1.1 so CONNECT can hijack the connection
				s.srv.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
			}
		}
		ln, err := listen(s.srv.Addr, ts)
		if err != nil {
			if errors.Is(err, syscall.EACCES) {
				err = fmt.Errorf("%w (ports below 1024 need CAP_NET_BIND_SERVICE or root; see deploy/systemd)", err)
			}
			return fail(fmt.Errorf("%s listener: %w", s.name, err))
		}
		out = append(out, runningServer{s.name, scheme, s.srv, ln})
		if ts != nil && ts.httpHandler != nil {
			cl, err := net.Listen("tcp", ts.httpAddr)
			if err != nil {
				return fail(fmt.Errorf("%s ACME challenge listener: %w", s.name, err))
			}
			ch := &http.Server{Handler: ts.httpHandler, ReadHeaderTimeout: 10 * time.Second}
			out = append(out, runningServer{s.name + "-acme-http", "http", ch, cl})
		}
	}
	return out, nil
}
