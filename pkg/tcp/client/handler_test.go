package client

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol"
	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol/json"
)

// 拨号失败时 Connect 以 nil 连接调用 OnError，未连接就 cleanup 时以 nil 连接调用 OnClose；
// 默认 handler 此前在这两处空指针 panic。
func TestDefaultHandlerNilConnOnDialFailure(t *testing.T) {
	prev := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(prev)

	h := &DefaultHandler{}
	h.OnError(context.Background(), nil, errors.New("x"))
	h.OnClose(context.Background(), nil)

	// 先占一个端口再释放，得到一个确定没人监听的地址
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	c, cleanup, err := New(addr, protocol.JSON, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Connect(); err == nil {
		t.Fatalf("Connect to closed port succeeded")
	}
	cleanup()
}

// startEchoListener 启动一个原样回写的 loopback 服务端。
func startEchoListener(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return l.Addr().String()
}

func newTestClient(t *testing.T, addr string) *Client {
	t.Helper()
	c, _, err := New(addr, protocol.JSON, &quietHandler{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type quietHandler struct{}

func (quietHandler) OnConnect(context.Context, net.Conn)              {}
func (quietHandler) OnMessage(context.Context, net.Conn, interface{}) {}
func (quietHandler) OnClose(context.Context, net.Conn)                {}
func (quietHandler) OnError(context.Context, net.Conn, error)         {}

// Close 之后 SimpleSend / SimpleReceive 返回错误。此前 Close 把 c.conn 置空后，SimpleSend 会空指针
// panic、被 recover 吞掉并返回 nil（调用方以为发送成功），SimpleReceive 则直接 panic。
// 客户端不支持重连：Close 之后 Connect 失败。
func TestClientUseAfterClose(t *testing.T) {
	addr := startEchoListener(t)
	c := newTestClient(t, addr)
	if err := c.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_ = c.Close()

	msg := &json.Message{Type: 1, Sequence: 1, Data: map[string]interface{}{}}
	if err := c.SimpleSend(msg); err == nil {
		t.Fatalf("SimpleSend after Close returned nil, want error")
	}
	if _, err := c.SimpleReceive(); err == nil {
		t.Fatalf("SimpleReceive after Close returned nil error")
	}
	if err := c.Send(msg); err == nil {
		t.Fatalf("Send after Close returned nil, want error")
	}
	if c.RemoteAddr() != nil {
		t.Fatalf("RemoteAddr after Close = %v, want nil", c.RemoteAddr())
	}
	if err := c.Connect(); err == nil {
		t.Fatalf("Connect after Close succeeded; client is single-use")
	}
}

// Start、Send、Close 并发：-race 下不报数据竞争（c.conn 与 sendChan 的读写都已同步），不 panic。
func TestClientConcurrentSendAndClose(t *testing.T) {
	addr := startEchoListener(t)
	for i := 0; i < 50; i++ {
		c := newTestClient(t, addr)
		if err := c.Connect(); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		started := make(chan struct{})
		go func() {
			defer close(started)
			c.Start()
		}()
		var wg sync.WaitGroup
		for j := 0; j < 4; j++ {
			wg.Add(1)
			go func(seq uint32) {
				defer wg.Done()
				for k := 0; k < 20; k++ {
					_ = c.Send(&json.Message{Type: 1, Sequence: seq, Data: map[string]interface{}{}})
					_ = c.RemoteAddr()
				}
			}(uint32(j + 1))
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Close()
		}()
		wg.Wait()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: client Start did not return after Close", i)
		}
	}
}
