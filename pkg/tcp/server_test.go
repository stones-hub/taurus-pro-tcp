package tcp

import (
	"context"
	stderrors "errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tcperrors "github.com/stones-hub/taurus-pro-tcp/pkg/tcp/errors"
	jsonproto "github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol/json"
)

func TestDefaultHandlerAcceptsNilConn(t *testing.T) {
	h := GetHandler("tcp-test-handler-not-registered")
	if _, ok := h.(*defaultHandler); !ok {
		t.Fatalf("GetHandler for unknown name = %T, want *defaultHandler", h)
	}
	// 服务器级别的错误以 nil 连接回调 OnError，此前默认 handler 在这里空指针 panic
	h.OnError(nil, tcperrors.ErrTooManyConnections)
	h.OnError(nil, tcperrors.ErrSystemFatal)
	h.OnConnect(nil)
	h.OnClose(nil)
	h.OnMessage(nil, "x")

	var c *Connection
	if got := c.RemoteAddr(); got != "" {
		t.Fatalf("nil Connection RemoteAddr() = %q, want empty", got)
	}
}

var registerSeq int64

func TestRegisterHandlerKeepsFirstAndFallsBack(t *testing.T) {
	// 注册表是包级的，-count>1 重复运行时名字必须每次不同
	name := fmt.Sprintf("tcp-test-register-%d", atomic.AddInt64(&registerSeq, 1))
	first := newRecordingHandler()
	second := newRecordingHandler()
	RegisterHandler(name, first)
	RegisterHandler(name, second)
	if got := GetHandler(name); got != Handler(first) {
		t.Fatalf("GetHandler after duplicate register = %p, want first handler %p", got, first)
	}
	if _, ok := GetHandler("tcp-test-register-missing").(*defaultHandler); !ok {
		t.Fatalf("unknown name must fall back to default handler")
	}

	// 注册表可以并发读写（race 检测下验证）
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			RegisterHandler("tcp-test-register-concurrent", first)
		}()
		go func() {
			defer wg.Done()
			_ = GetHandler("tcp-test-register-concurrent")
		}()
	}
	wg.Wait()
}

// 开满 max_connections=2 后再连第 3 个：使用默认 handler（只包一层记录事件），
// 证明服务器不崩溃、拒绝可以从 OnError 和指标观察到、已有连接继续服务，
// 并且槽位释放后排队的第 3 个连接会被接入。
func TestMaxConnectionsThirdConnectionWithDefaultHandler(t *testing.T) {
	h := newRecordingHandler()
	h.inner = GetHandler("tcp-test-default-for-max-conns")
	if _, ok := h.inner.(*defaultHandler); !ok {
		t.Fatalf("expected default handler, got %T", h.inner)
	}
	rs := startServer(t, h, func(s *Server) {
		s.silentTime = 10 * time.Millisecond // 缩短满载时的轮询间隔，只影响速度不影响判定
	}, WithMaxConnections(2))

	c1 := rs.dial(t)
	conn1 := recv(t, h.connects, "first OnConnect")
	rs.dial(t)
	recv(t, h.connects, "second OnConnect")

	// 拒绝以 nil 连接上报给（默认）handler，可以观察到
	h.waitErr(t, "ErrTooManyConnections with nil conn", func(e errEvent) bool {
		return e.conn == nil && e.err == tcperrors.ErrTooManyConnections
	})

	// 第 3 个连接的 TCP 握手由内核完成，但服务器没有空闲槽位，不会 Accept 它。
	// 拒绝计数在 Dial 返回之后读取，之后再增加的拒绝一定发生在第 3 个连接已经排队之后：
	// 那时 acceptLoop 拿不到槽位，不可能接入它
	c3 := rs.dial(t)
	base := rs.stat("connection_refused")
	waitUntil(t, "two more refusals after the third dial", func() bool {
		return rs.stat("connection_refused") >= base+2
	})
	if got := rs.stat("total_connections"); got != 2 {
		t.Fatalf("total_connections = %d while full, want 2", got)
	}
	if got := rs.ConnectionCount(); got != 2 {
		t.Fatalf("ConnectionCount() = %d while full, want 2", got)
	}

	// 服务仍然活着：已有连接的消息照常被处理
	writeAll(t, c1, packFrame(t, 1))
	m := recv(t, h.messages, "message on first connection")
	if m.conn != conn1 || m.msg.Sequence != 1 {
		t.Fatalf("got message seq %d on conn %d, want seq 1 on conn %d", m.msg.Sequence, m.conn.ID(), conn1.ID())
	}

	// 释放一个槽位后，排队的第 3 个连接被接入并能收发
	_ = c1.Close()
	if closed := recv(t, h.closes, "OnClose of first connection"); closed != conn1 {
		t.Fatalf("closed conn %d, want %d", closed.ID(), conn1.ID())
	}
	conn3 := recv(t, h.connects, "OnConnect of queued third connection")
	writeAll(t, c3, packFrame(t, 3))
	m = recv(t, h.messages, "message on third connection")
	if m.conn != conn3 || m.msg.Sequence != 3 {
		t.Fatalf("got message seq %d on conn %d, want seq 3 on conn %d", m.msg.Sequence, m.conn.ID(), conn3.ID())
	}
	if got := rs.stat("total_connections"); got != 3 {
		t.Fatalf("total_connections = %d, want 3", got)
	}
}

func TestStopBeforeStart(t *testing.T) {
	s, cleanup, err := NewServer("127.0.0.1:0", newTestProtocol(), newRecordingHandler())
	if err != nil {
		t.Fatal(err)
	}
	s.Stop()
	cleanup() // 重复调用安全
	if err := s.Start(); err != tcperrors.ErrServerStopped {
		t.Fatalf("Start after Stop = %v, want ErrServerStopped", err)
	}
	if s.listenAddr() != "" {
		t.Fatalf("server must not listen after Stop")
	}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := s.StopContext(ctx); err != nil {
		t.Fatalf("StopContext on never-started server = %v, want nil", err)
	}
}

func TestStartTwiceAndAfterStop(t *testing.T) {
	h := newRecordingHandler()
	rs := startServer(t, h, nil)
	if err := rs.Start(); err != tcperrors.ErrServerAlreadyStarted {
		t.Fatalf("second Start = %v, want ErrServerAlreadyStarted", err)
	}

	client := rs.dial(t)
	recv(t, h.connects, "OnConnect")

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := rs.StopContext(ctx); err != nil {
		t.Fatalf("StopContext = %v", err)
	}
	rs.Stop() // 重复调用安全
	if err := rs.StopContext(ctx); err != nil {
		t.Fatalf("repeated StopContext = %v", err)
	}

	expectClosed(t, client, "existing connection after stop")
	if c, err := net.DialTimeout("tcp", rs.addr, testTimeout); err == nil {
		_ = c.Close()
		t.Fatalf("dial after stop succeeded, want refused")
	}
	if err := rs.Start(); err != tcperrors.ErrServerStopped {
		t.Fatalf("Start after stop = %v, want ErrServerStopped", err)
	}
	if got := rs.stat("current_connections"); got != 0 {
		t.Fatalf("current_connections after stop = %d, want 0", got)
	}
}

// 用户 OnMessage 不返回时，StopContext 在期限到达时返回真实的超时错误；
// 监听和 socket 已关闭，不再接入新连接；处理协程返回后再次调用即成功。
func TestStopContextDeadlineWithBlockedOnMessage(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	h := newRecordingHandler()
	h.onMessage = func(*Connection, interface{}) {
		close(entered)
		<-release
	}
	rs := startServer(t, h, nil)
	client := rs.dial(t)
	recv(t, h.connects, "OnConnect")
	writeAll(t, client, packFrame(t, 1))
	recv(t, entered, "OnMessage entered")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := rs.StopContext(ctx)
	if !stderrors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StopContext with blocked handler = %v, want DeadlineExceeded", err)
	}

	expectClosed(t, client, "connection whose handler is blocked")
	if c, err := net.DialTimeout("tcp", rs.addr, testTimeout); err == nil {
		_ = c.Close()
		t.Fatalf("dial after StopContext timeout succeeded, want refused")
	}
	select {
	case err := <-rs.startErr:
		t.Fatalf("Start returned %v while a handler is still running", err)
	default:
	}

	close(release)
	ctx2, cancel2 := context.WithTimeout(context.Background(), testTimeout)
	defer cancel2()
	if err := rs.StopContext(ctx2); err != nil {
		t.Fatalf("StopContext after handler returned = %v", err)
	}
}

// 用户 OnClose 阻塞时：Stop 不调用也不等待用户回调，照常返回，并且返回时所有 socket 都已关闭；
// StopContext 先完成同样的关闭，再在期限到达时返回超时错误；回调返回后再次调用即成功。
// （契约变更：旧 Stop 会同步调用 OnClose，OnClose 阻塞时 Stop 也阻塞。）
func TestStopWithBlockedOnClose(t *testing.T) {
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce() // 断言失败时也放开回调，避免清理阶段卡住
	h := newRecordingHandler()
	h.onClose = func(*Connection) { <-release }
	rs := startServer(t, h, nil)

	// 逐个连接并等待各自的 OnConnect，使 clients[i] 与 conns[i] 一一对应
	// （各连接的 OnConnect 在各自协程里执行，同时连接时回调顺序不确定）
	var clients []net.Conn
	var conns []*Connection
	for i := 0; i < 3; i++ {
		clients = append(clients, rs.dial(t))
		conns = append(conns, recv(t, h.connects, "OnConnect"))
	}

	// 第一个连接由对端关闭，它的 OnClose 先卡住
	_ = clients[0].Close()
	waitUntil(t, "first connection transport closed", func() bool { return atomic.LoadInt32(&conns[0].closed) == 1 })

	stopReturned := make(chan struct{})
	go func() {
		rs.Stop()
		close(stopReturned)
	}()
	recv(t, stopReturned, "Stop returning while OnClose is blocked")
	for i, c := range conns {
		if atomic.LoadInt32(&c.closed) != 1 {
			t.Fatalf("connection %d transport still open after Stop returned", i)
		}
	}
	for _, c := range clients[1:] {
		expectClosed(t, c, "connection after Stop while OnClose is blocked")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := rs.StopContext(ctx); !stderrors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StopContext with blocked OnClose = %v, want DeadlineExceeded", err)
	}

	releaseOnce()
	ctx2, cancel2 := context.WithTimeout(context.Background(), testTimeout)
	defer cancel2()
	if err := rs.StopContext(ctx2); err != nil {
		t.Fatalf("StopContext after OnClose released = %v", err)
	}
}

// Accept 成功到登记连接之间发生 Stop：这个连接必须被关闭，不能漏到 Stop 之后继续服务。
func TestStopBetweenAcceptAndRegister(t *testing.T) {
	accepted := make(chan struct{})
	release := make(chan struct{})
	h := newRecordingHandler()
	rs := startServer(t, h, func(s *Server) {
		s.afterAccept = func() {
			close(accepted)
			<-release
		}
	})

	client := rs.dial(t)
	recv(t, accepted, "Accept")
	rs.Stop() // Stop 时 conns 里还没有这个连接
	close(release)

	expectClosed(t, client, "connection accepted during Stop")
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := rs.StopContext(ctx); err != nil {
		t.Fatalf("StopContext = %v", err)
	}
	if got := rs.stat("total_connections"); got != 0 {
		t.Fatalf("total_connections = %d, want 0 (connection must not be registered)", got)
	}
	select {
	case c := <-h.connects:
		t.Fatalf("OnConnect called for connection %d accepted during Stop", c.ID())
	default:
	}
}

// Start/Stop/StopContext 并发与重复调用：不 panic、不死锁、无数据竞争，结果只有约定的几种。
func TestConcurrentStartStop(t *testing.T) {
	for i := 0; i < 50; i++ {
		s, cleanup, err := NewServer("127.0.0.1:0", newTestProtocol(), newRecordingHandler())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		startErr := make(chan error, 1)
		stopErrs := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(5)
		go func() { defer wg.Done(); startErr <- s.Start() }()
		go func() { defer wg.Done(); s.Stop() }()
		go func() { defer wg.Done(); cleanup() }()
		go func() { defer wg.Done(); stopErrs <- s.StopContext(ctx) }()
		go func() {
			defer wg.Done()
			// 并发尝试连接，可能成功也可能被拒绝，只要求不卡住
			if addr := s.listenAddr(); addr != "" {
				if c, err := net.DialTimeout("tcp", addr, testTimeout); err == nil {
					_ = c.Close()
				}
			}
		}()

		allDone := make(chan struct{})
		go func() { wg.Wait(); close(allDone) }()
		select {
		case <-allDone:
		case <-time.After(testTimeout):
			t.Fatalf("iteration %d: Start/Stop did not finish", i)
		}
		if err := s.StopContext(ctx); err != nil {
			t.Fatalf("iteration %d: StopContext = %v", i, err)
		}
		if err := <-stopErrs; err != nil {
			t.Fatalf("iteration %d: concurrent StopContext = %v", i, err)
		}
		if err := <-startErr; err != nil && err != tcperrors.ErrServerStopped {
			t.Fatalf("iteration %d: Start = %v, want nil or ErrServerStopped", i, err)
		}
		if err := s.Start(); err != tcperrors.ErrServerStopped {
			t.Fatalf("iteration %d: Start after stop = %v, want ErrServerStopped", i, err)
		}
		cancel()
	}
}

// 多个客户端持续收发、同时不断有新连接进来时停止服务器：StopContext 在期限内完成，
// 所有客户端都看到连接关闭，没有遗留连接。
func TestStopUnderConnectionPressure(t *testing.T) {
	h := newRecordingHandler()
	h.echo = true
	rs := startServer(t, h, nil, WithMaxConnections(64))
	// 持续取走回调事件并计数：长连接一直收发，事件数量远超通道容量
	counts := h.consume(t)

	const clients = 16
	var echoed sync.WaitGroup
	echoed.Add(clients)
	var clientsDone sync.WaitGroup
	stopDialing := make(chan struct{})
	failures := make(chan string, 64)
	fail := func(msg string) {
		select {
		case failures <- msg:
		default:
		}
	}

	for i := 0; i < clients; i++ {
		conn := rs.dial(t)
		clientsDone.Add(1)
		go func(conn net.Conn) {
			defer clientsDone.Done()
			proto := newTestProtocol()
			first := true
			var buf []byte
			tmp := make([]byte, 4096)
			for seq := uint32(1); ; seq++ {
				frame, err := proto.Pack(newSeqMessage(seq))
				if err != nil {
					fail(err.Error())
					return
				}
				_ = conn.SetDeadline(time.Now().Add(testTimeout))
				if _, err := conn.Write(frame); err != nil {
					return // 服务器停止后写失败是预期的
				}
				for {
					m, n, err := proto.Unpack(buf)
					if err == nil {
						buf = buf[n:]
						if m.(*jsonproto.Message).Sequence != seq {
							fail("echo out of order")
							return
						}
						break
					}
					if err != tcperrors.ErrShortRead {
						fail("client unpack: " + err.Error())
						return
					}
					n, err = conn.Read(tmp)
					if err != nil {
						var ne net.Error
						if stderrors.As(err, &ne) && ne.Timeout() {
							fail("client read timed out: server neither replied nor closed")
						}
						return // 服务器停止后读到 EOF/reset 是预期的
					}
					buf = append(buf, tmp[:n]...)
				}
				if first {
					first = false
					echoed.Done()
				}
			}
		}(conn)
	}

	// 持续发起短连接：连上、完成一次收发、断开、再连，使 acceptLoop 在 Stop 时一直在接入
	var dialers sync.WaitGroup
	var roundTrips int64
	for i := 0; i < 4; i++ {
		dialers.Add(1)
		go func() {
			defer dialers.Done()
			proto := newTestProtocol()
			for {
				select {
				case <-stopDialing:
					return
				default:
				}
				c, err := net.DialTimeout("tcp", rs.addr, testTimeout)
				if err != nil {
					return // 监听关闭后拒绝
				}
				frame, _ := proto.Pack(newSeqMessage(1))
				_ = c.SetDeadline(time.Now().Add(testTimeout))
				var buf []byte
				tmp := make([]byte, 4096)
				ok := false
				if _, err := c.Write(frame); err == nil {
					for {
						if _, _, err := proto.Unpack(buf); err == nil {
							ok = true
							break
						}
						n, err := c.Read(tmp)
						if err != nil {
							var ne net.Error
							if stderrors.As(err, &ne) && ne.Timeout() {
								fail("dialed connection neither served nor closed")
							}
							break // 服务器停止时被关闭或重置
						}
						buf = append(buf, tmp[:n]...)
					}
				}
				_ = c.Close()
				if ok {
					atomic.AddInt64(&roundTrips, 1)
				}
			}
		}()
	}

	waitGroupOrFail(t, &echoed, "every client receiving at least one echo")
	waitUntil(t, "short-lived connections being served", func() bool { return atomic.LoadInt64(&roundTrips) >= 20 })

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := rs.StopContext(ctx); err != nil {
		t.Fatalf("StopContext under load = %v", err)
	}
	close(stopDialing)
	waitGroupOrFail(t, &clientsDone, "clients observing close")
	waitGroupOrFail(t, &dialers, "dialers finishing")
	close(failures)
	for f := range failures {
		t.Error(f)
	}

	if got := rs.stat("current_connections"); got != 0 {
		t.Fatalf("current_connections after stop = %d, want 0", got)
	}
	leaked := 0
	rs.conns.Range(func(_, _ interface{}) bool { leaked++; return true })
	if leaked != 0 {
		t.Fatalf("%d connection(s) left registered after stop", leaked)
	}
	assertCallbackOrder(t, h)

	n := counts()
	if n.connects < clients+20 || n.closes != n.connects {
		t.Fatalf("callbacks: %d connects, %d closes; want >= %d connects and one close per connect", n.connects, n.closes, clients+20)
	}
	if n.messages < clients+20 {
		t.Fatalf("callbacks: %d messages, want >= %d (one echo per client plus the short-lived round trips)", n.messages, clients+20)
	}
}

// assertCallbackOrder 断言每个连接恰好一次 OnConnect、恰好一次 OnClose，且 OnClose 是最后一个回调。
func assertCallbackOrder(t *testing.T, h *recordingHandler) {
	t.Helper()
	for id, ev := range h.eventLog() {
		connects, closes := 0, 0
		for _, e := range ev {
			switch e {
			case "connect":
				connects++
			case "close":
				closes++
			}
		}
		if connects != 1 || closes != 1 || ev[len(ev)-1] != "close" {
			t.Fatalf("connection %d callbacks %v: want exactly one connect and one close, close last", id, ev)
		}
	}
}

func waitGroupOrFail(t *testing.T, wg *sync.WaitGroup, what string) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	recv(t, done, what)
}
