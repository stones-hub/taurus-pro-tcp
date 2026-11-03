package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tcperr "github.com/stones-hub/taurus-pro-tcp/pkg/tcp/errors"
	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol"
	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol/json"
)

const testTimeout = 5 * time.Second

func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(testTimeout):
		t.Fatalf("timed out after %v waiting for %s", testTimeout, what)
		panic("unreachable")
	}
}

// recHandler 记录客户端回调，可选地在 OnMessage / OnClose 里执行额外动作。
type recHandler struct {
	onMessage func(msg interface{})
	onClose   func()

	messages chan interface{}
	closes   chan struct{}
	errs     chan error

	mu     sync.Mutex
	events []string
}

func newRecHandler() *recHandler {
	return &recHandler{
		messages: make(chan interface{}, 1024),
		closes:   make(chan struct{}, 16),
		errs:     make(chan error, 1024),
	}
}

func (h *recHandler) record(kind string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, kind)
}

func (h *recHandler) eventLog() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.events...)
}

func (h *recHandler) OnConnect(context.Context, net.Conn) { h.record("connect") }

func (h *recHandler) OnMessage(_ context.Context, _ net.Conn, msg interface{}) {
	h.record("message")
	if h.onMessage != nil {
		h.onMessage(msg)
	}
	h.messages <- msg
}

func (h *recHandler) OnClose(context.Context, net.Conn) {
	h.record("close")
	if h.onClose != nil {
		h.onClose()
	}
	h.closes <- struct{}{}
}

func (h *recHandler) OnError(_ context.Context, _ net.Conn, err error) {
	h.record("error")
	select {
	case h.errs <- err:
	default:
	}
}

func drainErrs(h *recHandler) []error {
	var out []error
	for {
		select {
		case e := <-h.errs:
			out = append(out, e)
		default:
			return out
		}
	}
}

func packFrame(t *testing.T, seq uint32) []byte {
	t.Helper()
	b, err := json.New(1 << 20).Pack(&json.Message{Type: 1, Sequence: seq, Data: map[string]interface{}{"seq": float64(seq)}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func rawFrame(body []byte) []byte {
	b := make([]byte, json.HeaderSize+len(body))
	b[0], b[1] = 0xFE, 0xFF
	n := uint32(len(body))
	b[2], b[3], b[4], b[5] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	copy(b[json.HeaderSize:], body)
	return b
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// newPipeClient 创建一个通过 net.Pipe 连接的客户端，返回客户端和"服务端"一侧。
// net.Pipe 的一次 Write 恰好被对端的一次 Read 读完（小于读缓冲区时），所以一次 Read 收到多帧、
// 一帧分多次 Read 都是确定的。
func newPipeClient(t *testing.T, h Handler, opts ...TCPClientOption) (*Client, net.Conn) {
	t.Helper()
	c, _, err := New("pipe", protocol.JSON, h, opts...)
	if err != nil {
		t.Fatal(err)
	}
	clientSide, serverSide := net.Pipe()
	c.dial = func(context.Context, string, string) (net.Conn, error) { return clientSide, nil }
	if err := c.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() {
		_ = serverSide.Close()
		_ = c.Close()
	})
	return c, serverSide
}

// newTCPClient 通过真实 loopback TCP 连接一个客户端，返回客户端和服务端接受到的连接。
func newTCPClient(t *testing.T, h Handler) (*Client, net.Conn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := l.Accept(); err == nil {
			accepted <- conn
		}
	}()
	c, _, err := New(l.Addr().String(), protocol.JSON, h)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	server := recv(t, accepted, "server accept")
	t.Cleanup(func() {
		_ = server.Close()
		_ = c.Close()
	})
	return c, server
}

// startClient 在协程里运行 Start，返回 Start 返回时关闭的通道。
func startClient(c *Client) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		c.Start()
		close(done)
	}()
	return done
}

func write(t *testing.T, conn net.Conn, b []byte) {
	t.Helper()
	_ = conn.SetWriteDeadline(time.Now().Add(testTimeout))
	if _, err := conn.Write(b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func expectSeqs(t *testing.T, h *recHandler, seqs ...uint32) {
	t.Helper()
	for _, want := range seqs {
		m := recv(t, h.messages, "client OnMessage").(*json.Message)
		if m.Sequence != want {
			t.Fatalf("client got seq %d, want %d", m.Sequence, want)
		}
	}
}

// 一次 Read 收到多帧必须全部分发。修复前客户端只解析第一帧，后面的帧要等对端再发数据。
func TestClientReadLoopDispatchesAllFramesFromOneRead(t *testing.T) {
	h := newRecHandler()
	c, server := newPipeClient(t, h)
	startClient(c)
	write(t, server, concat(packFrame(t, 1), packFrame(t, 2), packFrame(t, 3)))
	expectSeqs(t, h, 1, 2, 3)
}

// 一帧拆成多次 Read，以及"帧尾 + 下一帧"混在同一次 Read 里。
func TestClientReadLoopReassemblesSplitFrames(t *testing.T) {
	h := newRecHandler()
	c, server := newPipeClient(t, h)
	startClient(c)

	f1 := packFrame(t, 1)
	write(t, server, f1[:3])
	write(t, server, f1[3:json.HeaderSize])
	write(t, server, f1[json.HeaderSize:])
	expectSeqs(t, h, 1)

	f2, f3, f4 := packFrame(t, 2), packFrame(t, 3), packFrame(t, 4)
	half := len(f3) / 2
	write(t, server, concat(f2, f3[:half]))
	write(t, server, concat(f3[half:], f4))
	expectSeqs(t, h, 2, 3, 4)
}

// 非法数据只丢弃协议指定的部分，同一次 Read 中后面的合法帧照常分发；超大帧同样整帧丢弃。
func TestClientReadLoopSkipsBadFramesAndContinues(t *testing.T) {
	h := newRecHandler()
	c, server := newPipeClient(t, h, WithMaxMsgSize(256))
	startClient(c)

	garbage := []byte("garbage-before-magic")
	badJSON := rawFrame([]byte("{not json"))
	missingFields := rawFrame([]byte(`{"type":0,"sequence":0,"data":{},"timestamp":0}`))
	oversized := rawFrame(make([]byte, 300)) // 超过客户端 256 字节上限
	write(t, server, concat(garbage, packFrame(t, 1), badJSON, missingFields, oversized, packFrame(t, 2)))
	expectSeqs(t, h, 1, 2)

	invalid, tooLarge, other := 0, 0, 0
	for _, e := range drainErrs(h) {
		switch {
		case e == tcperr.ErrInvalidFormat:
			invalid++
		case e == tcperr.ErrMessageTooLarge:
			tooLarge++
		case tcperr.IsProtocolError(e):
			other++
		}
	}
	if invalid != 2 || tooLarge != 1 || other != 1 {
		t.Fatalf("got %d invalid-format, %d too-large, %d other protocol errors; want 2, 1, 1", invalid, tooLarge, other)
	}
}

// 在 OnClose 里调用 Close / cleanup 不死锁；之后的 Close 立即返回。
// 修复前 OnClose 在 closeOnce 里面执行，重入 Close 会永久阻塞。
func TestClientCloseFromOnCloseDoesNotDeadlock(t *testing.T) {
	t.Run("started", func(t *testing.T) {
		h := newRecHandler()
		var c *Client
		h.onClose = func() { _ = c.Close(); _ = c.Close() }
		var server net.Conn
		c, server = newPipeClient(t, h)
		done := startClient(c)
		_ = server.Close() // 对端关闭 -> 读循环退出 -> Start 调用 OnClose
		recv(t, done, "Start returning after OnClose re-entered Close")
		returned := make(chan struct{})
		go func() { _ = c.Close(); close(returned) }()
		recv(t, returned, "Close after re-entrant OnClose")
	})
	t.Run("never started", func(t *testing.T) {
		h := newRecHandler()
		var c *Client
		h.onClose = func() { _ = c.Close() }
		c, _ = newPipeClient(t, h)
		returned := make(chan struct{})
		go func() { _ = c.Close(); _ = c.Close(); close(returned) }()
		recv(t, returned, "Close with re-entrant OnClose")
		recv(t, h.closes, "OnClose")
	})
}

// Start 运行时 Close：先关闭传输层（OnClose 里能看到 ctx 已取消、连接已关闭），Close 不等待回调就返回；
// OnClose 在读写协程退出后调用一次，之后不再有 OnMessage；Start 返回时 OnClose 已执行完。
func TestClientCloseOrdering(t *testing.T) {
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()

	h := newRecHandler()
	var c *Client
	type closeState struct {
		ctxDone bool
		connNil bool
	}
	states := make(chan closeState, 1)
	h.onClose = func() {
		states <- closeState{ctxDone: c.ctx.Err() != nil, connNil: c.getConn() == nil}
		<-release
	}
	var server net.Conn
	c, server = newPipeClient(t, h)
	done := startClient(c)

	// 对端持续推送消息，Close 发生在收发过程中
	frame := packFrame(t, 1)
	stopWriting := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-stopWriting:
				return
			default:
			}
			_ = server.SetWriteDeadline(time.Now().Add(testTimeout))
			if _, err := server.Write(frame); err != nil {
				return
			}
		}
	}()
	// 对端不限速地一直写：持续取走消息事件并计数，避免事件通道写满后把读协程卡在 OnMessage 里
	first := make(chan struct{})
	stopConsume := make(chan struct{})
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(stopConsume) }) }
	t.Cleanup(stop) // 测试中途失败时消费协程也会退出
	consumed := make(chan int, 1)
	go func() {
		n := 0
		for {
			select {
			case <-h.messages:
				n++
				if n == 1 {
					close(first)
				}
			case <-stopConsume:
				for {
					select {
					case <-h.messages:
						n++
					default:
						consumed <- n
						return
					}
				}
			}
		}
	}()
	recv(t, first, "first message")

	closed := make(chan struct{})
	go func() { _ = c.Close(); close(closed) }()
	recv(t, closed, "Close returning while OnClose is blocked")

	st := recv(t, states, "OnClose")
	if !st.ctxDone || !st.connNil {
		t.Fatalf("OnClose ran before transport was closed: ctxDone=%v connNil=%v", st.ctxDone, st.connNil)
	}
	select {
	case <-done:
		t.Fatalf("Start returned before OnClose finished")
	default:
	}
	releaseOnce()
	recv(t, done, "Start returning after OnClose")
	close(stopWriting)
	_ = server.Close()
	recv(t, writerDone, "writer exit")

	stop()
	received := recv(t, consumed, "message consumer")

	ev := h.eventLog()
	closes, messages := 0, 0
	for _, e := range ev {
		switch e {
		case "close":
			closes++
		case "message":
			messages++
		}
	}
	if closes != 1 || ev[len(ev)-1] != "close" {
		t.Fatalf("callbacks %d events: want exactly one close, and it last (last=%q)", len(ev), ev[len(ev)-1])
	}
	if received < 1 || received != messages {
		t.Fatalf("consumed %d message events, handler recorded %d; want equal and >= 1", received, messages)
	}
}

// 正常的本端关闭不上报错误；对端关闭恰好上报一次；Close 之后主动 Send 失败会返回 ErrClientClosed 并回调 OnError。
func TestClientCloseErrorReporting(t *testing.T) {
	t.Run("local close is silent", func(t *testing.T) {
		h := newRecHandler()
		c, _ := newPipeClient(t, h)
		done := startClient(c)
		_ = c.Close()
		recv(t, done, "Start returning")
		if errs := drainErrs(h); len(errs) != 0 {
			t.Fatalf("errors on local close = %v, want none", errs)
		}
		if err := c.Send(&json.Message{Type: 1, Sequence: 1}); !errors.Is(err, ErrClientClosed) {
			t.Fatalf("Send after Close = %v, want ErrClientClosed", err)
		}
		if errs := drainErrs(h); len(errs) != 1 || !errors.Is(errs[0], ErrClientClosed) {
			t.Fatalf("OnError after explicit Send on closed client = %v, want one ErrClientClosed", errs)
		}
	})
	t.Run("peer close reported once", func(t *testing.T) {
		// 用真实 loopback TCP：net.Pipe 在对端关闭后 SetReadDeadline 也会失败，与 TCP 行为不同
		h := newRecHandler()
		c, server := newTCPClient(t, h)
		done := startClient(c)
		// 先完成一次收发，确保读循环已经阻塞在 Read 上
		write(t, server, packFrame(t, 1))
		expectSeqs(t, h, 1)
		_ = server.Close()
		recv(t, done, "Start returning after peer close")
		errs := drainErrs(h)
		if len(errs) != 1 || errs[0].Error() != "connection closed" {
			t.Fatalf("errors on peer close = %v, want exactly one \"connection closed\"", errs)
		}
	})
}

// tempReadConn 的 Read 总是返回临时错误，用来让读循环进入重试退避。
type tempReadConn struct{ net.Conn }

type tempError struct{}

func (tempError) Error() string   { return "temporary read failure" }
func (tempError) Timeout() bool   { return false }
func (tempError) Temporary() bool { return true }

func (tempReadConn) Read([]byte) (int, error) {
	return 0, &net.OpError{Op: "read", Net: "tcp", Err: tempError{}}
}

// 重试退避等待可以被 Close 打断：退避设为 1 小时，Close 后 Start 仍立即返回。
func TestClientRetrySleepIsCancellable(t *testing.T) {
	h := newRecHandler()
	c, _, err := New("pipe", protocol.JSON, h, WithBaseRetryDelay(time.Hour), WithMaxRetryDelay(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	c.dial = func(context.Context, string, string) (net.Conn, error) { return tempReadConn{clientSide}, nil }
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	done := startClient(c)
	// 第一次临时错误上报后，读循环进入 1 小时的退避
	if e := recv(t, h.errs, "temporary read error"); e == nil {
		t.Fatal("nil error")
	}
	_ = c.Close()
	recv(t, done, "Start returning during retry backoff")
}

// controlledDial 让拨号阻塞到测试放行，用来精确构造 Connect 与 Close 的竞态。
type controlledDial struct {
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	peers   []net.Conn // 每次拨号返回的连接的对端
}

func newControlledDial() *controlledDial {
	return &controlledDial{entered: make(chan struct{}, 8), release: make(chan struct{})}
}

func (d *controlledDial) dial(context.Context, string, string) (net.Conn, error) {
	clientSide, serverSide := net.Pipe()
	d.mu.Lock()
	d.peers = append(d.peers, serverSide)
	d.mu.Unlock()
	d.entered <- struct{}{}
	<-d.release // 故意忽略 ctx：模拟 Close 之后拨号才成功返回
	return clientSide, nil
}

// peerClosed 判断拨号返回的连接是否已被客户端关闭（对端读到错误而不是超时）。
func peerClosed(peer net.Conn) bool {
	_ = peer.SetReadDeadline(time.Now().Add(testTimeout))
	_, err := peer.Read(make([]byte, 1))
	var ne net.Error
	return err != nil && !(errors.As(err, &ne) && ne.Timeout())
}

// Close 发生在拨号期间：拨号随后成功，Connect 必须关闭新连接并返回 ErrClientClosed，不能让客户端复活。
func TestClientConnectRacingClose(t *testing.T) {
	d := newControlledDial()
	h := newRecHandler()
	c, _, err := New("pipe", protocol.JSON, h)
	if err != nil {
		t.Fatal(err)
	}
	c.dial = d.dial

	connectErr := make(chan error, 1)
	go func() { connectErr <- c.Connect() }()
	recv(t, d.entered, "dial started")
	_ = c.Close()
	close(d.release)

	if err := recv(t, connectErr, "Connect returning"); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("Connect racing Close = %v, want ErrClientClosed", err)
	}
	if c.IsConnected() || c.RemoteAddr() != nil {
		t.Fatalf("client revived after Close: connected=%v remote=%v", c.IsConnected(), c.RemoteAddr())
	}
	if !peerClosed(d.peers[0]) {
		t.Fatalf("connection dialed after Close was not closed (leaked)")
	}
	if err := c.Connect(); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("Connect after Close = %v, want ErrClientClosed", err)
	}
}

// 并发 Connect：只有一个成功，其余关闭各自拨到的连接并返回 ErrAlreadyConnected，不泄漏连接。
func TestClientConcurrentConnect(t *testing.T) {
	d := newControlledDial()
	h := newRecHandler()
	c, _, err := New("pipe", protocol.JSON, h)
	if err != nil {
		t.Fatal(err)
	}
	c.dial = d.dial
	defer c.Close()

	const n = 4
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() { errs <- c.Connect() }()
	}
	for i := 0; i < n; i++ {
		recv(t, d.entered, "dial started")
	}
	close(d.release)

	ok, already := 0, 0
	for i := 0; i < n; i++ {
		switch err := recv(t, errs, "Connect returning"); {
		case err == nil:
			ok++
		case errors.Is(err, ErrAlreadyConnected):
			already++
		default:
			t.Fatalf("concurrent Connect = %v", err)
		}
	}
	if ok != 1 || already != n-1 {
		t.Fatalf("concurrent Connect: %d succeeded, %d already-connected; want 1 and %d", ok, already, n-1)
	}
	closedPeers := 0
	for _, p := range d.peers {
		_ = p.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		_, err := p.Read(make([]byte, 1))
		var ne net.Error
		if err != nil && !(errors.As(err, &ne) && ne.Timeout()) {
			closedPeers++
		}
	}
	if closedPeers != n-1 {
		t.Fatalf("%d of %d losing connections closed, want %d", closedPeers, n-1, n-1)
	}
}

// resourceHandler 在 OnConnect 里登记资源、在 OnClose 里释放，用来验证 OnClose 一定晚于完成的 OnConnect。
type resourceHandler struct {
	*recHandler
	mu        sync.Mutex
	resources map[net.Conn]bool // OnConnect 已完成登记的连接
	connected bool              // 最近一次 OnConnect 是否已完整执行
	leaked    []string          // OnClose 时发现的顺序问题
	onConnect func()            // 可选：OnConnect 登记之后执行（例如阻塞或重入 Close）
}

func newResourceHandler() *resourceHandler {
	return &resourceHandler{recHandler: newRecHandler(), resources: make(map[net.Conn]bool)}
}

func (h *resourceHandler) OnConnect(ctx context.Context, conn net.Conn) {
	h.recHandler.OnConnect(ctx, conn)
	h.mu.Lock()
	h.resources[conn] = true
	h.mu.Unlock()
	if h.onConnect != nil {
		h.onConnect()
	}
	h.mu.Lock()
	h.connected = true
	h.mu.Unlock()
}

func (h *resourceHandler) OnClose(ctx context.Context, conn net.Conn) {
	h.mu.Lock()
	if conn != nil {
		if !h.resources[conn] {
			h.leaked = append(h.leaked, "OnClose before OnConnect registered the connection")
		} else if !h.connected {
			h.leaked = append(h.leaked, "OnClose while OnConnect still running")
		}
		delete(h.resources, conn)
	}
	h.mu.Unlock()
	h.recHandler.OnClose(ctx, conn)
}

func (h *resourceHandler) check(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.leaked) != 0 || len(h.resources) != 0 {
		t.Fatalf("resource order problems %v, %d resources never released", h.leaked, len(h.resources))
	}
}

func pipeDial(c *Client) net.Conn {
	clientSide, serverSide := net.Pipe()
	c.dial = func(context.Context, string, string) (net.Conn, error) { return clientSide, nil }
	return serverSide
}

// Close 发生在"连接已登记、OnConnect 还没开始"的窗口：Close 立即返回，返回时 IsConnected 已为 false、
// 连接已关闭；OnClose 推迟到 OnConnect 完成之后，Connect 返回 ErrClientClosed。
func TestClientCloseBeforeOnConnectCompletes(t *testing.T) {
	h := newResourceHandler()
	c, _, err := New("pipe", protocol.JSON, h)
	if err != nil {
		t.Fatal(err)
	}
	peer := pipeDial(c)
	defer peer.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	c.testHookBeforeOnConnect = func() {
		close(entered)
		<-release
	}

	connectErr := make(chan error, 1)
	go func() { connectErr <- c.Connect() }()
	recv(t, entered, "Connect registered the connection")

	closed := make(chan struct{})
	go func() { _ = c.Close(); close(closed) }()
	recv(t, closed, "Close returning while Connect is between registration and OnConnect")
	if c.IsConnected() || c.RemoteAddr() != nil {
		t.Fatalf("after Close: IsConnected=%v RemoteAddr=%v, want false and nil", c.IsConnected(), c.RemoteAddr())
	}
	if !peerClosed(peer) {
		t.Fatalf("connection not closed when Close returned")
	}
	if ev := h.eventLog(); len(ev) != 0 {
		t.Fatalf("callbacks before OnConnect ran: %v, want none (OnClose must wait for OnConnect)", ev)
	}

	close(release)
	if err := recv(t, connectErr, "Connect returning"); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("Connect = %v, want ErrClientClosed", err)
	}
	if ev := h.eventLog(); len(ev) != 2 || ev[0] != "connect" || ev[1] != "close" {
		t.Fatalf("callbacks %v, want [connect close]", ev)
	}
	if c.IsConnected() {
		t.Fatalf("IsConnected true after Close")
	}
	h.check(t)
}

// 在 OnConnect 里重入 Close：Close 立即返回，OnClose 在 OnConnect 完整执行完之后才调用。
func TestClientCloseInsideOnConnect(t *testing.T) {
	h := newResourceHandler()
	c, _, err := New("pipe", protocol.JSON, h)
	if err != nil {
		t.Fatal(err)
	}
	peer := pipeDial(c)
	defer peer.Close()
	h.onConnect = func() {
		_ = c.Close() // 例如认证失败
		_ = c.Close()
		if c.IsConnected() {
			t.Errorf("IsConnected true right after Close inside OnConnect")
		}
	}
	if err := c.Connect(); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("Connect with Close inside OnConnect = %v, want ErrClientClosed", err)
	}
	if ev := h.eventLog(); len(ev) != 2 || ev[0] != "connect" || ev[1] != "close" {
		t.Fatalf("callbacks %v, want [connect close]", ev)
	}
	h.check(t)
}

// Start 与 Connect 并发、读写在 OnConnect 返回前就结束：Start 不等待 OnConnect、直接返回，
// OnClose 由 Connect 在 OnConnect 返回后调用。
func TestClientStartFinishingWhileOnConnectRuns(t *testing.T) {
	h := newResourceHandler()
	c, _, err := New("pipe", protocol.JSON, h)
	if err != nil {
		t.Fatal(err)
	}
	peer := pipeDial(c)
	inConnect := make(chan struct{})
	release := make(chan struct{})
	h.onConnect = func() {
		close(inConnect)
		<-release
	}
	connectErr := make(chan error, 1)
	go func() { connectErr <- c.Connect() }()
	recv(t, inConnect, "OnConnect running")

	done := startClient(c)
	_ = peer.Close() // 对端关闭，读写协程退出
	recv(t, done, "Start returning while OnConnect still runs")
	// 对端关闭会上报一次 "connection closed"，这是允许的；不允许的是 OnClose 先于 OnConnect 完成
	for _, e := range h.eventLog() {
		if e == "close" {
			t.Fatalf("OnClose called before OnConnect returned: %v", h.eventLog())
		}
	}

	close(release)
	if err := recv(t, connectErr, "Connect returning"); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("Connect = %v, want ErrClientClosed", err)
	}
	ev := h.eventLog()
	if ev[0] != "connect" || ev[len(ev)-1] != "close" {
		t.Fatalf("callbacks %v, want connect first and close last", ev)
	}
	h.check(t)
}

// Connect / Close / Start 随机并发：每轮 OnClose 恰好一次、一定晚于完成的 OnConnect，资源都被释放，
// Close 返回后 IsConnected 一定为 false。
func TestClientConnectCloseStartRace(t *testing.T) {
	for i := 0; i < 200; i++ {
		h := newResourceHandler()
		c, _, err := New("pipe", protocol.JSON, h)
		if err != nil {
			t.Fatal(err)
		}
		peer := pipeDial(c)
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); _ = c.Connect() }()
		go func() { defer wg.Done(); c.Start() }()
		go func() {
			defer wg.Done()
			_ = c.Close()
			if c.IsConnected() {
				t.Errorf("iteration %d: IsConnected true after Close returned", i)
			}
		}()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		recv(t, done, "Connect/Start/Close returning")
		_ = peer.Close()
		if c.IsConnected() {
			t.Fatalf("iteration %d: IsConnected true after Close", i)
		}
		closes := 0
		for _, e := range h.eventLog() {
			if e == "close" {
				closes++
			}
		}
		if closes != 1 {
			t.Fatalf("iteration %d: OnClose called %d times (%v), want 1", i, closes, h.eventLog())
		}
		h.check(t)
	}
}

// receiveAsync 在协程里调用 SimpleReceive，便于加超时（SimpleReceive 本身没有读超时）。
func receiveAsync(c *Client) <-chan interface{} {
	ch := make(chan interface{}, 1)
	go func() {
		m, err := c.SimpleReceive()
		if err != nil {
			ch <- err
			return
		}
		ch <- m
	}()
	return ch
}

func expectReceived(t *testing.T, c *Client, want uint32) {
	t.Helper()
	switch v := recv(t, receiveAsync(c), "SimpleReceive").(type) {
	case *json.Message:
		if v.Sequence != want {
			t.Fatalf("SimpleReceive seq %d, want %d", v.Sequence, want)
		}
	case error:
		t.Fatalf("SimpleReceive error %v, want seq %d", v, want)
	}
}

// SimpleReceive 一次读到多帧：后面的帧留在缓存里，后续调用直接返回，不再读连接也不丢弃。
func TestSimpleReceiveKeepsExtraFrames(t *testing.T) {
	h := newRecHandler()
	c, server := newPipeClient(t, h)
	written := make(chan struct{})
	go func() {
		_, _ = server.Write(concat(packFrame(t, 1), packFrame(t, 2), packFrame(t, 3)))
		close(written)
	}()
	expectReceived(t, c, 1)
	recv(t, written, "single write consumed by the first Read") // 之后连接上已没有数据
	expectReceived(t, c, 2)
	expectReceived(t, c, 3)
}

// SimpleReceive 拆帧：等到完整的帧才返回，不再因为半帧报错丢数据。
func TestSimpleReceiveReassemblesSplitFrame(t *testing.T) {
	h := newRecHandler()
	c, server := newPipeClient(t, h)
	f := packFrame(t, 5)
	go func() {
		_, _ = server.Write(f[:4])
		_, _ = server.Write(f[4:])
	}()
	expectReceived(t, c, 5)
	if errs := drainErrs(h); len(errs) != 0 {
		t.Fatalf("errors for a split frame = %v, want none", errs)
	}
}

// SimpleReceive 缓存的剩余数据在 Start 之后交给读协程；Start 运行期间、或另一个 SimpleReceive
// 进行中调用 SimpleReceive，立即返回 ErrReceiveConflict。
func TestSimpleReceiveHandoverAndConflicts(t *testing.T) {
	h := newRecHandler()
	c, server := newPipeClient(t, h)
	go func() { _, _ = server.Write(concat(packFrame(t, 1), packFrame(t, 2))) }()
	expectReceived(t, c, 1)
	expectReceived(t, c, 2) // 来自缓存

	// 缓存已空，下一次 SimpleReceive 阻塞在 Read 上；此时再调用一次
	blocked := receiveAsync(c)
	waitStateTrue(t, c, func() bool { return c.receiving }, "SimpleReceive blocked in Read")
	if _, err := c.SimpleReceive(); !errors.Is(err, ErrReceiveConflict) {
		t.Fatalf("concurrent SimpleReceive = %v, want ErrReceiveConflict", err)
	}
	written := make(chan struct{})
	go func() {
		_, _ = server.Write(concat(packFrame(t, 3), packFrame(t, 4)))
		close(written)
	}()
	if m, ok := recv(t, blocked, "blocked SimpleReceive").(*json.Message); !ok || m.Sequence != 3 {
		t.Fatalf("blocked SimpleReceive = %v, want seq 3", m)
	}
	recv(t, written, "write consumed") // 帧 4 已进入 SimpleReceive 的缓存
	waitStateTrue(t, c, func() bool { return !c.receiving }, "SimpleReceive finished")

	// Start 之后由读协程分发缓存的帧 4，不需要对端再发数据
	startClient(c)
	expectSeqs(t, h, 4)
	if _, err := c.SimpleReceive(); !errors.Is(err, ErrReceiveConflict) {
		t.Fatalf("SimpleReceive while Start runs = %v, want ErrReceiveConflict", err)
	}
}

func waitStateTrue(t *testing.T, c *Client, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		c.stateMu.Lock()
		ok := cond()
		c.stateMu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// errReadConn 的 Read 总是返回一个非 EOF 错误。
type errReadConn struct{ net.Conn }

var errBoom = errors.New("boom")

func (errReadConn) Read([]byte) (int, error) { return 0, errBoom }

// SimpleReceive 的读错误只上报一次（此前非 EOF 错误会上报两次）。
func TestSimpleReceiveReportsReadErrorOnce(t *testing.T) {
	t.Run("EOF", func(t *testing.T) {
		h := newRecHandler()
		c, server := newPipeClient(t, h)
		_ = server.Close()
		if v := recv(t, receiveAsync(c), "SimpleReceive"); v.(error).Error() != "connection closed" {
			t.Fatalf("SimpleReceive after peer close = %v", v)
		}
		if errs := drainErrs(h); len(errs) != 1 {
			t.Fatalf("errors = %v, want exactly one", errs)
		}
		// 与读协程口径一致：对端关闭上报一次，但不计入错误数
		if got := atomic.LoadInt64(&c.stats.Errors); got != 0 {
			t.Fatalf("stats.Errors after peer close = %d, want 0", got)
		}
	})
	t.Run("non-EOF", func(t *testing.T) {
		h := newRecHandler()
		c, _, err := New("pipe", protocol.JSON, h)
		if err != nil {
			t.Fatal(err)
		}
		clientSide, serverSide := net.Pipe()
		defer serverSide.Close()
		c.dial = func(context.Context, string, string) (net.Conn, error) { return errReadConn{clientSide}, nil }
		if err := c.Connect(); err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.SimpleReceive(); err == nil {
			t.Fatalf("SimpleReceive with failing read returned nil error")
		}
		if errs := drainErrs(h); len(errs) != 1 || !errors.Is(errs[0], errBoom) && errs[0].Error() != "read error: boom" {
			t.Fatalf("errors = %v, want exactly one read error", errs)
		}
	})
}

// flakyWriteConn 前 failures 次 Write 返回临时错误，并记录每次 SetWriteDeadline / Write 的调用。
type flakyWriteConn struct {
	net.Conn
	mu        sync.Mutex
	failures  int
	writes    int
	deadlines []time.Time
}

func (f *flakyWriteConn) SetWriteDeadline(t time.Time) error {
	f.mu.Lock()
	f.deadlines = append(f.deadlines, t)
	f.mu.Unlock()
	return f.Conn.SetWriteDeadline(t)
}

func (f *flakyWriteConn) Write(b []byte) (int, error) {
	f.mu.Lock()
	f.writes++
	fail := f.writes <= f.failures
	f.mu.Unlock()
	if fail {
		return 0, &net.OpError{Op: "write", Net: "tcp", Err: tempError{}}
	}
	return f.Conn.Write(b)
}

func (f *flakyWriteConn) counts() (writes int, deadlines []time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes, append([]time.Time(nil), f.deadlines...)
}

// 写重试前重新设置写入期限：每次 Write 之前都有一次 SetWriteDeadline，且期限向后推。
func TestClientWriteRetryRefreshesDeadline(t *testing.T) {
	h := newRecHandler()
	c, _, err := New("pipe", protocol.JSON, h, WithBaseRetryDelay(time.Millisecond), WithMaxRetryDelay(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	fc := &flakyWriteConn{Conn: clientSide, failures: 2}
	c.dial = func(context.Context, string, string) (net.Conn, error) { return fc, nil }
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	startClient(c)

	if err := c.Send(&json.Message{Type: 1, Sequence: 7, Data: map[string]interface{}{}}); err != nil {
		t.Fatal(err)
	}
	_ = serverSide.SetReadDeadline(time.Now().Add(testTimeout))
	buf := make([]byte, 4096)
	n, err := serverSide.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("peer read after retries: n=%d err=%v", n, err)
	}
	writes, deadlines := fc.counts()
	if writes != 3 || len(deadlines) != writes {
		t.Fatalf("%d writes and %d SetWriteDeadline calls, want 3 and one before every write", writes, len(deadlines))
	}
	for i := 1; i < len(deadlines); i++ {
		if !deadlines[i].After(deadlines[i-1]) {
			t.Fatalf("write deadline %d (%v) not later than %d (%v)", i, deadlines[i], i-1, deadlines[i-1])
		}
	}
}

// scriptedWriteConn 按顺序执行 steps 里的 Write 行为，用完后透传；repeat 时一直重复最后一步。
type scriptedWriteConn struct {
	net.Conn
	mu     sync.Mutex
	steps  []func(under net.Conn, b []byte) (int, error)
	repeat bool
	writes int
}

func (s *scriptedWriteConn) Write(b []byte) (int, error) {
	s.mu.Lock()
	s.writes++
	var step func(net.Conn, []byte) (int, error)
	if len(s.steps) > 0 {
		step = s.steps[0]
		if len(s.steps) > 1 || !s.repeat {
			s.steps = s.steps[1:]
		}
	}
	s.mu.Unlock()
	if step == nil {
		return s.Conn.Write(b)
	}
	return step(s.Conn, b)
}

func (s *scriptedWriteConn) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

var errTempWrite = &net.OpError{Op: "write", Net: "tcp", Err: tempError{}}

func fixedClientFrame(t *testing.T, seq uint32) ([]byte, *json.Message) {
	t.Helper()
	m := &json.Message{Type: 1, Sequence: seq, Data: map[string]interface{}{"seq": float64(seq)}, Timestamp: int64(seq)}
	b, err := json.New(1 << 20).Pack(m)
	if err != nil {
		t.Fatal(err)
	}
	return b, m
}

func newScriptedClient(t *testing.T, h Handler, repeat bool, steps ...func(net.Conn, []byte) (int, error)) (*Client, *scriptedWriteConn, net.Conn, <-chan struct{}) {
	t.Helper()
	c, _, err := New("pipe", protocol.JSON, h, WithBaseRetryDelay(time.Millisecond), WithMaxRetryDelay(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	clientSide, serverSide := net.Pipe()
	wc := &scriptedWriteConn{Conn: clientSide, steps: steps, repeat: repeat}
	c.dial = func(context.Context, string, string) (net.Conn, error) { return wc, nil }
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	done := startClient(c)
	t.Cleanup(func() {
		_ = serverSide.Close()
		_ = c.Close()
		recv(t, done, "client Start returning")
	})
	return c, wc, serverSide, done
}

func readExactly(t *testing.T, conn net.Conn, n int) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(testTimeout))
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("peer read %d bytes: %v", n, err)
	}
	return buf
}

func waitStat(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// 客户端第一次 Write 只写出半帧并返回临时错误：重试只发剩下的一半，对端恰好收到完整的 seq1 和 seq2，
// BytesWritten 是实际写出的字节数。修复前从帧开头整帧重发，对端收到重复的前缀。
func TestClientWriteRetrySendsOnlyRemainderAfterPartialWrite(t *testing.T) {
	half := func(under net.Conn, b []byte) (int, error) {
		n, _ := under.Write(b[:len(b)/2])
		return n, errTempWrite
	}
	c, wc, server, _ := newScriptedClient(t, newRecHandler(), false, half)
	f1, m1 := fixedClientFrame(t, 1)
	f2, m2 := fixedClientFrame(t, 2)
	if err := c.Send(m1); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(m2); err != nil {
		t.Fatal(err)
	}
	got := readExactly(t, server, len(f1)+len(f2))
	if !bytes.Equal(got, concat(f1, f2)) {
		t.Fatalf("peer stream = %q, want exactly frame1+frame2 %q", got, concat(f1, f2))
	}
	waitStat(t, "two messages counted", func() bool { return atomic.LoadInt64(&c.stats.MessagesSent) == 2 })
	if got := atomic.LoadInt64(&c.stats.BytesWritten); got != int64(len(f1)+len(f2)) {
		t.Fatalf("BytesWritten = %d, want %d", got, len(f1)+len(f2))
	}
	if n := wc.writeCount(); n != 3 {
		t.Fatalf("%d writes, want 3 (half, remainder, frame2)", n)
	}
	_ = c.Close()
	_ = server.SetReadDeadline(time.Now().Add(testTimeout))
	if rest, _ := io.ReadAll(server); len(rest) != 0 {
		t.Fatalf("peer received %d extra bytes: %q", len(rest), rest)
	}
}

// 客户端 Write 全部写出但同时返回临时错误：不再重发。
func TestClientWriteNotResentWhenFullyWrittenWithTemporaryError(t *testing.T) {
	all := func(under net.Conn, b []byte) (int, error) {
		n, _ := under.Write(b)
		return n, errTempWrite
	}
	c, _, server, _ := newScriptedClient(t, newRecHandler(), false, all)
	f1, m1 := fixedClientFrame(t, 1)
	f2, m2 := fixedClientFrame(t, 2)
	_ = c.Send(m1)
	_ = c.Send(m2)
	if got := readExactly(t, server, len(f1)+len(f2)); !bytes.Equal(got, concat(f1, f2)) {
		t.Fatalf("peer stream = %q, want exactly frame1+frame2", got)
	}
	_ = c.Close()
	_ = server.SetReadDeadline(time.Now().Add(testTimeout))
	if rest, _ := io.ReadAll(server); len(rest) != 0 {
		t.Fatalf("peer received %d extra bytes: %q", len(rest), rest)
	}
}

// 客户端 Write 一直返回 (0, nil)：按重试计，用尽 maxRetries 后关闭，Start 返回，不记为已发送。
func TestClientWriteZeroProgressIsBounded(t *testing.T) {
	h := newRecHandler()
	zero := func(net.Conn, []byte) (int, error) { return 0, nil }
	c, wc, _, done := newScriptedClient(t, h, true, zero)
	_ = c.Send(&json.Message{Type: 1, Sequence: 1, Data: map[string]interface{}{}})
	recv(t, done, "Start returning after zero-progress retries are exhausted")
	if n := wc.writeCount(); n != c.maxRetries+1 {
		t.Fatalf("%d writes, want %d (1 + maxRetries)", n, c.maxRetries+1)
	}
	if got := atomic.LoadInt64(&c.stats.MessagesSent); got != 0 {
		t.Fatalf("MessagesSent = %d, want 0", got)
	}
}

// SimpleReceive 阻塞时调用 Start：Start 回调 OnError(ErrReceiveConflict) 并立即返回，不启动读协程；
// SimpleReceive 返回后再次 Start 可以正常工作，并接手缓存的数据。
func TestStartRejectedWhileSimpleReceiveBlocked(t *testing.T) {
	h := newRecHandler()
	c, server := newPipeClient(t, h)
	blocked := receiveAsync(c)
	waitStateTrue(t, c, func() bool { return c.receiving }, "SimpleReceive blocked in Read")

	recv(t, startClient(c), "Start returning while SimpleReceive is blocked")
	errs := drainErrs(h)
	if len(errs) != 1 || !errors.Is(errs[0], ErrReceiveConflict) {
		t.Fatalf("errors = %v, want one ErrReceiveConflict", errs)
	}
	c.stateMu.Lock()
	started := c.started
	c.stateMu.Unlock()
	if started {
		t.Fatalf("rejected Start marked the client as started")
	}

	go func() { _, _ = server.Write(concat(packFrame(t, 1), packFrame(t, 2))) }()
	if m, ok := recv(t, blocked, "SimpleReceive").(*json.Message); !ok || m.Sequence != 1 {
		t.Fatalf("SimpleReceive = %v, want seq 1", m)
	}
	waitStateTrue(t, c, func() bool { return !c.receiving }, "SimpleReceive finished")
	startClient(c)
	expectSeqs(t, h, 2) // 缓存的帧 2 由重新启动的读协程分发
}

// 阻塞中的 SimpleReceive 被本端 Close 打断：返回 ErrClientClosed，不回调 OnError、不计错误。
// 对端关闭（EOF）回调一次 OnError，和读协程一样不计入错误数。
func TestSimpleReceiveInterruptedByClose(t *testing.T) {
	h := newRecHandler()
	c, _ := newPipeClient(t, h)
	blocked := receiveAsync(c)
	waitStateTrue(t, c, func() bool { return c.receiving }, "SimpleReceive blocked in Read")
	_ = c.Close()
	if err, ok := recv(t, blocked, "SimpleReceive returning after Close").(error); !ok || !errors.Is(err, ErrClientClosed) {
		t.Fatalf("SimpleReceive interrupted by Close = %v, want ErrClientClosed", err)
	}
	if errs := drainErrs(h); len(errs) != 0 {
		t.Fatalf("errors = %v, want none for a local close", errs)
	}
	if got := atomic.LoadInt64(&c.stats.Errors); got != 0 {
		t.Fatalf("stats.Errors = %d, want 0", got)
	}
}

// 在 OnConnect 里同步调用 Start：Start 一直运行到对端关闭，结束时 OnConnect 还没返回，
// 于是把 OnClose 交给 Connect；Connect 在 OnConnect 返回后调用 OnClose 并返回 ErrClientClosed。
func TestStartInsideOnConnect(t *testing.T) {
	h := newResourceHandler()
	c, _, err := New("pipe", protocol.JSON, h)
	if err != nil {
		t.Fatal(err)
	}
	peer := pipeDial(c)
	inStart := make(chan struct{})
	h.onConnect = func() {
		close(inStart)
		c.Start()
	}
	connectErr := make(chan error, 1)
	go func() { connectErr <- c.Connect() }()
	recv(t, inStart, "OnConnect calling Start")
	_ = peer.Close()

	if err := recv(t, connectErr, "Connect returning"); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("Connect with Start inside OnConnect = %v, want ErrClientClosed", err)
	}
	ev := h.eventLog()
	closes := 0
	for _, e := range ev {
		if e == "close" {
			closes++
		}
	}
	if ev[0] != "connect" || ev[len(ev)-1] != "close" || closes != 1 {
		t.Fatalf("callbacks %v, want connect first, exactly one close, close last", ev)
	}
	h.check(t)
}

// Start 先取到连接（此时还没 Connect，为 nil），随后 Connect 完整执行（登记连接、OnConnect 返回）；
// Start 的读写协程这才启动并立即结束、关闭传输层（连同刚登记的连接）。OnClose 必须收到那个
// 被关闭的连接，用户在 OnConnect 里为它登记的资源才能释放。
func TestStartCapturedNilConnBeforeConnect(t *testing.T) {
	h := newResourceHandler()
	c, _, err := New("pipe", protocol.JSON, h)
	if err != nil {
		t.Fatal(err)
	}
	peer := pipeDial(c)
	defer peer.Close()
	captured := make(chan struct{})
	release := make(chan struct{})
	c.testHookStartCaptured = func() {
		close(captured)
		<-release
	}
	done := startClient(c)
	recv(t, captured, "Start captured the (nil) connection")
	if err := c.Connect(); err != nil {
		t.Fatalf("Connect after Start captured nil = %v", err)
	}
	close(release)
	recv(t, done, "Start returning")
	if !peerClosed(peer) {
		t.Fatalf("connection registered by Connect was not closed")
	}
	h.check(t)
}
