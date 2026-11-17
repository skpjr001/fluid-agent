package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func main() {
	cfgPath := flag.String("config", "config.json", "static config file")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := loadStatic(*cfgPath)
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	store, err := NewStore(cfg.DynamicDir)
	if err != nil {
		slog.Error("store", "err", err)
		os.Exit(1)
	}
	stop := make(chan struct{})
	go store.Watch(cfg.reloadEvery, stop)

	relay := NewWebhookRelay(store, cfg.TrustForwardedFor, cfg.MaxBodyBytes)
	admin := &Admin{store: store, relay: relay, token: cfg.AdminToken}

	hookMux := http.NewServeMux()
	hookMux.Handle("/hooks/", relay)
	hookMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	servers := []*http.Server{
		{Addr: cfg.ProxyAddr, Handler: NewForwardProxy(store, cfg.MaxConns), ReadHeaderTimeout: 10 * time.Second},
		{Addr: cfg.WebhookAddr, Handler: hookMux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second},
		{Addr: cfg.AdminAddr, Handler: admin.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second},
	}
	names := []string{"proxy", "webhooks", "admin"}

	var wg sync.WaitGroup
	for i, s := range servers {
		wg.Add(1)
		go func(name string, s *http.Server) {
			defer wg.Done()
			slog.Info("listening", "server", name, "addr", s.Addr)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("server failed", "server", name, "err", err)
				os.Exit(1)
			}
		}(names[i], s)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	slog.Info("shutting down")
	close(stop)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, s := range servers {
		s.Shutdown(ctx)
	}
	wg.Wait()
}
