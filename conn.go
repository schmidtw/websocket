//go:build !js
// +build !js

package websocket

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
)

// MessageType represents the type of a WebSocket message.
// See https://tools.ietf.org/html/rfc6455#section-5.6
type MessageType int

// MessageType constants.
const (
	// MessageText is for UTF-8 encoded text messages like JSON.
	MessageText MessageType = iota + 1
	// MessageBinary is for binary messages like protobufs.
	MessageBinary
)

// Conn represents a WebSocket connection.
// All methods may be called concurrently except for Reader and Read.
//
// You must always read from the connection. Otherwise control
// frames will not be handled. See Reader and CloseRead.
//
// Be sure to call Close on the connection when you
// are finished with it to release associated resources.
//
// On any error from any method, the connection is closed
// with an appropriate reason.
//
// This applies to context expirations as well unfortunately.
// See https://github.com/nhooyr/websocket/issues/242#issuecomment-633182220
type Conn struct {
	noCopy noCopy

	subprotocol    string
	rwc            io.ReadWriteCloser
	client         bool
	copts          *compressionOptions
	flateThreshold int
	br             *bufio.Reader
	bw             *bufio.Writer

	readTimeout  chan context.Context
	writeTimeout chan context.Context

	// Read state.
	readMu            *mu
	readHeaderBuf     [8]byte
	readControlBuf    [maxControlPayload]byte
	msgReader         *msgReader
	readCloseFrameErr error

	// Write state.
	msgWriter      *msgWriter
	writeFrameMu   *mu
	writeBuf       []byte
	writeHeaderBuf [8]byte
	writeHeader    header

	wg         sync.WaitGroup
	closed     chan struct{}
	closeMu    sync.Mutex
	closeErr   error
	wroteClose bool

	pingCounter   int32
	activePingsMu sync.Mutex
	activePings   map[string]chan<- struct{}

	pingCallback func()
}

type connConfig struct {
	subprotocol    string
	rwc            io.ReadWriteCloser
	client         bool
	copts          *compressionOptions
	flateThreshold int

	br *bufio.Reader
	bw *bufio.Writer
}

func newConn(cfg connConfig) *Conn {
	c := &Conn{
		subprotocol:    cfg.subprotocol,
		rwc:            cfg.rwc,
		client:         cfg.client,
		copts:          cfg.copts,
		flateThreshold: cfg.flateThreshold,

		br: cfg.br,
		bw: cfg.bw,

		readTimeout:  make(chan context.Context),
		writeTimeout: make(chan context.Context),

		closed:      make(chan struct{}),
		activePings: make(map[string]chan<- struct{}),
	}

	c.readMu = newMu(c)
	c.writeFrameMu = newMu(c)

	c.msgReader = newMsgReader(c)

	c.msgWriter = newMsgWriter(c)
	if c.client {
		c.writeBuf = extractBufioWriterBuf(c.bw, c.rwc)
	}

	if c.flate() && c.flateThreshold == 0 {
		c.flateThreshold = 128
		if !c.msgWriter.flateContextTakeover() {
			c.flateThreshold = 512
		}
	}

	runtime.SetFinalizer(c, func(c *Conn) {
		c.close(errors.New("connection garbage collected"))
	})

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.timeoutLoop()
	}()

	return c
}

// Subprotocol returns the negotiated subprotocol.
// An empty string means the default protocol.
func (c *Conn) Subprotocol() string {
	return c.subprotocol
}

func (c *Conn) close(err error) {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()

	if c.isClosed() {
		return
	}
	if err == nil {
		err = c.rwc.Close()
	}
	c.setCloseErrLocked(err)

	close(c.closed)
	runtime.SetFinalizer(c, nil)

	// Have to close after c.closed is closed to ensure any goroutine that wakes up
	// from the connection being closed also sees that c.closed is closed and returns
	// closeErr.
	c.rwc.Close()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.msgWriter.close()
		c.msgReader.close()
	}()
}

func (c *Conn) timeoutLoop() {
	readCtx := context.Background()
	writeCtx := context.Background()

	for {
		select {
		case <-c.closed:
			return

		case writeCtx = <-c.writeTimeout:
		case readCtx = <-c.readTimeout:

		case <-readCtx.Done():
			c.setCloseErr(fmt.Errorf("read timed out: %w", readCtx.Err()))
			c.wg.Add(1)
			go func() {
				defer c.wg.Done()
				c.writeError(StatusPolicyViolation, errors.New("read timed out"))
			}()
		case <-writeCtx.Done():
			c.close(fmt.Errorf("write timed out: %w", writeCtx.Err()))
			return
		}
	}
}

func (c *Conn) flate() bool {
	return c.copts != nil
}

// Ping sends a ping to the peer and waits for a pong.
// Use this to measure latency or ensure the peer is responsive.
// Ping must be called concurrently with Reader as it does
// not read from the connection but instead waits for a Reader call
// to read the pong.
//
// TCP Keepalives should suffice for most use cases.
func (c *Conn) Ping(ctx context.Context) error {
	p := atomic.AddInt32(&c.pingCounter, 1)

	err := c.ping(ctx, strconv.Itoa(int(p)))
	if err != nil {
		return fmt.Errorf("failed to ping: %w", err)
	}
	return nil
}

func (c *Conn) ping(ctx context.Context, p string) error {
	pong := make(chan struct{}, 1)

	c.activePingsMu.Lock()
	c.activePings[p] = pong
	c.activePingsMu.Unlock()

	defer func() {
		c.activePingsMu.Lock()
		delete(c.activePings, p)
		c.activePingsMu.Unlock()
	}()

	err := c.writeControl(ctx, opPing, []byte(p))
	if err != nil {
		return err
	}

	select {
	case <-c.closed:
		return net.ErrClosed
	case <-ctx.Done():
		err := fmt.Errorf("failed to wait for pong: %w", ctx.Err())
		c.close(err)
		return err
	case <-pong:
		return nil
	}
}

// SetPingCallback sets a callback that is called when a ping is received.
// The callback is called synchronously from the Reader goroutine and must
// not block.
func (c *Conn) SetPingCallback(cb func()) {
	c.pingCallback = cb
}

type mu struct {
	c  *Conn
	ch chan struct{}
}

func newMu(c *Conn) *mu {
	return &mu{
		c:  c,
		ch: make(chan struct{}, 1),
	}
}

func (m *mu) forceLock() {
	m.ch <- struct{}{}
}

func (m *mu) tryLock() bool {
	select {
	case m.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

func (m *mu) lock(ctx context.Context) error {
	select {
	case <-m.c.closed:
		return net.ErrClosed
	case <-ctx.Done():
		err := fmt.Errorf("failed to acquire lock: %w", ctx.Err())
		m.c.close(err)
		return err
	case m.ch <- struct{}{}:
		// To make sure the connection is certainly alive.
		// As it's possible the send on m.ch was selected
		// over the receive on closed.
		select {
		case <-m.c.closed:
			// Make sure to release.
			m.unlock()
			return net.ErrClosed
		default:
		}
		return nil
	}
}

func (m *mu) unlock() {
	select {
	case <-m.ch:
	default:
	}
}

type noCopy struct{}

func (*noCopy) Lock() {}
