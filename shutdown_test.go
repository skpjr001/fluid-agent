package main

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// An open console live stream must not hold up a graceful shutdown.
func TestShutdownEndsLiveStreams(t *testing.T) {
	ta := newTestApp(t, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: ta.adminHandler()}
	go srv.Serve(ln)

	req, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+"/api/stream?topics=logs", nil)
	req.Header.Set("Authorization", "Bearer "+viewerToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	if line, _ := br.ReadString('\n'); !strings.HasPrefix(line, ": connected") {
		t.Fatalf("stream did not start: %q", line)
	}

	ended := make(chan error, 1)
	go func() {
		for {
			if _, err := br.ReadString('\n'); err != nil {
				ended <- err
				return
			}
		}
	}()

	// What Serve does on SIGTERM.
	start := time.Now()
	ta.hub.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v (took %s)", err, time.Since(start))
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("shutdown took %s; the live stream held it up", d)
	}
	select {
	case err := <-ended:
		if err == nil || (!errors.Is(err, context.Canceled) && err.Error() != "EOF" && !strings.Contains(err.Error(), "EOF")) {
			t.Logf("stream ended with %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the client's stream did not end")
	}
	ta.hub.Close() // idempotent
}
