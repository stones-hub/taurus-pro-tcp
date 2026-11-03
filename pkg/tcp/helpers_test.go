package tcp

import (
	"context"
	stderrors "errors"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	tcperrors "github.com/stones-hub/taurus-pro-tcp/pkg/tcp/errors"
	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol"
	jsonproto "github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol/json"
)

// testTimeout 是所有等待事件的上限，超过即判定失败，而不是靠 sleep 等结果出现。
const testTimeout = 5 * time.Second

func TestMain(m *testing.M) {
	// 库在每条消息上都打日志，测试时丢弃，设置 TCP_TEST_LOG=1 可以打开
	if os.Getenv("TCP_TEST_LOG") == "" {
		log.SetOutput(io.Discard)
	}
	os.Exit(m.Run())
}

type errEvent struct {
	conn *Connection
	err  error
}

type msgEvent struct {
	conn *Connection
	msg  *jsonproto.Message // JSON 协议时为解析出的消息, 其他协议为 nil
	raw  interface{}
}

// recordingHandler 记录所有回调，可选地把事件委托给 inner（例如默认 handler）并回显消息。
type recordingHandler struct {
	inner Handler
	echo  bool

	// 可选的阻塞点，用于构造"用户回调不返回"的场景
	onMessage func(c *Connection, m interface{})
	onClose   func(c *Connection)
	onError   func(c *Connection, err error)

	connects chan *Connection
	messages chan msgEvent
	closes   chan *Connection
	errs     chan errEvent

	// 每个连接按顺序记录的回调种类（connect/message/close），用于断言回调顺序
	mu     sync.Mutex
	events map[uint64][]string
}

func (h *recordingHandler) record(c *Connection, kind string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.events == nil {
		h.events = make(map[uint64][]string)
	}
	h.events[c.ID()] = append(h.events[c.ID()], kind)
}

// eventLog 返回所有连接的回调记录副本。
func (h *recordingHandler) eventLog() map[uint64][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[uint64][]string, len(h.events))
	for id, ev := range h.events {
		out[id] = append([]string(nil), ev...)
	}
	return out
}

func newRecordingHandler() *recordingHandler {
	return &recordingHandler{
		connects: make(chan *Connection, 1024),
		messages: make(chan msgEvent, 1024),
		closes:   make(chan *Connection, 1024),
		errs:     make(chan errEvent, 4096),
	}
}

func (h *recordingHandler) OnConnect(c *Connection) {
	h.record(c, "connect")
	if h.inner != nil {
		h.inner.OnConnect(c)
	}
	h.connects <- c
}

func (h *recordingHandler) OnMessage(c *Connection, m interface{}) {
	h.record(c, "message")
	if h.inner != nil {
		h.inner.OnMessage(c, m)
	}
	if h.onMessage != nil {
		h.onMessage(c, m)
	}
	jm, _ := m.(*jsonproto.Message)
	h.messages <- msgEvent{conn: c, msg: jm, raw: m}
	if h.echo {
		_ = c.Send(m)
	}
}

func (h *recordingHandler) OnClose(c *Connection) {
	h.record(c, "close")
	if h.inner != nil {
		h.inner.OnClose(c)
	}
	if h.onClose != nil {
		h.onClose(c)
	}
	h.closes <- c
}

func (h *recordingHandler) OnError(c *Connection, err error) {
	if h.inner != nil {
		h.inner.OnError(c, err)
	}
	if h.onError != nil {
		h.onError(c, err)
	}
	// 错误可能高频出现（例如连接数满时每个静默周期一次），满了就丢弃，不阻塞服务器协程
	select {
	case h.errs <- errEvent{conn: c, err: err}:
	default:
	}
}

// eventCounts 是 consume 统计到的回调次数。
type eventCounts struct{ connects, messages, closes int }

// consume 启动一个协程持续取走 connects/messages/closes 通道里的事件并计数，
// 让高负载测试不会因为事件通道写满而把服务器回调卡住（那样的失败看起来像产品死锁）。
// 返回的函数停止消费并返回最终计数：调用时所有回调都应已返回（例如 StopContext 成功之后），
// 它会再取走通道里剩下的事件，不丢弃任何事件。使用 consume 的测试不能再直接读这三个通道。
// 停止动作同时注册到 t.Cleanup（用 once 保证只执行一次），测试中途失败时消费协程也会退出。
func (h *recordingHandler) consume(t *testing.T) func() eventCounts {
	var n eventCounts
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-h.connects:
				n.connects++
			case <-h.messages:
				n.messages++
			case <-h.closes:
				n.closes++
			case <-stop:
				for {
					select {
					case <-h.connects:
						n.connects++
					case <-h.messages:
						n.messages++
					case <-h.closes:
						n.closes++
					default:
						return
					}
				}
			}
		}
	}()
	var once sync.Once
	finish := func() {
		once.Do(func() {
			close(stop)
			<-done
		})
	}
	t.Cleanup(finish)
	return func() eventCounts {
		finish()
		return n
	}
}

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

// waitErr 等待一个满足 match 的错误事件。
func (h *recordingHandler) waitErr(t *testing.T, what string, match func(errEvent) bool) errEvent {
	t.Helper()
	deadline := time.After(testTimeout)
	for {
		select {
		case e := <-h.errs:
			if match(e) {
				return e
			}
		case <-deadline:
			t.Fatalf("timed out after %v waiting for error %s", testTimeout, what)
		}
	}
}

func newTestProtocol() *jsonproto.Protocol {
	return jsonproto.New(1 << 20)
}

func newSeqMessage(seq uint32) *jsonproto.Message {
	return &jsonproto.Message{
		Type:     1,
		Sequence: seq,
		Data:     map[string]interface{}{"seq": float64(seq)},
	}
}

func packFrame(t *testing.T, seq uint32) []byte {
	t.Helper()
	b, err := newTestProtocol().Pack(newSeqMessage(seq))
	if err != nil {
		t.Fatalf("pack frame %d: %v", seq, err)
	}
	return b
}

// rawFrame 构造一个 JSON 协议帧，body 原样写入，用来制造非法帧。
func rawFrame(body []byte) []byte {
	b := make([]byte, jsonproto.HeaderSize+len(body))
	b[0], b[1] = 0xFE, 0xFF
	n := uint32(len(body))
	b[2], b[3], b[4], b[5] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	copy(b[jsonproto.HeaderSize:], body)
	return b
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// frameReader 从客户端连接读取并解析服务器发回的帧。
type frameReader struct {
	conn  net.Conn
	proto protocol.Protocol
	buf   []byte
}

func newFrameReader(conn net.Conn) *frameReader {
	return &frameReader{conn: conn, proto: newTestProtocol()}
}

// next 读取下一条 JSON 消息。
func (r *frameReader) next(t *testing.T) *jsonproto.Message {
	t.Helper()
	return r.nextRaw(t).(*jsonproto.Message)
}

// nextRaw 用 reader 的协议读取下一条消息。
func (r *frameReader) nextRaw(t *testing.T) interface{} {
	t.Helper()
	if err := r.conn.SetReadDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	tmp := make([]byte, 4096)
	for {
		if len(r.buf) > 0 {
			m, n, err := r.proto.Unpack(r.buf)
			if err == nil {
				r.buf = r.buf[n:]
				return m
			}
			if err != tcperrors.ErrShortRead {
				t.Fatalf("client unpack: %v", err)
			}
		}
		n, err := r.conn.Read(tmp)
		if err != nil {
			t.Fatalf("client read: %v", err)
		}
		r.buf = append(r.buf, tmp[:n]...)
	}
}

// expectClosed 断言对端已关闭连接：Read 在期限内返回错误（EOF 或 reset），而不是超时。
func expectClosed(t *testing.T, conn net.Conn, what string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 1024)
	for {
		_, err := conn.Read(buf)
		if err == nil {
			continue
		}
		var ne net.Error
		if stderrors.As(err, &ne) && ne.Timeout() {
			t.Fatalf("%s: connection still open after %v", what, testTimeout)
		}
		return
	}
}

// waitUntil 以短间隔检查条件，直到成立或超时失败。
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", testTimeout, what)
		}
		time.Sleep(time.Millisecond)
	}
}

// listenAddr 返回服务器实际监听的地址，尚未监听时返回 ""。
func (s *Server) listenAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

type runningServer struct {
	*Server
	addr     string
	startErr chan error
}

// startServer 在 127.0.0.1 随机端口启动服务器。configure 在 Start 之前调用，可以修改未导出字段。
// 测试结束时有界地停止服务器，并断言 Start 返回 nil。
func startServer(t *testing.T, h Handler, configure func(*Server), opts ...ServerOption) *runningServer {
	t.Helper()
	s, _, err := NewServer("127.0.0.1:0", newTestProtocol(), h, opts...)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if configure != nil {
		configure(s)
	}
	rs := &runningServer{Server: s, startErr: make(chan error, 1)}
	go func() { rs.startErr <- s.Start() }()
	waitUntil(t, "server listening", func() bool { return s.listenAddr() != "" })
	rs.addr = s.listenAddr()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		if err := s.StopContext(ctx); err != nil {
			t.Errorf("cleanup StopContext: %v", err)
		}
		select {
		case err := <-rs.startErr:
			if err != nil {
				t.Errorf("Start returned %v, want nil", err)
			}
		case <-time.After(testTimeout):
			t.Errorf("Start did not return after StopContext")
		}
	})
	return rs
}

func (rs *runningServer) dial(t *testing.T) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", rs.addr, testTimeout)
	if err != nil {
		t.Fatalf("dial %s: %v", rs.addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func (rs *runningServer) stat(key string) int64 {
	return rs.GetMetrics()[key].(int64)
}

func writeAll(t *testing.T, conn net.Conn, b []byte) {
	t.Helper()
	if err := conn.SetWriteDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatalf("set write deadline: %v", err)
	}
	if _, err := conn.Write(b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// startPipeConnection 用 net.Pipe 启动一个服务端 Connection。
// net.Pipe 的一次 Write 恰好被对端的一次 Read 读完（数据小于读缓冲区时），
// 所以"一次 Read 收到多帧"和"一帧分多次 Read"都是确定的，不依赖内核如何合并 TCP 段。
func startPipeConnection(t *testing.T, h Handler) (*Connection, net.Conn) {
	t.Helper()
	return startPipeConn(t, h, newTestProtocol(), nil)
}

func startPipeConnectionWithProtocol(t *testing.T, h Handler, proto protocol.Protocol) (*Connection, net.Conn) {
	t.Helper()
	return startPipeConn(t, h, proto, nil)
}

// startPipeConn 创建并启动连接。configure 在 Start 之前调用，可以修改未导出字段。
func startPipeConn(t *testing.T, h Handler, proto protocol.Protocol, configure func(*Connection), opts ...ConnectionOption) (*Connection, net.Conn) {
	t.Helper()
	serverSide, clientSide := net.Pipe()
	c := NewConnection(serverSide, proto, h, opts...)
	if configure != nil {
		configure(c)
	}
	done := make(chan struct{})
	go func() {
		c.Start()
		close(done)
	}()
	t.Cleanup(func() {
		_ = clientSide.Close()
		c.Close()
		select {
		case <-done:
		case <-time.After(testTimeout):
			t.Errorf("connection goroutines did not exit")
		}
	})
	return c, clientSide
}
