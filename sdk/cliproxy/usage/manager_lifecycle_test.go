package usage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// gatedPlugin blocks on the first record until gate closes, then counts every record.
type gatedPlugin struct {
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once
	mu      sync.Mutex
	models  []string
}

func newGatedPlugin() *gatedPlugin {
	return &gatedPlugin{gate: make(chan struct{}), entered: make(chan struct{})}
}

func (p *gatedPlugin) HandleUsage(_ context.Context, record Record) {
	p.once.Do(func() {
		close(p.entered)
		<-p.gate
	})
	p.mu.Lock()
	p.models = append(p.models, record.Model)
	p.mu.Unlock()
}

func (p *gatedPlugin) handled() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.models...)
}

// waitingCtx reports when Shutdown starts waiting on it.
type waitingCtx struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func (c *waitingCtx) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestShutdownWaitsForQueuedRecords(t *testing.T) {
	mgr := NewManager(10)
	plugin := newGatedPlugin()
	mgr.Register(plugin)
	mgr.Publish(context.Background(), Record{Model: "in-flight"})
	<-plugin.entered
	mgr.Publish(context.Background(), Record{Model: "queued"})

	ctx := &waitingCtx{Context: context.Background(), waiting: make(chan struct{})}
	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Shutdown(ctx) }()
	var err error
	select {
	case <-ctx.waiting:
		close(plugin.gate)
		err = <-errCh
	case err = <-errCh: // returned without waiting: the in-flight record is still blocked
		t.Cleanup(func() { close(plugin.gate) })
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := plugin.handled(); len(got) != 2 {
		t.Fatalf("handled %v after Shutdown, want in-flight and queued", got)
	}
}

func TestShutdownGivesUpWhenContextEnds(t *testing.T) {
	mgr := NewManager(10)
	plugin := newGatedPlugin()
	t.Cleanup(func() { close(plugin.gate) })
	mgr.Register(plugin)
	mgr.Publish(context.Background(), Record{Model: "stuck"})
	<-plugin.entered

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := mgr.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown with ended context = %v, want context.Canceled", err)
	}
}

func TestStartAfterShutdownDeliversAgain(t *testing.T) {
	mgr := NewManager(10)
	plugin := &captureUsagePlugin{records: make(chan Record, 1)}
	mgr.Register(plugin)
	mgr.Start(context.Background())
	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr.Publish(context.Background(), Record{Model: "dropped"})

	mgr.Start(context.Background())
	t.Cleanup(mgr.Stop)
	mgr.Publish(context.Background(), Record{Model: "after-restart"})
	select {
	case got := <-plugin.records:
		if got.Model != "after-restart" {
			t.Fatalf("got %q, want after-restart (records published while stopped are dropped)", got.Model)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery after restart")
	}
}
