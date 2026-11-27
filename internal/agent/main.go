package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/skpjr001/fluid-agent/internal/config"
	"github.com/skpjr001/fluid-agent/internal/database"
	"github.com/skpjr001/fluid-agent/internal/hooks"
	"github.com/skpjr001/fluid-agent/internal/hub"
	"github.com/skpjr001/fluid-agent/internal/metrics"
	"github.com/skpjr001/fluid-agent/internal/netguard"
	"github.com/skpjr001/fluid-agent/internal/telemetry"
	"github.com/skpjr001/fluid-agent/schema"
	"github.com/skpjr001/fluid-agent/ui"
)

// version is the build version, set by Main from cmd/fluid-agent.
var version = "dev"

var (
	uiFS     = ui.FS
	schemaFS = schema.FS
)

// App holds the shared pieces every listener uses.
type App struct {
	cfg     *config.StaticConfig
	store   *Store
	db      *database.DB
	sink    *LogSink
	hub     *hub.Hub
	metrics *metrics.Metrics
	guard   *netguard.Guard
	outbox  *Outbox
	forward *Forwarder
	proxy   *ForwardProxy
	inbound *Inbound
	admin   *Admin
	started time.Time
	tracing bool
	env     hostEnv
	notes   []DeployNote

	draining  atomic.Bool       // shutting down: /readyz answers 503
	addrs     map[string]string // listener name -> bound address; set before listening is closed
	listening chan struct{}     // closed once every listener accepts connections
	hooks     hooks.Hooks       // your code before/after startup and shutdown
}

const usage = `fluid-agent — forward proxy, webhook relay and callback ingest for IP-allowlisted servers

Usage:
  fluid-agent [serve] [-config config.json]   run the agent (default)
  fluid-agent init    [-config config.json]   create missing directories and config files (serve does this too)
  fluid-agent check   [-config config.json]   validate config.json and every dynamic file
  fluid-agent logs    [flags]                 query the SQLite log store (fluid-agent logs -h)
  fluid-agent healthcheck [-config ...]       exit 0 if the local agent is live (for Docker)
  fluid-agent version                         print the version
`

// lifecycle holds the hooks given to Main; serve runs them.
var lifecycle hooks.Hooks

// Main runs the fluid-agent command line; v is the build version and h the
// startup and shutdown hooks.
func Main(v string, h hooks.Hooks) {
	version, lifecycle = v, h
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "init":
		err = cmdInit(args, os.Stdout)
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
	existed := pathPresent(*cfgPath)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	created := bootstrap(*cfgPath, cfg, existed, len(os.Getenv("PROXY_ADMIN_TOKEN")) >= 16)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop() // restore default handling: a second signal ends the process at once
	}()
	app, err := NewApp(ctx, cfg)
	if err != nil {
		return err
	}
	logBootstrap(created)
	app.hooks = lifecycle
	return app.Serve(ctx)
}

// NewApp opens the database, loads config and builds every component.
func NewApp(ctx context.Context, cfg *config.StaticConfig) (*App, error) {
	app := &App{cfg: cfg, hub: hub.NewHub(), metrics: metrics.NewMetrics(), started: time.Now(), listening: make(chan struct{})}
	describeMetrics(app.metrics)
	stdout := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(stdout))

	db, err := database.Open(cfg.Database.Path)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	app.db = db
	app.sink = NewLogSink(db, config.LogLevels[strings.ToLower(cfg.Database.LogLevel)], app.hub)
	slog.SetDefault(slog.New(NewTeeHandler(stdout, app.sink)))

	store, err := NewStore(cfg.DynamicDir)
	if err != nil {
		return nil, fmt.Errorf("dynamic config: %w", err)
	}
	var seen map[string]bool
	store.onReload = func(ok bool) {
		// Pooled keep-alive connections skip the dial-time guard, so drop them
		// whenever rules change; revoked destinations stop working at once.
		if app.proxy != nil {
			app.proxy.tr.CloseIdleConnections()
			app.forward.client.CloseIdleConnections()
		}
		// When access.json or a route changes, log the access notes that are new
		// (e.g. "webhook x accepts requests from any address without auth").
		if seen == nil {
			seen = map[string]bool{}
			for _, n := range app.notes {
				seen[n.Level+n.Message] = true
			}
		}
		next, fresh := map[string]bool{}, []DeployNote(nil)
		for _, n := range accessNotes(app.cfg, store.snap.Load(), app.env) {
			next[n.Level+n.Message] = true
			if !seen[n.Level+n.Message] {
				fresh = append(fresh, n)
			}
		}
		seen = next
		if len(fresh) > 0 {
			slog.Info("access rules changed", "kind", "deploy", "topic", "access")
			logDeploymentNotes(fresh)
		}
		res := "ok"
		if !ok {
			res = "error"
		}
		app.metrics.Inc("fluid_config_reloads_total", "result", res)
	}
	app.store = store
	app.guard = &netguard.Guard{Policy: func() (bool, []netip.Prefix) {
		pc := store.Proxy()
		return pc.AllowPrivate, pc.privateOK
	}, Metrics: app.metrics}
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
	if app.draining.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "draining": true})
		return
	}
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
	shutdownTracing, err := telemetry.Setup(ctx, cfg.Telemetry, version)
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	app.tracing = cfg.Telemetry.OTLPEndpoint != "" || os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
	app.env = detectEnv()
	app.notes = deploymentNotes(cfg, app.store.snap.Load(), app.env, len(os.Getenv("PROXY_ADMIN_TOKEN")) >= 16, time.Now())
	logDeploymentNotes(app.notes)
	app.hooks.Run(ctx, "before_startup", app.hooks.BeforeStartup)

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
	app.addrs = map[string]string{}
	for _, rs := range running {
		app.addrs[rs.name] = rs.ln.Addr().String()
		slog.Info("listening", "kind", "system", "server", rs.name, "addr", rs.ln.Addr().String(), "scheme", rs.scheme)
		go func(rs runningServer) {
			if err := rs.srv.Serve(rs.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s: %w", rs.name, err)
			}
		}(rs)
	}
	slog.Info("fluid-agent started", "kind", "system", "version", version)
	close(app.listening)
	app.hooks.Start(ctx, "after_startup", app.hooks.AfterStartup)

	select {
	case <-ctx.Done():
	case err = <-errc:
		slog.Error("server failed", "kind", "system", "err", err)
	}
	app.shutdown(running, stopWatch, cancelBG, &bgWG, shutdownTracing)
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

// shutdown stops the agent gracefully:
//  1. /readyz starts failing; with shutdown_delay the listeners keep serving
//     that long so load balancers stop sending new requests;
//  2. live streams end, listeners stop accepting, and in-flight requests,
//     CONNECT tunnels and deliveries get shutdown_timeout to finish;
//  3. at the deadline, remaining connections and tunnels are closed and
//     unfinished deliveries are requeued (attempt not counted), never lost;
//  4. logs are flushed and the database is closed.
//
// The before_shutdown hook runs alongside step 1 and after_shutdown before
// step 4; neither can block shutdown past its timeout.
//
// A second SIGINT/SIGTERM ends the process at once.
func (app *App) shutdown(running []runningServer, stopWatch chan struct{}, cancelBG context.CancelFunc,
	bgWG *sync.WaitGroup, shutdownTracing func(context.Context) error) {
	start := time.Now()
	timeout, delay := app.cfg.ShutdownTimeout.D(), app.cfg.ShutdownDelay.D()
	app.draining.Store(true)
	slog.Info("shutting down gracefully (send the signal again to stop at once)", "kind", "system",
		"timeout", timeout.String(), "delay", delay.String(), "open_tunnels", app.proxy.tunnels.Load(),
		"deliveries_in_flight", app.outbox.Counts(context.Background())["inflight"])
	hookDone := app.hooks.Start(context.Background(), "before_shutdown", app.hooks.BeforeShutdown)
	if delay > 0 {
		slog.Info("readiness now fails; still serving during shutdown_delay", "kind", "system", "delay", delay.String())
		time.Sleep(delay)
	}
	<-hookDone // returns at the hook's timeout at the latest

	deadline, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	app.hub.Close() // live streams never go idle; end them so Shutdown can finish
	close(stopWatch)
	cancelBG() // outbox stops claiming new deliveries; in-flight ones continue

	var wg sync.WaitGroup
	var forced, tunnelsClosed atomic.Int64
	for _, rs := range running {
		wg.Add(1)
		go func(rs runningServer) {
			defer wg.Done()
			if err := rs.srv.Shutdown(deadline); err != nil {
				forced.Add(1)
				rs.srv.Close() // deadline reached: close what is left
			}
		}(rs)
	}
	wg.Add(1)
	go func() { defer wg.Done(); tunnelsClosed.Store(int64(app.proxy.DrainTunnels(deadline))) }()
	bgDone := make(chan struct{})
	go func() { bgWG.Wait(); close(bgDone) }()
	select {
	case <-bgDone:
	case <-deadline.Done():
		app.outbox.abort() // requeue deliveries still in flight
		<-bgDone
	}
	wg.Wait()

	if n := forced.Load(); n > 0 {
		slog.Warn("shutdown timeout reached: closed remaining connections", "kind", "system", "listeners", n)
	}
	if n := tunnelsClosed.Load(); n > 0 {
		slog.Warn("shutdown timeout reached: closed open CONNECT tunnels", "kind", "system", "tunnels", n)
	}
	if n := app.outbox.interrupted.Load(); n > 0 {
		slog.Warn("deliveries interrupted by shutdown were requeued", "kind", "system", "deliveries", n)
	}
	tctx, tcancel := context.WithTimeout(context.Background(), 5*time.Second)
	shutdownTracing(tctx)
	tcancel()
	slog.Info("shutdown complete", "kind", "system", "took", time.Since(start).Round(time.Millisecond).String())
	app.hooks.Run(context.Background(), "after_shutdown", app.hooks.AfterShutdown)
	app.sink.Close()
	app.db.Close()
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
	if cfg, err := config.Load(*cfgPath); err == nil {
		if !pathPresent(*cfgPath) {
			fmt.Fprintf(out, "%s does not exist yet: built-in defaults apply, and it is created on first start (or run fluid-agent init)\n", *cfgPath)
		}
		if !pathPresent(cfg.DynamicDir) {
			fmt.Fprintf(out, "%s does not exist yet: it is created on first start (or run fluid-agent init)\n", cfg.DynamicDir)
		}
		ns := deploymentNotes(cfg, loadDir(cfg.DynamicDir, nil), detectEnv(), len(os.Getenv("PROXY_ADMIN_TOKEN")) >= 16, time.Now())
		fmt.Fprintln(out, "\nDeployment notes:")
		for _, n := range ns {
			fmt.Fprintf(out, "  %-5s [%s] %s\n", strings.ToUpper(n.Level), n.Topic, n.Message)
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
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return []string{err.Error()}
	}
	if fi, err := os.Stat(cfg.DynamicDir); err == nil && !fi.IsDir() {
		return []string{cfg.DynamicDir + ": not a directory"}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
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
		addr := config.Default().WebhookAddr
		if cfg, err := config.Load(*cfgPath); err == nil {
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
// The proxy and the console check their access.json allowlist before anything else.
func (app *App) proxyHandler() http.Handler { return app.accessGuard("proxy", app.proxy) }

func (app *App) webhookHandler() http.Handler {
	// each route checks its allowed_ips; every call is captured in full (RQH/RQB/RSH/RSB)
	return app.captureInbound(stripBasePath(app.cfg.WebhookBasePath, app.inbound.Handler()))
}

func (app *App) adminHandler() http.Handler {
	return app.accessGuard("admin", stripBasePath(app.cfg.AdminBasePath, app.admin.Handler()))
}

// openListeners binds every listener (with TLS where configured) plus any
// ACME HTTP-01 challenge listeners. Nothing is served until Serve runs them.
func (app *App) openListeners() ([]runningServer, error) {
	cfg := app.cfg
	acmeCache := filepath.Join(filepath.Dir(cfg.Database.Path), "acme")
	specs := []struct {
		name string
		srv  *http.Server
		tls  *config.TLSConfig
		h1   bool
	}{
		// The proxy strips its base path itself: absolute-URL proxy requests
		// carry the destination's path, which must never be rewritten.
		{"proxy", &http.Server{Addr: cfg.ProxyAddr, Handler: app.proxyHandler(), ReadHeaderTimeout: 10 * time.Second}, cfg.ProxyTLS, true},
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

// describeMetrics adds the help text for the agent's counters.
func describeMetrics(m *metrics.Metrics) {
	for name, h := range map[string]string{
		"fluid_proxy_requests_total":       "Forward proxy requests by kind (http, connect) and result.",
		"fluid_forward_requests_total":     "JSON /forward requests by result.",
		"fluid_inbound_requests_total":     "Webhook and ingest requests by source, route and result.",
		"fluid_deliveries_total":           "Delivery attempts by mode and result.",
		"fluid_config_reloads_total":       "Config reloads by result.",
		"fluid_rate_limited_total":         "Inbound requests rejected by the per-IP rate limit.",
		"fluid_admin_requests_total":       "Admin API requests by role and result.",
		"fluid_blocked_destinations_total": "Destinations refused by the private-address guard.",
	} {
		m.Describe(name, "counter", h)
	}
}
