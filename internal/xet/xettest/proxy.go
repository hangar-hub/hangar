package xettest

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// IdleProxy is a TCP proxy that drops a connection when the server sends
// nothing for longer than its idle timeout, as Cloudflare and other proxies
// do while an origin is silent. Use it to show that /v2/shards heartbeats
// keep a slow shard upload alive.
type IdleProxy struct {
	// Addr is the proxy's listen address.
	Addr string
	// Dropped counts connections closed for being idle.
	Dropped atomic.Int64

	ln     net.Listener
	target string
	idle   time.Duration
	wg     sync.WaitGroup
}

// NewIdleProxy listens on a random local port and forwards to target.
func NewIdleProxy(target string, idle time.Duration) (*IdleProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &IdleProxy{Addr: ln.Addr().String(), ln: ln, target: target, idle: idle}
	p.wg.Add(1)
	go p.serve()
	return p, nil
}

// Close stops the proxy and waits for its connections to end.
func (p *IdleProxy) Close() error {
	err := p.ln.Close()
	p.wg.Wait()
	return err
}

func (p *IdleProxy) serve() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handle(c)
		}()
	}
}

func (p *IdleProxy) handle(client net.Conn) {
	defer client.Close()
	server, err := net.Dial("tcp", p.target)
	if err != nil {
		return
	}
	defer server.Close()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(server, client)
		done <- struct{}{}
	}()
	go func() {
		buf := make([]byte, 32<<10)
		for {
			// The idle clock runs only on what the server sends.
			_ = server.SetReadDeadline(time.Now().Add(p.idle))
			n, err := server.Read(buf)
			if n > 0 {
				if _, werr := client.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					p.Dropped.Add(1)
				}
				break
			}
		}
		done <- struct{}{}
	}()
	<-done
}
