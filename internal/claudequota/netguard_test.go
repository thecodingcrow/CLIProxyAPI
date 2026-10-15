package claudequota

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"testing"
)

// TestMain refuses every resolver dial for the whole package: real upstreams
// are hostnames, the fakes are 127.0.0.1, so any resolution fails the run.
func TestMain(m *testing.M) {
	previous := net.DefaultResolver
	var mu sync.Mutex
	var attempts []string
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(_ context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		attempts = append(attempts, network+" "+address)
		mu.Unlock()
		return nil, errors.New("external dial blocked by test guard")
	}}
	code := m.Run()
	net.DefaultResolver = previous
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) > 0 {
		_, _ = os.Stderr.WriteString("claudequota tests attempted external resolution\n")
		code = 1
	}
	os.Exit(code)
}
