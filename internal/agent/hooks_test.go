package agent

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skpjr001/fluid-agent/internal/hooks"
)

func TestLifecycleHooksRunInOrderAndCannotBreakStartupOrShutdown(t *testing.T) {
	var mu sync.Mutex
	var order []string
	mark := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, s)
	}
	var drainingAtBefore atomic.Bool
	stuck := make(chan struct{})
	defer close(stuck)
	afterStartup := make(chan struct{})

	var sa *servedApp
	sa = serveAppHooks(t, nil, hooks.Hooks{
		Timeout: 200 * time.Millisecond,
		BeforeStartup: func(context.Context) error {
			mark("before_startup")
			panic("a broken hook must not stop startup")
		},
		AfterStartup: func(context.Context) error {
			mark("after_startup")
			close(afterStartup)
			return errors.New("logged, not fatal")
		},
		BeforeShutdown: func(context.Context) error {
			mark("before_shutdown")
			drainingAtBefore.Store(sa.draining.Load())
			<-stuck // ignores ctx: shutdown must go on at the timeout
			return nil
		},
		AfterShutdown: func(context.Context) error {
			mark("after_shutdown")
			return nil
		},
	})
	<-afterStartup
	resp, err := http.Get("http://" + sa.addrs["admin"] + "/livez")
	if err != nil {
		t.Fatalf("served despite the panicking before_startup hook: %v", err)
	}
	resp.Body.Close()

	start := time.Now()
	sa.cancel()
	select {
	case <-sa.done:
	case <-time.After(5 * time.Second):
		t.Fatal("a stuck before_shutdown hook blocked shutdown")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("shutdown took %v", d)
	}
	if sa.err != nil {
		t.Fatalf("Serve: %v", sa.err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"before_startup", "after_startup", "before_shutdown", "after_shutdown"}; !slices.Equal(order, want) {
		t.Fatalf("order %v, want %v", order, want)
	}
	if !drainingAtBefore.Load() {
		t.Fatal("before_shutdown runs once readiness fails")
	}
}
