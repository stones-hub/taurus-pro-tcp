package tcp

import (
	"context"
	stderrors "errors"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tcperrors "github.com/stones-hub/taurus-pro-tcp/pkg/tcp/errors"
)

// ctx 预先取消时，StopContext 仍先同步完成网络层关闭再返回：返回后立刻 Dial 被拒绝，
// 现有连接的 socket 已关闭；仍在执行的回调（这里是卡住的 OnClose）使它返回 Canceled。
func TestStopContextPreCancelledClosesTransportFirst(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	h := newRecordingHandler()
	h.onClose = func(*Connection) { <-release }
	rs := startServer(t, h, nil)

	clients := []net.Conn{rs.dial(t), rs.dial(t)}
	var conns []*Connection
	for range clients {
		conns = append(conns, recv(t, h.connects, "OnConnect"))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// 只用一个 P，并且返回后先做纯内存检查、不做任何系统调用：如果 StopContext 只是把关闭
	// 交给另一个协程，那个协程此时还没有机会运行，检查就会失败（不依赖调度时机碰巧落后）
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	err := rs.StopContext(ctx)
	closedFlags := []int32{atomic.LoadInt32(&conns[0].closed), atomic.LoadInt32(&conns[1].closed)}
	dialConn, dialErr := net.Dial("tcp", rs.addr)

	if !stderrors.Is(err, context.Canceled) {
		t.Fatalf("StopContext(cancelled) = %v, want Canceled (OnClose still blocked)", err)
	}
	if dialErr == nil {
		_ = dialConn.Close()
		t.Fatalf("dial right after StopContext(cancelled) returned succeeded, want refused")
	}
	for i, f := range closedFlags {
		if f != 1 {
			t.Fatalf("connection %d transport not closed when StopContext returned", i)
		}
	}
	for _, c := range clients {
		expectClosed(t, c, "peer after StopContext(cancelled)")
	}
}

// 在回调里调用 Stop / StopContext 不会死锁：Stop 立即返回；StopContext 等的是包括当前回调在内的
// 所有回调，所以在回调里只能等到 ctx 结束、返回超时错误；外层随后能正常完成关闭，重复调用也立即返回。
func TestStopFromInsideCallbacks(t *testing.T) {
	type result struct{ stopCtxErr error }
	results := make(chan result, 1)
	var srv atomic.Pointer[Server]
	h := newRecordingHandler()
	h.onClose = func(*Connection) {
		s := srv.Load()
		s.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		results <- result{stopCtxErr: s.StopContext(ctx)}
	}
	rs := startServer(t, h, nil)
	srv.Store(rs.Server)

	client := rs.dial(t)
	other := rs.dial(t)
	recv(t, h.connects, "OnConnect")
	recv(t, h.connects, "OnConnect")

	_ = client.Close() // 对端关闭 -> OnClose -> Stop + StopContext
	r := recv(t, results, "Stop/StopContext returning inside OnClose")
	if !stderrors.Is(r.stopCtxErr, context.DeadlineExceeded) {
		t.Fatalf("StopContext inside OnClose = %v, want DeadlineExceeded", r.stopCtxErr)
	}
	expectClosed(t, other, "other connection after Stop inside OnClose")
	// 另一个连接的 OnClose 也会调用 StopContext，取走它的结果
	recv(t, results, "second OnClose")

	stopped := make(chan struct{})
	go func() {
		rs.Stop()
		rs.Stop()
		close(stopped)
	}()
	recv(t, stopped, "repeated Stop after stop from callback")
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := rs.StopContext(ctx); err != nil {
		t.Fatalf("outer StopContext = %v", err)
	}
}

func TestStopFromOnMessage(t *testing.T) {
	var srv atomic.Pointer[Server]
	stopped := make(chan struct{})
	h := newRecordingHandler()
	h.onMessage = func(*Connection, interface{}) {
		srv.Load().Stop()
		close(stopped)
	}
	rs := startServer(t, h, nil)
	srv.Store(rs.Server)

	client := rs.dial(t)
	conn := recv(t, h.connects, "OnConnect")
	writeAll(t, client, packFrame(t, 1))
	recv(t, stopped, "Stop returning inside OnMessage")
	expectClosed(t, client, "connection after Stop inside OnMessage")
	if closed := recv(t, h.closes, "OnClose after OnMessage returned"); closed != conn {
		t.Fatalf("OnClose for conn %d, want %d", closed.ID(), conn.ID())
	}
}

// 回调卡住时并发、重复地调用 Stop / StopContext：每次调用都有界返回，且 StopContext 不额外留下协程。
func TestRepeatedStopWithBlockedCallbacksIsBounded(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 8)
	h := newRecordingHandler()
	h.onMessage = func(*Connection, interface{}) {
		entered <- struct{}{}
		<-release
	}
	rs := startServer(t, h, nil)
	clients := []net.Conn{rs.dial(t), rs.dial(t), rs.dial(t)}
	for _, c := range clients {
		recv(t, h.connects, "OnConnect")
		writeAll(t, c, packFrame(t, 1))
		recv(t, entered, "OnMessage entered")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	rs.Stop() // 网络层关闭后，剩下的协程只有卡在 OnMessage 里的处理协程
	runtime.GC()
	before := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); rs.Stop() }()
		go func() { defer wg.Done(); errs <- rs.StopContext(cancelled) }()
	}
	waitGroupOrFail(t, &wg, "concurrent Stop/StopContext returning while callbacks are blocked")
	close(errs)
	for err := range errs {
		if !stderrors.Is(err, context.Canceled) {
			t.Fatalf("StopContext with blocked OnMessage = %v, want Canceled", err)
		}
	}
	// 本测试自己起的 200 个协程退出后，协程数应回到调用前的水平：StopContext 没有留下等待协程
	waitUntil(t, "goroutines back to the level before 100 StopContext calls", func() bool {
		return runtime.NumGoroutine() <= before
	})
	for _, c := range clients {
		expectClosed(t, c, "connection with blocked OnMessage")
	}

	close(release)
	ctx, cancel2 := context.WithTimeout(context.Background(), testTimeout)
	defer cancel2()
	if err := rs.StopContext(ctx); err != nil {
		t.Fatalf("StopContext after release = %v", err)
	}
	assertCallbackOrder(t, h)
}

// 服务器已经在实际接入和收发时，Stop 与新连接、收发并发：每轮先确认 4 个连接都完成了一次回显，
// 再让 4 个协程不断新建连接并收发、同时 Stop。所有调用有界返回，已有连接都看到关闭，
// Start 返回 nil，每个被接入的连接回调成对且有序。
// （Stop 抢在 Start 监听之前的竞态由 TestConcurrentStartStop 覆盖。）
func TestConcurrentStartStopWithClients(t *testing.T) {
	for i := 0; i < 30; i++ {
		h := newRecordingHandler()
		h.echo = true
		s, _, err := NewServer("127.0.0.1:0", newTestProtocol(), h)
		if err != nil {
			t.Fatal(err)
		}
		startErr := make(chan error, 1)
		go func() { startErr <- s.Start() }()
		waitUntil(t, "server listening", func() bool { return s.listenAddr() != "" })
		addr := s.listenAddr()

		// 第一批：确定已接入并完成收发
		var held []net.Conn
		for j := 0; j < 4; j++ {
			c, err := net.DialTimeout("tcp", addr, testTimeout)
			if err != nil {
				t.Fatalf("iteration %d: dial: %v", i, err)
			}
			held = append(held, c)
			writeAll(t, c, packFrame(t, uint32(j+1)))
			if m := newFrameReader(c).next(t); m.Sequence != uint32(j+1) {
				t.Fatalf("iteration %d: echo seq %d, want %d", i, m.Sequence, j+1)
			}
		}

		// 第二批：与 Stop 并发的新连接和收发，结果可能成功也可能被拒绝或关闭，只要求不卡住
		var wg sync.WaitGroup
		for j := 0; j < 4; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				frame, _ := newTestProtocol().Pack(newSeqMessage(1))
				for k := 0; k < 20; k++ {
					c, err := net.DialTimeout("tcp", addr, testTimeout)
					if err != nil {
						return // 监听已关闭
					}
					_ = c.SetDeadline(time.Now().Add(testTimeout))
					_, _ = c.Write(frame)
					_, _ = c.Read(make([]byte, 256))
					_ = c.Close()
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.Gosched()
			s.Stop()
		}()
		waitGroupOrFail(t, &wg, "clients and Stop finishing")

		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		if err := s.StopContext(ctx); err != nil {
			t.Fatalf("iteration %d: StopContext = %v", i, err)
		}
		cancel()
		for _, c := range held {
			expectClosed(t, c, "served connection after Stop")
			_ = c.Close()
		}
		if err := recv(t, startErr, "Start returning"); err != nil {
			t.Fatalf("iteration %d: Start = %v, want nil", i, err)
		}
		if got := len(h.eventLog()); got < 4 {
			t.Fatalf("iteration %d: %d connections had callbacks, want >= 4", i, got)
		}
		assertCallbackOrder(t, h)
	}
}

// fakeListener 的 Accept 依次返回预设的错误，用来模拟 Accept 的致命错误和临时错误。
type fakeListener struct {
	mu     sync.Mutex
	errs   []error
	closed chan struct{}
	once   sync.Once
}

func newFakeListener(errs ...error) *fakeListener {
	return &fakeListener{errs: errs, closed: make(chan struct{})}
}

func (l *fakeListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.errs) == 0 {
		return nil, net.ErrClosed
	}
	err := l.errs[0]
	l.errs = l.errs[1:]
	return nil, err
}

func (l *fakeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *fakeListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

type tempError struct{}

func (tempError) Error() string   { return "temporary accept failure" }
func (tempError) Timeout() bool   { return false }
func (tempError) Temporary() bool { return true }

func newServerWithListener(t *testing.T, h Handler, l net.Listener) *Server {
	t.Helper()
	s, _, err := NewServer("127.0.0.1:0", newTestProtocol(), h)
	if err != nil {
		t.Fatal(err)
	}
	s.listen = func(string, string) (net.Listener, error) { return l, nil }
	s.baseDelay = time.Millisecond
	s.maxDelay = time.Millisecond
	return s
}

// Accept 遇到非临时错误时，Start 返回可辨识的错误（ErrSystemFatal + 底层错误），不再返回 nil。
func TestStartReturnsAcceptFatalError(t *testing.T) {
	boom := stderrors.New("accept boom")
	h := newRecordingHandler()
	s := newServerWithListener(t, h, newFakeListener(boom))

	startErr := make(chan error, 1)
	go func() { startErr <- s.Start() }()
	err := recv(t, startErr, "Start returning after fatal accept error")
	if !stderrors.Is(err, tcperrors.ErrSystemFatal) || !stderrors.Is(err, boom) {
		t.Fatalf("Start = %v, want error wrapping ErrSystemFatal and the accept error", err)
	}
	h.waitErr(t, "OnError(nil, ErrSystemFatal)", func(e errEvent) bool {
		return e.conn == nil && e.err == tcperrors.ErrSystemFatal
	})
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := s.StopContext(ctx); err != nil {
		t.Fatalf("StopContext after fatal error = %v", err)
	}
	if err := s.Start(); err != tcperrors.ErrServerStopped {
		t.Fatalf("Start after fatal exit = %v, want ErrServerStopped", err)
	}
}

// 临时错误按 maxRetries 重试（每次上报 ErrSystemOverload），用尽后 Start 返回致命错误。
func TestStartReturnsErrorAfterTemporaryAcceptRetries(t *testing.T) {
	temp := &net.OpError{Op: "accept", Net: "tcp", Err: tempError{}}
	h := newRecordingHandler()
	s := newServerWithListener(t, h, newFakeListener(temp, temp, temp, temp))

	startErr := make(chan error, 1)
	go func() { startErr <- s.Start() }()
	err := recv(t, startErr, "Start returning after retries")
	if !stderrors.Is(err, tcperrors.ErrSystemFatal) || !stderrors.Is(err, temp) {
		t.Fatalf("Start = %v, want error wrapping ErrSystemFatal and the temporary error", err)
	}
	if n := countErrs(h, tcperrors.ErrSystemOverload); n != s.maxRetries {
		t.Fatalf("ErrSystemOverload reported %d times, want %d", n, s.maxRetries)
	}
}

// ConnectionCount 是已登记的连接数；acceptLoop 阻塞在 Accept 时占用的槽位不算在内。
func TestConnectionCountExcludesAcceptSlot(t *testing.T) {
	h := newRecordingHandler()
	rs := startServer(t, h, nil, WithMaxConnections(4))
	rs.dial(t)
	recv(t, h.connects, "OnConnect")
	// 等 acceptLoop 拿到下一个槽位并阻塞在 Accept 上：此时槽位通道里有 2 个
	waitUntil(t, "acceptLoop holding the next slot", func() bool { return len(rs.connChan) == 2 })
	if got := rs.ConnectionCount(); got != 1 {
		t.Fatalf("ConnectionCount() = %d, want 1", got)
	}
}

// 在 Start 之前就关闭的连接（例如 Stop 恰好发生在登记之后、处理协程开始之前）不触发任何回调，
// Start 立即返回；重复 Start 也不会再次分发回调。
func TestConnectionClosedBeforeStartHasNoCallbacks(t *testing.T) {
	h := newRecordingHandler()
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	c := NewConnection(serverSide, newTestProtocol(), h)
	c.Close()

	done := make(chan struct{})
	go func() {
		c.Start()
		c.Start()
		close(done)
	}()
	recv(t, done, "Start returning for a closed connection")
	if ev := h.eventLog(); len(ev) != 0 {
		t.Fatalf("callbacks for connection closed before Start: %v", ev)
	}
	if errs := drainErrs(h); len(errs) != 0 {
		t.Fatalf("errors reported for connection closed before Start: %v", errs)
	}
}

// 对端关闭：恰好上报一次 ErrConnectionClosed；本端关闭（Connection.Close / Server.Stop）：不上报任何错误。
// （OnClose 在所有循环退出后才调用，所以收到 OnClose 时该连接的错误都已上报完。）
// 用真实 loopback TCP：net.Pipe 在对端关闭后 SetReadDeadline 也会失败，与 TCP 行为不同。
func TestCloseReportsConnectionClosedAtMostOnce(t *testing.T) {
	connErrs := func(h *recordingHandler, c *Connection) []errEvent {
		var out []errEvent
		for _, e := range drainErrs(h) {
			if e.conn == c {
				out = append(out, e)
			}
		}
		return out
	}

	t.Run("peer close", func(t *testing.T) {
		h := newRecordingHandler()
		rs := startServer(t, h, nil)
		client := rs.dial(t)
		conn := recv(t, h.connects, "OnConnect")
		_ = client.Close()
		recv(t, h.closes, "OnClose")
		errs := connErrs(h, conn)
		if len(errs) != 1 || errs[0].err != tcperrors.ErrConnectionClosed {
			t.Fatalf("errors on peer close = %v, want exactly one ErrConnectionClosed", errs)
		}
	})
	t.Run("local Connection.Close", func(t *testing.T) {
		h := newRecordingHandler()
		rs := startServer(t, h, nil)
		rs.dial(t)
		conn := recv(t, h.connects, "OnConnect")
		conn.Close()
		recv(t, h.closes, "OnClose")
		if errs := connErrs(h, conn); len(errs) != 0 {
			t.Fatalf("errors on local close = %v, want none", errs)
		}
		if got := conn.GetMetrics()["total_errors"].(int64); got != 0 {
			t.Fatalf("total_errors after local close = %d, want 0", got)
		}
	})
	t.Run("server Stop", func(t *testing.T) {
		h := newRecordingHandler()
		rs := startServer(t, h, nil)
		rs.dial(t)
		conn := recv(t, h.connects, "OnConnect")
		rs.Stop()
		recv(t, h.closes, "OnClose")
		if errs := connErrs(h, conn); len(errs) != 0 {
			t.Fatalf("errors on server Stop = %v, want none", errs)
		}
	})
}
