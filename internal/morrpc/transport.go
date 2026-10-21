package morrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

// MaxMessageBytes bounds a single request read by the server (audio uploads
// are chunked by the protocol, so 32 MiB is generous).
const MaxMessageBytes = 32 << 20

// NormalizeEndpoint turns an on-chain provider endpoint into host:port.
func NormalizeEndpoint(endpoint string) string {
	e := strings.TrimSpace(endpoint)
	for _, p := range []string{"tcp://", "http://", "https://"} {
		e = strings.TrimPrefix(e, p)
	}
	return strings.TrimSuffix(e, "/")
}

// Client dials providers. One TCP connection carries exactly one request.
type Client struct {
	DialTimeout  time.Duration
	WriteTimeout time.Duration
	ReplyTimeout time.Duration
	Dial         func(ctx context.Context, network, addr string) (net.Conn, error)
}

// DefaultClient uses the reference node's timeouts.
func DefaultClient() *Client {
	return &Client{DialTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, ReplyTimeout: 30 * time.Second}
}

func (c *Client) dial(ctx context.Context, addr string) (net.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, c.DialTimeout)
	defer cancel()
	var conn net.Conn
	var err error
	if c.Dial != nil {
		conn, err = c.Dial(dctx, "tcp", NormalizeEndpoint(addr))
	} else {
		d := net.Dialer{KeepAlive: 10 * time.Second}
		conn, err = d.DialContext(dctx, "tcp", NormalizeEndpoint(addr))
	}
	if err != nil {
		return nil, fmt.Errorf("connect to provider %s: %w", addr, err)
	}
	return conn, nil
}

func (c *Client) send(conn net.Conn, msg *RPCMessage) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(c.WriteTimeout))
	if _, err := conn.Write(b); err != nil {
		return fmt.Errorf("write to provider: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})
	return nil
}

// Call sends msg and returns the first response.
func (c *Client) Call(ctx context.Context, addr string, msg *RPCMessage) (*RPCResponse, error) {
	var out *RPCResponse
	err := c.Stream(ctx, addr, msg, c.ReplyTimeout, c.ReplyTimeout, func(r *RPCResponse) (bool, error) {
		out = r
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errors.New("provider closed the connection without replying")
	}
	return out, nil
}

// ErrTimeout is returned when the provider does not answer in time.
var ErrTimeout = errors.New("provider timed out")

// Stream sends msg and calls fn for every response until fn reports done,
// the provider closes the connection, or an error occurs. firstTimeout bounds
// the wait for the first response and idleTimeout the gap between later ones
// (zero disables a bound).
func (c *Client) Stream(ctx context.Context, addr string, msg *RPCMessage, firstTimeout, idleTimeout time.Duration, fn func(*RPCResponse) (done bool, err error)) error {
	conn, err := c.dial(ctx, addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-stop:
		}
	}()

	if err := c.send(conn, msg); err != nil {
		return err
	}
	dec := json.NewDecoder(bufio.NewReader(conn))
	deadline := firstTimeout
	received := 0
	for {
		if deadline > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(deadline))
		} else {
			_ = conn.SetReadDeadline(time.Time{})
		}
		var res RPCResponse
		if err := dec.Decode(&res); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return fmt.Errorf("%w after %s", ErrTimeout, deadline)
			}
			if errors.Is(err, io.EOF) {
				if received == 0 {
					return errors.New("provider closed the connection without replying")
				}
				return nil
			}
			return fmt.Errorf("read from provider: %w", err)
		}
		received++
		deadline = idleTimeout
		done, err := fn(&res)
		if err != nil || done {
			return err
		}
	}
}

// Handler serves one request; send may be called repeatedly to stream responses.
type Handler func(ctx context.Context, msg *RPCMessage, remote string, send func(*RPCResponse) error) error

// Server accepts consumer connections (provider side).
type Server struct {
	Handler     Handler
	Log         *slog.Logger
	MaxConns    int
	ReadTimeout time.Duration
}

// Serve accepts connections on ln until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if s.ReadTimeout == 0 {
		s.ReadTimeout = 30 * time.Second
	}
	maxConns := s.MaxConns
	if maxConns <= 0 {
		maxConns = 1024
	}
	slots := make(chan struct{}, maxConns)
	var wg sync.WaitGroup
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.Log.Warn("accept failed", "err", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			s.Log.Warn("connection limit reached, rejecting", "remote", conn.RemoteAddr().String())
			_ = json.NewEncoder(conn).Encode(PlainError("", "provider at capacity"))
			_ = conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			s.serveConn(ctx, conn)
		}()
	}
}

func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(10 * time.Second)
	}
	remote := conn.RemoteAddr().String()
	defer func() {
		if r := recover(); r != nil {
			s.Log.Error("panic serving MOR-RPC request", "remote", remote, "panic", r)
		}
	}()

	_ = conn.SetReadDeadline(time.Now().Add(s.ReadTimeout))
	var msg RPCMessage
	if err := json.NewDecoder(io.LimitReader(conn, MaxMessageBytes)).Decode(&msg); err != nil {
		s.Log.Debug("bad request", "remote", remote, "err", err)
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var writeMu sync.Mutex
	send := func(r *RPCResponse) error {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := conn.Write(b); err != nil {
			cancel()
			return err
		}
		return nil
	}
	if err := s.Handler(cctx, &msg, remote, send); err != nil {
		s.Log.Debug("request failed", "method", msg.Method, "remote", remote, "err", err)
		_ = send(PlainError(msg.ID, err.Error()))
	}
}
