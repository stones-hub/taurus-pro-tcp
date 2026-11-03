package tcp

import (
	"bytes"
	stderrors "errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	tcperrors "github.com/stones-hub/taurus-pro-tcp/pkg/tcp/errors"
	binaryproto "github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol/binary"
	jsonproto "github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol/json"
)

func expectEchoes(t *testing.T, r *frameReader, seqs ...uint32) {
	t.Helper()
	for _, want := range seqs {
		m := r.next(t)
		if m.Sequence != want {
			t.Fatalf("echo sequence = %d, want %d", m.Sequence, want)
		}
	}
}

// drainErrs 取出目前已记录的全部错误事件。
func drainErrs(h *recordingHandler) []errEvent {
	var out []errEvent
	for {
		select {
		case e := <-h.errs:
			out = append(out, e)
		default:
			return out
		}
	}
}

func countErrs(h *recordingHandler, target error) int {
	n := 0
	for _, e := range drainErrs(h) {
		if e.err == target {
			n++
		}
	}
	return n
}

// 一次 Read 收到多帧时必须全部分发。修复前只解析第一帧，后两帧要等对端再发数据，
// 而对端在等回复，于是永久卡住（这里表现为读取超时）。
func TestReadLoopDispatchesAllFramesFromOneRead(t *testing.T) {
	h := newRecordingHandler()
	h.echo = true
	_, client := startPipeConnection(t, h)

	writeAll(t, client, concat(packFrame(t, 1), packFrame(t, 2), packFrame(t, 3)))
	expectEchoes(t, newFrameReader(client), 1, 2, 3)
}

// 同样的场景走真实 loopback TCP：一次 Write 写入多帧。
func TestServerEchoesPipelinedFramesOverLoopback(t *testing.T) {
	h := newRecordingHandler()
	h.echo = true
	rs := startServer(t, h, nil)
	client := rs.dial(t)

	var frames [][]byte
	var seqs []uint32
	for seq := uint32(1); seq <= 20; seq++ {
		frames = append(frames, packFrame(t, seq))
		seqs = append(seqs, seq)
	}
	writeAll(t, client, concat(frames...))
	expectEchoes(t, newFrameReader(client), seqs...)
}

// 一帧拆成多次 Read，以及"帧尾 + 下一帧开头"混在同一次 Read 里。
func TestReadLoopReassemblesSplitFrames(t *testing.T) {
	h := newRecordingHandler()
	h.echo = true
	_, client := startPipeConnection(t, h)
	r := newFrameReader(client)

	f1 := packFrame(t, 1)
	// 每次 Write 在 net.Pipe 上都是一次独立的 Read：先只给 3 字节头，再给剩下的头，再给 body
	writeAll(t, client, f1[:3])
	writeAll(t, client, f1[3:jsonproto.HeaderSize])
	writeAll(t, client, f1[jsonproto.HeaderSize:])
	expectEchoes(t, r, 1)
	if n := countErrs(h, tcperrors.ErrShortRead); n < 1 {
		t.Fatalf("ErrShortRead reported %d times for a split frame, want >= 1 (existing behaviour)", n)
	}

	f2, f3, f4 := packFrame(t, 2), packFrame(t, 3), packFrame(t, 4)
	half := len(f3) / 2
	writeAll(t, client, concat(f2, f3[:half]))
	writeAll(t, client, concat(f3[half:], f4))
	expectEchoes(t, r, 2, 3, 4)
}

// 非法数据只丢弃该丢弃的部分，同一次 Read 中后面的合法帧照常处理。
func TestReadLoopSkipsInvalidFramesAndContinues(t *testing.T) {
	h := newRecordingHandler()
	h.echo = true
	_, client := startPipeConnection(t, h)

	garbage := []byte("garbage-before-magic")
	badJSON := rawFrame([]byte("{not json"))
	missingFields := rawFrame([]byte(`{"type":0,"sequence":0,"data":{},"timestamp":0}`))
	writeAll(t, client, concat(garbage, packFrame(t, 1), badJSON, missingFields, packFrame(t, 2)))
	expectEchoes(t, newFrameReader(client), 1, 2)

	invalid, protocolErr := 0, 0
	for _, e := range drainErrs(h) {
		if e.conn == nil {
			t.Fatalf("connection-level error %v reported with nil conn", e.err)
		}
		switch {
		case e.err == tcperrors.ErrInvalidFormat:
			invalid++
		case tcperrors.IsProtocolError(e.err):
			protocolErr++
		}
	}
	// 魔数前的垃圾 + 缺字段的帧各一次 ErrInvalidFormat，JSON 解析失败一次协议错误
	if invalid != 2 || protocolErr != 1 {
		t.Fatalf("got %d ErrInvalidFormat and %d other protocol errors, want 2 and 1", invalid, protocolErr)
	}
}

// 超过协议上限的完整帧被整帧丢弃，后面的合法帧照常处理。
func TestReadLoopDropsOversizedFrameAndContinues(t *testing.T) {
	h := newRecordingHandler()
	h.echo = true
	_, client := startPipeConnectionWithProtocol(t, h, jsonproto.New(256))

	oversized := rawFrame(make([]byte, 512))
	writeAll(t, client, concat(oversized, packFrame(t, 7)))
	expectEchoes(t, newFrameReader(client), 7)
	if n := countErrs(h, tcperrors.ErrMessageTooLarge); n != 1 {
		t.Fatalf("ErrMessageTooLarge reported %d times, want 1", n)
	}
}

// Send 与 Close 并发：只会返回约定的错误，OnError 拿到的是当前连接而不是 nil，不 panic。
func TestSendConcurrentWithClose(t *testing.T) {
	for i := 0; i < 200; i++ {
		h := newRecordingHandler()
		c, _ := startPipeConnection(t, h)
		var wg sync.WaitGroup
		wg.Add(2)
		var sendErr error
		go func() {
			defer wg.Done()
			sendErr = c.Send(newSeqMessage(1))
		}()
		go func() {
			defer wg.Done()
			c.Close()
		}()
		wg.Wait()
		if sendErr != nil && sendErr != tcperrors.ErrConnectionClosed && sendErr != tcperrors.ErrSendChannelFull {
			t.Fatalf("Send concurrent with Close = %v", sendErr)
		}
		if err := c.Send(newSeqMessage(2)); err != tcperrors.ErrConnectionClosed {
			t.Fatalf("Send after Close = %v, want ErrConnectionClosed", err)
		}
		for _, e := range drainErrs(h) {
			if e.conn == nil {
				t.Fatalf("OnError(%v) called with nil conn from Send", e.err)
			}
		}
	}
}

// 二进制协议：校验失败的帧整帧丢弃，同一次 Read 中后面的合法帧照常处理。
func TestReadLoopBinaryChecksumErrorContinues(t *testing.T) {
	proto := binaryproto.New(1 << 20)
	h := newRecordingHandler()
	h.echo = true
	_, client := startPipeConnectionWithProtocol(t, h, proto)

	pack := func(seq uint32) []byte {
		b, err := proto.Pack(&binaryproto.Message{Version: 1, Type: 2, Sequence: seq, Data: []byte("payload")})
		if err != nil {
			t.Fatalf("pack binary frame: %v", err)
		}
		return b
	}
	bad := pack(1)
	bad[len(bad)-1] ^= 0xFF // 破坏 CRC
	writeAll(t, client, concat(pack(10), bad, []byte{0x00, 0x01}, pack(11)))

	r := &frameReader{conn: client, proto: proto}
	for _, want := range []uint32{10, 11} {
		m := r.nextRaw(t).(*binaryproto.Message)
		if m.Sequence != want || string(m.Data) != "payload" {
			t.Fatalf("binary echo = seq %d data %q, want seq %d", m.Sequence, m.Data, want)
		}
	}
	errs := drainErrs(h)
	checksum, invalid := 0, 0
	for _, e := range errs {
		switch e.err {
		case tcperrors.ErrChecksum:
			checksum++
		case tcperrors.ErrInvalidFormat:
			invalid++
		}
	}
	if checksum != 1 || invalid != 1 {
		t.Fatalf("got %d ErrChecksum and %d ErrInvalidFormat (errors %v), want 1 and 1", checksum, invalid, errs)
	}
}

// 一次 Read 收到的帧数超过速率令牌时：每条消息各消耗一个令牌，超出时上报 ErrRateLimitExceeded
// 并等待，消息不丢弃、按顺序分发，总耗时不低于令牌桶允许的下限（限流没有因为合并读取而放松）。
func TestReadLoopRateLimitsEachFrameInOneRead(t *testing.T) {
	const rate, frames = 20, 40
	h := newRecordingHandler() // 不回显：写循环也会消耗同一个限速器的令牌，这里只看读方向
	begin := time.Now()        // 令牌桶在 NewConnection 时创建并装满
	_, client := startPipeConn(t, h, newTestProtocol(), func(c *Connection) {
		c.silentTime = 5 * time.Millisecond // 只缩短等待令牌时的轮询间隔，不改变速率
	}, WithRateLimit(rate))

	var all [][]byte
	for seq := uint32(1); seq <= frames; seq++ {
		all = append(all, packFrame(t, seq))
	}
	writeAll(t, client, concat(all...))
	for seq := uint32(1); seq <= frames; seq++ {
		m := recv(t, h.messages, "rate-limited message")
		if m.msg.Sequence != seq {
			t.Fatalf("message seq %d, want %d", m.msg.Sequence, seq)
		}
	}
	elapsed := time.Since(begin)

	if n := countErrs(h, tcperrors.ErrRateLimitExceeded); n < 1 {
		t.Fatalf("ErrRateLimitExceeded reported %d times for %d frames at %d/s, want >= 1", n, frames, rate)
	}
	// 读方向需要 frames 个令牌（Read 前 1 个 + 后续每帧 1 个），写循环启动时也取走 1 个；
	// 初始 rate 个令牌，其余按 rate/s 补充，所以至少需要 (frames+1-rate)/rate 秒
	minimum := time.Duration(frames+1-rate) * time.Second / rate
	if elapsed < minimum*9/10 {
		t.Fatalf("%d frames dispatched in %v at %d/s, want >= %v", frames, elapsed, rate, minimum)
	}
}

// newExhaustedLimiterConn 启动一个连接，限速器每小时才补 1 个令牌、初始 1 个，并等读循环在第一次
// Read 前取走这唯一的令牌。之后读循环阻塞在 Read 上，令牌为 0，任何发送都要等令牌。
func newExhaustedLimiterConn(t *testing.T, h *recordingHandler) (*Connection, net.Conn) {
	t.Helper()
	c, client := startPipeConn(t, h, newTestProtocol(), func(c *Connection) {
		c.rateLimiter = rate.NewLimiter(rate.Every(time.Hour), 1)
	})
	recv(t, h.connects, "OnConnect")
	waitUntil(t, "read loop taking the only token", func() bool { return c.rateLimiter.Tokens() < 1 })
	return c, client
}

// 没有消息要发时，写循环不消耗令牌、不上报限速错误（读写共用的令牌已被读方向用完）。
// 修复前写循环每轮先取令牌再等消息，令牌耗尽时每 100ms 误报一次 ErrRateLimitExceeded。
// 这里的"没有误报"只能在一段时间内观察：300ms 内修复前至少误报 2 次，修复后无论观察多久都是 0 次，
// 所以这个窗口不会让正确的实现误判失败。
func TestWriteLoopIdleDoesNotReportRateLimit(t *testing.T) {
	h := newRecordingHandler()
	c, _ := newExhaustedLimiterConn(t, h)
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if errs := drainErrs(h); len(errs) != 0 {
			t.Fatalf("errors with nothing to send = %v, want none", errs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := c.GetMetrics()["total_errors"].(int64); got != 0 {
		t.Fatalf("total_errors with nothing to send = %d, want 0", got)
	}
}

// 有消息在等令牌时本端 Close：写循环立即退出，消息不会被写出，关闭后不再上报限速错误。
// 用回调卡位代替计时：写循环为这条消息上报第一次限速错误时停在 OnError 里，此时 Close，再放行。
// 修复后它醒来就退出，整个过程恰好 1 次限速错误；修复前（关闭后仍空转等令牌）会持续上报，OnClose 也迟迟不来。
func TestWriteLoopExitsPromptlyOnCloseWhileWaitingForToken(t *testing.T) {
	parked := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	var once sync.Once
	h := newRecordingHandler()
	h.onError = func(_ *Connection, err error) {
		if err == tcperrors.ErrRateLimitExceeded {
			once.Do(func() {
				close(parked)
				<-release
			})
		}
	}
	c, client := newExhaustedLimiterConn(t, h)
	if errs := drainErrs(h); len(errs) != 0 {
		t.Fatalf("errors before any send = %v, want none", errs)
	}

	if err := c.Send(newSeqMessage(1)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	recv(t, parked, "write loop reporting rate limit for the queued message")
	c.Close()
	releaseOnce()
	recv(t, h.closes, "OnClose after Close while waiting for a token")

	limited := 0
	for _, e := range drainErrs(h) {
		if e.err != tcperrors.ErrRateLimitExceeded {
			t.Fatalf("unexpected error %v", e.err)
		}
		limited++
	}
	if limited != 1 {
		t.Fatalf("ErrRateLimitExceeded reported %d times, want exactly 1 (the parked one)", limited)
	}
	// 没有令牌，消息不应被写出：对端只会读到关闭
	_ = client.SetReadDeadline(time.Now().Add(testTimeout))
	if n, err := client.Read(make([]byte, 64)); n != 0 || err == nil {
		t.Fatalf("peer read %d bytes, err %v; want 0 bytes and close", n, err)
	}
}

// 默认值下协议上限 10MB、连接上限 1MB：1.5MB 的帧在接收缓冲区超过连接上限时被清空并上报
// ErrBufferOverflow，随后的合法帧照常处理；Send 超过连接上限的消息返回 ErrMessageTooLarge。
func TestFrameBetweenConnectionAndProtocolLimit(t *testing.T) {
	h := newRecordingHandler()
	h.echo = true
	c, client := startPipeConn(t, h, jsonproto.New(10<<20), func(c *Connection) {
		c.rateLimiter = rate.NewLimiter(rate.Inf, 0) // 1.5MB 会分成约 100 次读取，这里不测限速
	})
	if c.maxMessageSize != 1<<20 {
		t.Fatalf("default connection max message size = %d, want 1MB", c.maxMessageSize)
	}

	big := make([]byte, 1536*1024)
	for i := range big {
		big[i] = 'a'
	}
	writeAll(t, client, concat(rawFrame(big), packFrame(t, 9)))
	expectEchoes(t, newFrameReader(client), 9)
	if n := countErrs(h, tcperrors.ErrBufferOverflow); n < 1 {
		t.Fatalf("ErrBufferOverflow reported %d times for a 1.5MB frame, want >= 1", n)
	}

	huge := newSeqMessage(1)
	huge.Data["blob"] = string(big)
	if err := c.Send(huge); err != tcperrors.ErrMessageTooLarge {
		t.Fatalf("Send of a 1.5MB message = %v, want ErrMessageTooLarge", err)
	}
}

// flakyWriteConn 前 failures 次 Write 返回临时错误，并记录每次 SetWriteDeadline / Write 的调用。
type flakyWriteConn struct {
	net.Conn
	mu        sync.Mutex
	failures  int
	writes    int
	deadlines []time.Time
	failed    chan struct{} // 每次返回临时错误时发出信号
}

type tempWriteError struct{}

func (tempWriteError) Error() string   { return "temporary write failure" }
func (tempWriteError) Timeout() bool   { return false }
func (tempWriteError) Temporary() bool { return true }

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
		f.failed <- struct{}{}
		return 0, &net.OpError{Op: "write", Net: "tcp", Err: tempWriteError{}}
	}
	return f.Conn.Write(b)
}

func (f *flakyWriteConn) counts() (int, []time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes, append([]time.Time(nil), f.deadlines...)
}

func startFlakyWriteConn(t *testing.T, h Handler, failures int, retryDelay time.Duration) (*Connection, *flakyWriteConn, net.Conn) {
	t.Helper()
	serverSide, clientSide := net.Pipe()
	fc := &flakyWriteConn{Conn: serverSide, failures: failures, failed: make(chan struct{}, failures)}
	c := NewConnection(fc, newTestProtocol(), h)
	c.baseRetryDelay = retryDelay
	c.maxRetryDelay = retryDelay
	done := make(chan struct{})
	go func() { c.Start(); close(done) }()
	t.Cleanup(func() {
		_ = clientSide.Close()
		c.Close()
		recv(t, done, "connection goroutines exiting")
	})
	return c, fc, clientSide
}

// 临时写错误重试时，每次 Write 之前都重新设置写入期限，且期限向后推；否则超时导致的临时错误
// 之后，重试会因为期限已过而立即再次失败。
func TestWriteRetryRefreshesDeadline(t *testing.T) {
	h := newRecordingHandler()
	c, fc, client := startFlakyWriteConn(t, h, 2, time.Millisecond)
	recv(t, h.connects, "OnConnect")
	if err := c.Send(newSeqMessage(1)); err != nil {
		t.Fatal(err)
	}
	if m := newFrameReader(client).next(t); m.Sequence != 1 {
		t.Fatalf("peer got seq %d after retries, want 1", m.Sequence)
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

// 重试退避期间本端 Close：立即退出，不再设置期限、不再尝试写入（期限不会在关闭后被继续延长）。
func TestWriteRetryStopsOnClose(t *testing.T) {
	h := newRecordingHandler()
	c, fc, _ := startFlakyWriteConn(t, h, 1, time.Hour)
	recv(t, h.connects, "OnConnect")
	if err := c.Send(newSeqMessage(1)); err != nil {
		t.Fatal(err)
	}
	recv(t, fc.failed, "first write failing with a temporary error")
	c.Close()
	recv(t, h.closes, "OnClose during retry backoff")
	writes, deadlines := fc.counts()
	if writes != 1 || len(deadlines) != 1 {
		t.Fatalf("after Close during backoff: %d writes, %d SetWriteDeadline calls; want 1 and 1", writes, len(deadlines))
	}
}

// Connection 为 nil 时 LocalAddr / RemoteAddr 不 panic。
func TestNilConnectionAddrs(t *testing.T) {
	var c *Connection
	if c.LocalAddr() != nil || c.RemoteAddr() != "" {
		t.Fatalf("nil Connection addrs = %v / %q", c.LocalAddr(), c.RemoteAddr())
	}
}

// writeStep 定义脚本化连接某一次 Write 的行为：可以把 b 的一部分真正写进底层连接，再返回任意 (n, err)。
type writeStep func(under net.Conn, b []byte) (int, error)

// scriptedWriteConn 按顺序执行 steps 里的 Write 行为，用完后（或 repeat 为 true 时一直用最后一步）。
type scriptedWriteConn struct {
	net.Conn
	mu        sync.Mutex
	steps     []writeStep
	repeat    bool
	writes    int
	deadlines int
	stepped   chan struct{} // 每执行一个脚本步骤发一次信号
}

func newScriptedWriteConn(under net.Conn, repeat bool, steps ...writeStep) *scriptedWriteConn {
	return &scriptedWriteConn{Conn: under, steps: steps, repeat: repeat, stepped: make(chan struct{}, 64)}
}

func (s *scriptedWriteConn) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	s.deadlines++
	s.mu.Unlock()
	return s.Conn.SetWriteDeadline(t)
}

func (s *scriptedWriteConn) Write(b []byte) (int, error) {
	s.mu.Lock()
	s.writes++
	var step writeStep
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
	defer func() {
		select {
		case s.stepped <- struct{}{}:
		default:
		}
	}()
	return step(s.Conn, b)
}

func (s *scriptedWriteConn) counts() (writes, deadlines int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes, s.deadlines
}

var errTempWrite = &net.OpError{Op: "write", Net: "tcp", Err: tempWriteError{}}

// writeHalfThenTemp 写出前一半后返回临时错误（例如写期限到达时 poll 层返回的已写字节数 + 超时）。
func writeHalfThenTemp(under net.Conn, b []byte) (int, error) {
	n, _ := under.Write(b[:len(b)/2])
	return n, errTempWrite
}

// writeAllThenTemp 全部写出，但同时返回临时错误。
func writeAllThenTemp(under net.Conn, b []byte) (int, error) {
	n, _ := under.Write(b)
	return n, errTempWrite
}

// fixedFrame 打包一条时间戳固定的消息，使测试能精确比较对端收到的字节流。
func fixedFrame(t *testing.T, seq uint32) ([]byte, *jsonproto.Message) {
	t.Helper()
	m := newSeqMessage(seq)
	m.Timestamp = int64(seq)
	b, err := newTestProtocol().Pack(m)
	if err != nil {
		t.Fatal(err)
	}
	return b, m
}

func startScriptedConn(t *testing.T, h Handler, sc func(net.Conn) *scriptedWriteConn, retryDelay time.Duration) (*Connection, *scriptedWriteConn, net.Conn) {
	t.Helper()
	serverSide, clientSide := net.Pipe()
	wc := sc(serverSide)
	c := NewConnection(wc, newTestProtocol(), h)
	c.baseRetryDelay = retryDelay
	c.maxRetryDelay = retryDelay
	done := make(chan struct{})
	go func() { c.Start(); close(done) }()
	t.Cleanup(func() {
		_ = clientSide.Close()
		c.Close()
		recv(t, done, "connection goroutines exiting")
	})
	return c, wc, clientSide
}

// readExactly 从对端读取恰好 n 个字节。
func readExactly(t *testing.T, conn net.Conn, n int) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(testTimeout))
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("peer read %d bytes: %v", n, err)
	}
	return buf
}

// expectStreamEnds 断言对端在本端关闭后读不到任何多余字节。
func expectStreamEnds(t *testing.T, c *Connection, peer net.Conn) {
	t.Helper()
	c.Close()
	_ = peer.SetReadDeadline(time.Now().Add(testTimeout))
	rest, _ := io.ReadAll(peer)
	if len(rest) != 0 {
		t.Fatalf("peer received %d extra bytes after the expected frames: %q", len(rest), rest)
	}
}

// 第一次 Write 只写出半帧并返回临时错误：重试只发剩下的一半。对端收到的字节流恰好是完整的
// seq1 和 seq2，没有重复的前缀；发送统计是实际写出的字节数。
// 修复前重试从帧开头整帧重发，对端收到"半帧 + 整帧 + 整帧"，帧边界错位。
func TestWriteRetrySendsOnlyRemainderAfterPartialWrite(t *testing.T) {
	h := newRecordingHandler()
	c, wc, peer := startScriptedConn(t, h, func(u net.Conn) *scriptedWriteConn {
		return newScriptedWriteConn(u, false, writeHalfThenTemp)
	}, time.Millisecond)
	recv(t, h.connects, "OnConnect")

	f1, m1 := fixedFrame(t, 1)
	f2, m2 := fixedFrame(t, 2)
	if err := c.Send(m1); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(m2); err != nil {
		t.Fatal(err)
	}
	got := readExactly(t, peer, len(f1)+len(f2))
	if !bytes.Equal(got, concat(f1, f2)) {
		t.Fatalf("peer stream = %q, want exactly frame1+frame2 %q", got, concat(f1, f2))
	}
	waitUntil(t, "two messages counted", func() bool { return c.GetMetrics()["messages_sent"].(int64) == 2 })
	if got := c.GetMetrics()["bytes_sent"].(int64); got != int64(len(f1)+len(f2)) {
		t.Fatalf("bytes_sent = %d, want %d", got, len(f1)+len(f2))
	}
	if writes, deadlines := wc.counts(); writes != 3 || deadlines != 3 {
		t.Fatalf("%d writes, %d deadlines; want 3 and 3 (half, remainder, frame2)", writes, deadlines)
	}
	expectStreamEnds(t, c, peer)
}

// Write 已写出全部字节但同时返回临时错误：这一帧不再重发。修复前会整帧重发，对端收到两次 seq1。
func TestWriteNotResentWhenFullyWrittenWithTemporaryError(t *testing.T) {
	h := newRecordingHandler()
	c, _, peer := startScriptedConn(t, h, func(u net.Conn) *scriptedWriteConn {
		return newScriptedWriteConn(u, false, writeAllThenTemp)
	}, time.Millisecond)
	recv(t, h.connects, "OnConnect")

	f1, m1 := fixedFrame(t, 1)
	f2, m2 := fixedFrame(t, 2)
	_ = c.Send(m1)
	_ = c.Send(m2)
	got := readExactly(t, peer, len(f1)+len(f2))
	if !bytes.Equal(got, concat(f1, f2)) {
		t.Fatalf("peer stream = %q, want exactly frame1+frame2", got)
	}
	waitUntil(t, "two messages counted", func() bool { return c.GetMetrics()["messages_sent"].(int64) == 2 })
	expectStreamEnds(t, c, peer)
}

// Write 一直返回 (0, nil)：零进展按重试计，用尽 maxRetryCount 后上报 io.ErrShortWrite 并关闭连接，
// 不会无限循环，也不会把这条消息记为已发送。修复前 (0, nil) 被当作发送成功。
func TestWriteZeroProgressIsBounded(t *testing.T) {
	h := newRecordingHandler()
	zero := func(net.Conn, []byte) (int, error) { return 0, nil }
	c, wc, _ := startScriptedConn(t, h, func(u net.Conn) *scriptedWriteConn {
		return newScriptedWriteConn(u, true, zero)
	}, time.Millisecond)
	recv(t, h.connects, "OnConnect")
	_ = c.Send(newSeqMessage(1))
	recv(t, h.closes, "OnClose after zero-progress retries are exhausted")

	if writes, _ := wc.counts(); writes != c.maxRetryCount+1 {
		t.Fatalf("%d writes, want %d (1 + maxRetryCount)", writes, c.maxRetryCount+1)
	}
	shortWrites := 0
	for _, e := range drainErrs(h) {
		if stderrors.Is(e.err, io.ErrShortWrite) {
			shortWrites++
		}
	}
	if shortWrites != c.maxRetryCount+1 {
		t.Fatalf("io.ErrShortWrite reported %d times, want %d", shortWrites, c.maxRetryCount+1)
	}
	if got := c.GetMetrics()["messages_sent"].(int64); got != 0 {
		t.Fatalf("messages_sent = %d, want 0", got)
	}
}

// Write 报告的字节数大于给它的长度：视为底层连接异常，上报错误并关闭，不记为已发送。
func TestWriteInvalidCountClosesConnection(t *testing.T) {
	h := newRecordingHandler()
	tooMany := func(_ net.Conn, b []byte) (int, error) { return len(b) + 5, nil }
	c, _, _ := startScriptedConn(t, h, func(u net.Conn) *scriptedWriteConn {
		return newScriptedWriteConn(u, false, tooMany)
	}, time.Millisecond)
	recv(t, h.connects, "OnConnect")
	_ = c.Send(newSeqMessage(1))
	recv(t, h.closes, "OnClose after invalid write count")
	if errs := drainErrs(h); len(errs) != 1 {
		t.Fatalf("errors = %v, want exactly one invalid-count error", errs)
	}
	if got := c.GetMetrics()["messages_sent"].(int64); got != 0 {
		t.Fatalf("messages_sent = %d, want 0", got)
	}
}

// 部分写出后在重试退避期间本端 Close：立即中止，不再写剩余部分、不再刷新期限。
func TestPartialWriteRetryAbortsOnClose(t *testing.T) {
	h := newRecordingHandler()
	c, wc, _ := startScriptedConn(t, h, func(u net.Conn) *scriptedWriteConn {
		// 对端不读，半帧写进 net.Pipe 会阻塞；这里只返回"写了 1 字节"而不真正写，模拟部分写出
		return newScriptedWriteConn(u, false, func(_ net.Conn, b []byte) (int, error) { return 1, errTempWrite })
	}, time.Hour)
	recv(t, h.connects, "OnConnect")
	_ = c.Send(newSeqMessage(1))
	recv(t, wc.stepped, "partial write")
	c.Close()
	recv(t, h.closes, "OnClose during retry backoff")
	if writes, deadlines := wc.counts(); writes != 1 || deadlines != 1 {
		t.Fatalf("after Close: %d writes, %d deadlines; want 1 and 1", writes, deadlines)
	}
}
