package cli

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// Exercise the real HTTP and TLS stacks without opening a system socket.
type pipeListener struct {
	address  *net.TCPAddr
	incoming chan net.Conn
	done     chan struct{}
	once     sync.Once
}

func (l *pipeListener) Addr() net.Addr { return l.address }
func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}
func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.incoming:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

var testListeners sync.Map
var testPort atomic.Int32

func dialTestServer(ctx context.Context, _, addr string) (net.Conn, error) {
	entry, ok := testListeners.Load(addr)
	if !ok {
		return nil, net.ErrClosed
	}
	l := entry.(*pipeListener)
	local, remote := net.Pipe()
	select {
	case l.incoming <- remote:
		return local, nil
	case <-l.done:
	case <-ctx.Done():
	}
	local.Close()
	remote.Close()
	return nil, net.ErrClosed
}

func newTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return startPipeServer(t, false, handler)
}

func startPipeServer(t *testing.T, secure bool, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	l := &pipeListener{address: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(testPort.Add(1))}, incoming: make(chan net.Conn), done: make(chan struct{})}
	server := &httptest.Server{Listener: l, Config: &http.Server{Handler: handler}}
	testListeners.Store(l.Addr().String(), l)
	if secure {
		server.StartTLS()
	} else {
		server.Start()
	}
	transport := server.Client().Transport.(*http.Transport)
	transport.DialContext = dialTestServer
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() {
		server.Close()
		testListeners.Delete(l.Addr().String())
		http.DefaultTransport = previous
	})
	return server
}
