// Copyright (c) 2025 Taurus Team. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Author: yelei
// Email: 61647649@qq.com
// Date: 2025-06-13

package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	tcperr "github.com/stones-hub/taurus-pro-tcp/pkg/tcp/errors"
	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol"
)

// ErrClientClosed 表示客户端已经 Close：之后的 Connect、Send 都返回它。
var ErrClientClosed = errors.New("client closed")

// ErrAlreadyConnected 表示客户端已经连接（客户端只能连接一次）。
var ErrAlreadyConnected = errors.New("client already connected")

// ErrReceiveConflict 表示 SimpleReceive 与 Start 的读协程或另一个 SimpleReceive 同时使用。
var ErrReceiveConflict = errors.New("SimpleReceive cannot be used while Start is running or another SimpleReceive is in progress")

// Client TCP客户端
type Client struct {
	// 基础配置
	address string // 连接的服务端地址
	// 上下文控制
	ctx    context.Context
	cancel context.CancelFunc
	wg     *sync.WaitGroup
	// 连接相关
	connMu   sync.RWMutex // 保护 conn 与 lastConn：Connect 写入、Close 置空, Start 时取一次
	conn     net.Conn
	lastConn net.Conn                                                             // Close 时的连接, 用于 Close 负责的 OnClose 回调
	dial     func(ctx context.Context, network, address string) (net.Conn, error) // 拨号, 默认 net.Dialer, 测试可替换
	protocol protocol.Protocol
	handler  Handler
	// 状态控制
	connected  atomic.Bool
	closeOnce  sync.Once  // 保证传输层只关闭一次
	stateMu    sync.Mutex // 保护以下状态, 用来判定由谁、在什么时候调用 OnClose；任何回调都不在此锁内调用
	started    bool       // Start 是否已经启动读写协程
	closed     bool       // 是否已经 Close
	connecting bool       // Connect 已登记连接、OnConnect 尚未返回
	closeOwed  bool       // OnConnect 执行期间发生了关闭, 由 Connect 在 OnConnect 返回后补调 OnClose
	receiving  bool       // SimpleReceive 正在执行
	recvBuf    []byte     // SimpleReceive 读到但尚未解析完的数据（半帧或多余的帧）

	testHookBeforeOnConnect func()       // 仅测试使用: Connect 登记连接之后、调用 OnConnect 之前调用
	testHookStartCaptured   func()       // 仅测试使用: Start 取到连接之后、启动读写协程之前调用
	sendMu                  sync.RWMutex // Send 持读锁入队、Close 持写锁关闭 sendChan，避免向已关闭的通道发送
	// 统计信息
	stats Stats

	// 可通过options配置
	maxMsgSize        uint32           // 最大消息大小
	bufferSize        int              // 缓冲区大小
	sendChan          chan interface{} // 消息通道
	connectionTimeout time.Duration    // 连接超时时间
	idleTimeout       time.Duration    // 空闲超时时间
	maxRetries        int              // 最大重试次数
	baseDelay         time.Duration    // 初始重试等待时间
	maxDelay          time.Duration    // 最大重试等待时间

}

// New 创建新的客户端实例
func New(address string, protocolType protocol.ProtocolType, handler Handler, opts ...TCPClientOption) (*Client, func(), error) {
	if address == "" {
		return nil, nil, fmt.Errorf("address cannot be empty")
	}

	ctx, cancel := context.WithCancel(context.Background())
	client := &Client{
		address: address,

		// 上下文控制
		ctx:    ctx,
		cancel: cancel,
		wg:     &sync.WaitGroup{},

		// 设置处理器 和 协议
		handler: handler,

		// 状态控制
		closeOnce: sync.Once{},

		// 统计信息
		stats: NewStats(),

		// 可通过options配置
		maxMsgSize:        1 * 1024 * 1024,  // 默认1MB
		bufferSize:        1024,             // 缓冲区大小
		connectionTimeout: 5 * time.Second,  // 默认连接超时5秒
		idleTimeout:       30 * time.Minute, // 默认空闲超时30分钟
		maxRetries:        3,                // 默认最多重试3次
		baseDelay:         time.Second,      // 默认初始等待1秒
		maxDelay:          10 * time.Second, // 默认最大等待10秒
	}

	// 应用选项
	for _, opt := range opts {
		opt(client)
	}

	// 连接相关
	if client.dial == nil {
		dialer := &net.Dialer{Timeout: client.connectionTimeout} // 使用连接超时时间
		client.dial = dialer.DialContext
	}
	if client.handler == nil {
		client.handler = &DefaultHandler{}
	}
	p, err := protocol.NewProtocol(protocol.WithType(protocolType), protocol.WithMaxMessageSize(uint32(client.maxMsgSize)))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create protocol: %v", err)
	}
	client.protocol = p

	if client.sendChan == nil {
		// 消息通道
		client.sendChan = make(chan interface{}, client.bufferSize)
	}

	// 状态控制
	client.connected.Store(false)

	return client, func() {
		client.Close()
	}, nil
}

// Connect 连接到服务器
//
// 客户端只能连接一次、不支持重连：已连接时返回 ErrAlreadyConnected；Close 之后（包括拨号期间被
// Close）返回 ErrClientClosed，拨号得到的连接会被立即关闭，不会在 Close 之后"复活"。
// 并发调用 Connect 时只有一个成功，其余的关闭各自拨到的连接并返回 ErrAlreadyConnected。
// 如果在 OnConnect 执行期间连接被关闭，Connect 在 OnConnect 返回后调用 OnClose，然后返回
// ErrClientClosed。这里的"关闭"不只是本端 Close：在 OnConnect 里调用 Start 并一直运行到对端断开、
// 读写出错时，传输层同样已关闭，Connect 也返回 ErrClientClosed（此时客户端确实已关闭、不能再用）。
func (c *Client) Connect() error {
	if c.ctx.Err() != nil {
		c.stats.AddError()
		c.handler.OnError(c.ctx, nil, ErrClientClosed)
		return ErrClientClosed
	}
	if c.connected.Load() {
		c.handler.OnError(c.ctx, nil, ErrAlreadyConnected)
		c.stats.AddError()
		return ErrAlreadyConnected
	}

	// 建立TCP连接
	conn, err := c.dial(c.ctx, "tcp", c.address)
	if err != nil {
		if c.ctx.Err() != nil {
			// 拨号期间被 Close 取消
			c.stats.AddError()
			c.handler.OnError(c.ctx, nil, ErrClientClosed)
			return ErrClientClosed
		}
		c.handler.OnError(c.ctx, nil, fmt.Errorf("failed to connect to server: %v", err))
		c.stats.AddError()
		return fmt.Errorf("failed to connect to server: %v", err)
	}

	// 登记连接：先持 stateMu 判定是否已关闭（closeTransport 第一步就在 stateMu 下置 closed），
	// 再持 connMu 写入 conn 与 connected。closeTransport 在同一把 connMu 下置空二者，
	// 所以 Close 返回后 IsConnected 一定为 false，连接也不会在 Close 之后"复活"
	c.stateMu.Lock()
	if c.closed {
		c.stateMu.Unlock()
		_ = conn.Close()
		c.stats.AddError()
		c.handler.OnError(c.ctx, nil, ErrClientClosed)
		return ErrClientClosed
	}
	c.connMu.Lock()
	if c.conn != nil {
		c.connMu.Unlock()
		c.stateMu.Unlock()
		_ = conn.Close()
		c.stats.AddError()
		c.handler.OnError(c.ctx, nil, ErrAlreadyConnected)
		return ErrAlreadyConnected
	}
	c.conn = conn
	c.connected.Store(true)
	c.connMu.Unlock()
	c.connecting = true
	c.stateMu.Unlock()

	if c.testHookBeforeOnConnect != nil {
		c.testHookBeforeOnConnect()
	}

	// 通知连接建立。OnConnect 执行期间发生的关闭（并发的 Close、在 OnConnect 里调用 Close、
	// 并发运行的 Start 读写结束）不会立即回调 OnClose，而是记为 closeOwed，
	// 由这里在 OnConnect 返回后补调，保证 OnClose 一定在 OnConnect 完成之后
	c.handler.OnConnect(c.ctx, conn)

	c.stateMu.Lock()
	c.connecting = false
	owed := c.closeOwed
	c.closeOwed = false
	c.stateMu.Unlock()
	if owed {
		// 本端关闭发生在连接建立过程中：关闭流程本身不上报错误，只通过返回值告知调用方
		c.handler.OnClose(c.ctx, conn)
		return ErrClientClosed
	}
	return nil
}

// Start 启动客户端, 阻塞等待所有协程退出
//
// 回调顺序与等待语义：
//   - 读写协程退出（对端关闭、读写出错或 Close）后，Start 调用 OnClose，然后返回；
//     所以 Start 返回时 OnClose 已执行完，且 OnClose 之后不会再有 OnMessage。
//   - 例外：Start 与 Connect 并发、读写结束时 OnConnect 还没返回，Start 不等待它，
//     而是把 OnClose 交给 Connect 在 OnConnect 返回后调用（OnClose 始终晚于 OnConnect），Start 直接返回。
//   - 在 Start 之前（或从未 Start）就 Close 的客户端，由 Close 调用 OnClose，Start 不启动任何协程、立即返回。
//   - 重复调用 Start 立即返回。
//   - 之前 SimpleReceive 读到但未取走的数据会交给读协程继续解析。
//   - SimpleReceive 正在进行时调用 Start：回调 OnError(ErrReceiveConflict) 并立即返回，不启动任何协程，
//     之后（SimpleReceive 返回后）仍可正常调用 Start。
func (c *Client) Start() {
	c.stateMu.Lock()
	if c.started || c.closed {
		c.stateMu.Unlock()
		return
	}
	if c.receiving {
		// SimpleReceive 正在读同一个连接：两个读者会切碎字节流。拒绝本次 Start、不置 started，
		// SimpleReceive 返回后可以再次调用 Start
		c.stateMu.Unlock()
		c.stats.AddError()
		c.handler.OnError(c.ctx, c.getConn(), ErrReceiveConflict)
		return
	}
	c.started = true
	// 在同一临界区里取连接：closeTransport 先在 stateMu 下置 closed、之后才置空 c.conn，
	// 所以这里看到未关闭时取到的一定是 Connect 登记的连接（未 Connect 时为 nil）
	conn := c.getConn()
	leftover := c.recvBuf
	c.recvBuf = nil
	c.stateMu.Unlock()

	if c.testHookStartCaptured != nil {
		c.testHookStartCaptured()
	}

	// 启动收发协程
	c.wg.Add(2)
	go c.readLoop(conn, leftover)
	go c.writeLoop(conn)
	c.wg.Wait()
	// 两个协程都已退出（它们退出时会关闭传输层），不会再分发消息。
	// 回调用传输层实际关闭的那个连接：Start 可能先于 Connect 登记而取到 nil，随后登记的连接
	// 也已由 closeTransport 关闭，OnClose 必须收到它，用户在 OnConnect 里为它登记的资源才能释放
	conn = c.closedConn()
	c.stateMu.Lock()
	if c.connecting {
		// OnConnect 还在另一个协程里执行：交给 Connect 在它返回后调用 OnClose
		c.closeOwed = true
		c.stateMu.Unlock()
		return
	}
	c.stateMu.Unlock()
	c.handler.OnClose(c.ctx, conn)
}

// readLoop 读取循环, conn 为 Start 时取到的连接, leftover 为 SimpleReceive 留下的未解析数据
func (c *Client) readLoop(conn net.Conn, leftover []byte) {
	defer func() {
		c.closeTransport() // 任意一个协程退出，都要让另一个也退出
		c.wg.Done()
	}()

	if conn == nil {
		c.stats.AddError()
		c.handler.OnError(c.ctx, nil, fmt.Errorf("client not connected"))
		return
	}

	// 预分配读取缓冲区, 16kB
	readBuf := make([]byte, 16*1024)
	// 消息缓冲区, 最大消息大小
	msgBuf := make([]byte, 0, c.maxMsgSize)
	// 重试相关变量
	retryCount := 0
	retryDelay := c.baseDelay

	if len(leftover) > 0 {
		msgBuf = c.unpackAll(conn, append(msgBuf, leftover...))
	}

	for {
		// 本端关闭属于正常退出，不上报错误
		if c.ctx.Err() != nil {
			return
		}

		// 检查缓冲区大小，如果过大，说明可能有大量无效数据，直接清空
		if uint32(len(msgBuf)) > c.maxMsgSize {
			msgBuf = msgBuf[:0]
			c.stats.AddError()
			c.handler.OnError(c.ctx, conn, fmt.Errorf("buffer overflow"))
			continue
		}

		// 设置读取超时
		if err := conn.SetReadDeadline(time.Now().Add(c.idleTimeout)); err != nil {
			if c.ctx.Err() != nil {
				return
			}
			c.stats.AddError()
			c.handler.OnError(c.ctx, conn, fmt.Errorf("set read deadline failed: %v", err))
			return
		}

		// 从连接读取数据
		n, err := conn.Read(readBuf)
		if err != nil {
			if c.ctx.Err() != nil {
				return // 本端关闭导致的读错误
			}
			if err == io.EOF {
				// 对端关闭：只从这里上报一次
				c.handler.OnError(c.ctx, conn, fmt.Errorf("connection closed"))
				return
			}
			if tcperr.IsTemporaryError(err) {
				retryCount++
				c.handler.OnError(c.ctx, conn, fmt.Errorf("temporary read error (attempt %d/%d): %v", retryCount, c.maxRetries, err))
				if retryCount > c.maxRetries {
					// 重试次数超过最大值，关闭连接
					c.stats.AddError()
					return
				}

				retryDelay *= 2
				if retryDelay > c.maxDelay {
					retryDelay = c.maxDelay
				}
				c.stats.AddError()
				// 使用指数退避策略, Close 时立即醒来
				if !c.sleep(retryDelay) {
					return
				}
				continue
			}
			c.stats.AddError()
			c.handler.OnError(c.ctx, conn, fmt.Errorf("read error: %v", err))
			return
		}

		// 读取成功，重置重试相关变量
		retryCount = 0
		retryDelay = c.baseDelay

		// 追加到消息缓冲区, 并解析其中所有完整的消息
		msgBuf = append(msgBuf, readBuf[:n]...)
		msgBuf = c.unpackAll(conn, msgBuf)
	}
}

// unpackAll 循环解析 msgBuf 中的消息并分发，返回尚未处理的剩余数据。
// 一次 Read 可能带来多帧，只解析一帧的话剩下的帧要等对端再发数据才会被处理。
// 数据不足一帧，或协议实现没有让缓冲区前进（consumed <= 0）时停止，等待下一次 Read。
func (c *Client) unpackAll(conn net.Conn, msgBuf []byte) []byte {
	for attempt := 0; len(msgBuf) > 0; attempt++ {
		if c.ctx.Err() != nil {
			return msgBuf // 已关闭，不再分发
		}

		message, consumed, err := c.protocol.Unpack(msgBuf)
		switch err {
		case nil:
			// 成功解析一个完整的消息
			c.stats.AddMessageReceived()
			c.stats.AddBytesRead(int64(consumed))
			c.handler.OnMessage(c.ctx, conn, message)

		case tcperr.ErrShortRead:
			// 数据不足，等待更多数据。与只解析一帧时一样，只有本次 Read 后第一次解析就不足一帧才上报
			if attempt == 0 {
				c.stats.AddError()
				c.handler.OnError(c.ctx, conn, fmt.Errorf("short read"))
			}
			return msgBuf

		case tcperr.ErrMessageTooLarge, tcperr.ErrInvalidFormat, tcperr.ErrChecksum:
			// 消息过大 / 格式错误 / 校验错误：丢弃协议指定的长度
			c.stats.AddError()
			c.handler.OnError(c.ctx, conn, err)

		default:
			// 其他错误，丢弃整个包或指定长度
			c.stats.AddError()
			c.handler.OnError(c.ctx, conn, err)
			if consumed <= 0 {
				return msgBuf[:0]
			}
		}

		// 移除已处理的数据
		if consumed <= 0 {
			// 协议没有让缓冲区前进，继续解析只会原地打转
			return msgBuf
		}
		if consumed >= len(msgBuf) {
			msgBuf = msgBuf[:0]
		} else {
			msgBuf = msgBuf[consumed:]
		}
	}
	return msgBuf
}

// writeLoop 写入循环, conn 为 Start 时取到的连接
func (c *Client) writeLoop(conn net.Conn) {
	defer func() {
		c.closeTransport() // 任意一个协程退出，都要让另一个也退出
		c.wg.Done()
	}()

	if conn == nil {
		return // readLoop 已上报未连接
	}

	for {
		// 本端关闭（ctx 取消、sendChan 关闭）属于正常退出，不上报错误
		select {
		case <-c.ctx.Done():
			return
		case data, ok := <-c.sendChan:
			if !ok || c.ctx.Err() != nil {
				return
			}

			if !c.writeFull(conn, data.([]byte)) {
				return
			}
		}
	}
}

// writeFull 把一帧完整写出，失败时已上报错误，返回 false 表示写循环应退出。
//
// 只重发尚未写出的部分：Write 可能写出一部分后返回错误（例如写期限到达时返回已写出的字节数和
// 超时错误），从帧开头重发会让对端收到重复的前缀、帧边界错位。临时错误和"零进展"（n==0 且无错误）
// 都按重试处理，共用 maxRetries 次上限；每次尝试前刷新写期限；关闭后立即停止，不再刷新期限。
// Write 报告的字节数超出 [0, 剩余长度] 视为底层连接异常，上报错误并关闭。
// BytesWritten 按实际写出的字节累加，MessagesSent 在整帧写完时加 1。
func (c *Client) writeFull(conn net.Conn, data []byte) bool {
	written := 0
	retryCount := 0
	retryDelay := c.baseDelay
	for written < len(data) {
		if c.ctx.Err() != nil {
			return false // 本端已关闭
		}
		// 每次尝试前重新设置写入超时：否则超时导致的临时错误之后，重试会因为期限已过而立即失败
		if err := conn.SetWriteDeadline(time.Now().Add(c.idleTimeout)); err != nil {
			if c.ctx.Err() != nil {
				return false
			}
			c.stats.AddError()
			c.handler.OnError(c.ctx, conn, fmt.Errorf("set write deadline failed: %v", err))
			return false
		}
		remaining := len(data) - written
		n, err := conn.Write(data[written:])
		if n < 0 || n > remaining {
			c.stats.AddError()
			c.handler.OnError(c.ctx, conn, fmt.Errorf("invalid write count %d for %d bytes: %v", n, remaining, err))
			return false
		}
		written += n
		c.stats.AddBytesWritten(int64(n))

		var retryErr error
		switch {
		case err == nil && n > 0:
			continue // 有进展，继续写剩余部分
		case err == nil:
			retryErr = io.ErrShortWrite // 零进展
		case c.ctx.Err() != nil:
			return false // 本端已关闭
		case written == len(data) && tcperr.IsTemporaryError(err):
			continue // 数据已全部写出，临时错误不影响这一帧
		case !tcperr.IsTemporaryError(err):
			if written == len(data) {
				c.stats.AddMessageSent() // 这一帧已全部写出，照实统计；连接出错，随后关闭
			}
			c.handler.OnError(c.ctx, conn, fmt.Errorf("write error: %v", err))
			c.stats.AddError()
			return false
		default:
			retryErr = err
		}

		// 重试：临时错误或零进展
		retryCount++
		c.handler.OnError(c.ctx, conn, fmt.Errorf("temporary write error (attempt %d/%d): %v", retryCount, c.maxRetries, retryErr))
		if retryCount > c.maxRetries {
			c.stats.AddError()
			return false
		}
		// 使用指数退避策略, Close 时立即醒来
		retryDelay *= 2
		if retryDelay > c.maxDelay {
			retryDelay = c.maxDelay
		}
		c.stats.AddError()
		if !c.sleep(retryDelay) {
			return false
		}
	}
	c.stats.AddMessageSent()
	return true
}

// sleep 等待 d，客户端关闭时提前返回 false。
func (c *Client) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-c.ctx.Done():
		return false
	}
}

// Send 发送消息, 消息先进入发送队列, 由 writeLoop 异步写出
//
// Close 之后调用返回 ErrClientClosed，并同时回调 OnError（这是调用方的一次失败操作，
// 与关闭流程本身不上报错误不同）。Close 时队列里还没写出的消息会被丢弃。
func (c *Client) Send(msg interface{}) error {
	conn := c.getConn()

	if !c.connected.Load() {
		if c.ctx.Err() != nil {
			c.stats.AddError()
			c.handler.OnError(c.ctx, conn, ErrClientClosed)
			return ErrClientClosed
		}
		c.stats.AddError()
		c.handler.OnError(c.ctx, conn, fmt.Errorf("client not connected"))
		return fmt.Errorf("client not connected")
	}

	// 2. 打包消息
	data, err := c.protocol.Pack(msg)
	if err != nil {
		c.stats.AddError()
		c.handler.OnError(c.ctx, conn, fmt.Errorf("pack message failed: %v", err))
		return err
	}

	// 3. 判断消息大小是否超过最大消息大小
	if uint32(len(data)) > c.maxMsgSize {
		c.stats.AddError()
		c.handler.OnError(c.ctx, conn, fmt.Errorf("message too large"))
		return fmt.Errorf("message too large")
	}

	// 4. 入队。持读锁检查并入队：closeTransport 先 cancel、再持写锁关闭 sendChan，
	// 所以这里要么在关闭前完成入队，要么看到已关闭，不会向已关闭的通道发送。
	// 回调在释放读锁之后再调用，避免回调里调用 Close 时与写锁互等
	err = c.enqueue(data)
	if err != nil {
		c.stats.AddError()
		c.handler.OnError(c.ctx, conn, err)
	}
	return err
}

// enqueue 把数据放入发送队列。
func (c *Client) enqueue(data []byte) error {
	c.sendMu.RLock()
	defer c.sendMu.RUnlock()
	if c.ctx.Err() != nil {
		return ErrClientClosed
	}
	select {
	case c.sendChan <- data:
		return nil
	default:
		return fmt.Errorf("send buffer full")
	}
}

// Close 关闭客户端连接, 回收资源
//
// Close 先同步关闭传输层（取消 ctx、关闭发送队列和 socket），然后返回；它不在任何锁或 once 内部
// 调用用户回调，所以可以重复调用、并发调用，也可以在任意回调（包括 OnClose）里调用，都会立即返回。
// OnClose 只调用一次，且一定晚于已经开始的 OnConnect：Start 正在运行时由 Start 在读写协程退出后调用；
// OnConnect 正在执行时由 Connect 在它返回后调用；否则由第一次 Close 在传输层关闭之后调用。
// 前两种情况 Close 不等待回调。
// 客户端关闭后不能再 Connect。
func (c *Client) Close() error {
	if c.closeTransport() {
		// 没有 Start 在运行，由关闭者负责回调；此时没有读写协程，回调不会与 OnMessage 并发
		c.handler.OnClose(c.ctx, c.closedConn())
	}
	return nil
}

// closeTransport 关闭传输层，只执行一次；返回 true 表示由调用方负责调用 OnClose
// （只有第一次调用、Start 从未开始、且没有 OnConnect 正在执行时为 true）。不调用任何用户回调。
func (c *Client) closeTransport() (callOnClose bool) {
	c.closeOnce.Do(func() {
		c.stateMu.Lock()
		c.closed = true
		switch {
		case c.started:
			// Start 在读写协程退出后负责
		case c.connecting:
			// OnConnect 还没返回：由 Connect 在它返回后负责，保证 OnClose 晚于 OnConnect
			c.closeOwed = true
		default:
			callOnClose = true
		}
		c.stateMu.Unlock()

		// 发出关闭信号
		c.cancel()

		// 关闭发送通道（持写锁，与 Send 的入队互斥）
		c.sendMu.Lock()
		close(c.sendChan)
		c.sendMu.Unlock()

		// 清理连接资源。读写协程用的是 Start 时取到的连接，这里置空不会让它们解引用 nil
		// connected 与 conn 在同一把锁下清除：Connect 在这把锁下同时设置二者
		c.connMu.Lock()
		conn := c.conn
		c.conn = nil
		c.lastConn = conn
		c.connected.Store(false)
		c.connMu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
	})
	return callOnClose
}

// String 返回客户端状态的字符串表示
func (c *Client) String() string {
	return fmt.Sprintf(
		"TCPClient{address: %s, connected: %v, stats: {"+
			"msgs_sent: %d, msgs_recv: %d, "+
			"bytes_read: %d, bytes_written: %d, "+
			"errors: %d}}",
		c.address,
		c.connected.Load(),
		atomic.LoadInt64(&c.stats.MessagesSent),
		atomic.LoadInt64(&c.stats.MessagesReceived),
		atomic.LoadInt64(&c.stats.BytesRead),
		atomic.LoadInt64(&c.stats.BytesWritten),
		atomic.LoadInt64(&c.stats.Errors),
	)
}

// IsConnected 检查是否已连接
func (c *Client) IsConnected() bool {
	return c.connected.Load()
}

// RemoteAddr 获取远程地址
func (c *Client) RemoteAddr() net.Addr {
	if conn := c.getConn(); conn != nil {
		return conn.RemoteAddr()
	}
	return nil
}

// LocalAddr 获取本地地址
func (c *Client) LocalAddr() net.Addr {
	if conn := c.getConn(); conn != nil {
		return conn.LocalAddr()
	}
	return nil
}

// closedConn 返回 closeTransport 关闭的连接（关闭时未连接则为 nil）。
func (c *Client) closedConn() net.Conn {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	return c.lastConn
}

// getConn 返回当前连接, 未连接或已关闭时为 nil
func (c *Client) getConn() net.Conn {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	return c.conn
}

// 发送消息给服务器
func (c *Client) SimpleSend(msg interface{}) error {
	conn := c.getConn()
	if !c.connected.Load() || conn == nil {
		c.stats.AddError()
		c.handler.OnError(c.ctx, conn, fmt.Errorf("client not connected"))
		return fmt.Errorf("client not connected")
	}

	// 2. 打包消息
	data, err := c.protocol.Pack(msg)
	if err != nil {
		c.stats.AddError()
		c.handler.OnError(c.ctx, conn, fmt.Errorf("pack message failed: %v", err))
		return err
	}

	// 3. 判断消息大小是否超过最大消息大小
	if uint32(len(data)) > c.maxMsgSize {
		c.stats.AddError()
		c.handler.OnError(c.ctx, conn, fmt.Errorf("message too large"))
		return fmt.Errorf("message too large")
	}

	n, err := conn.Write(data)
	if err != nil {
		c.stats.AddError()
		c.handler.OnError(c.ctx, conn, fmt.Errorf("write message failed: %v", err))
		return err
	}

	c.stats.AddMessageSent()
	c.stats.AddBytesWritten(int64(n))
	return nil
}

// SimpleReceive 同步接收一条消息，阻塞直到收到一个完整的帧
//
// 一次读取到的多余数据（下一帧或半帧）会缓存起来，留给下一次 SimpleReceive，不会丢弃；
// 之后调用 Start 时，缓存的数据交给读协程继续解析。
// 不能与 Start 的读协程或另一个 SimpleReceive 同时使用，否则立即返回 ErrReceiveConflict。
// 阻塞期间被本端 Close 打断时返回 ErrClientClosed，不回调 OnError；对端关闭时返回 "connection closed"
// 并回调一次 OnError（与读协程一样不计入错误数）。
func (c *Client) SimpleReceive() (interface{}, error) {
	c.stateMu.Lock()
	if c.started || c.receiving {
		c.stateMu.Unlock()
		c.stats.AddError()
		c.handler.OnError(c.ctx, c.getConn(), ErrReceiveConflict)
		return nil, ErrReceiveConflict
	}
	c.receiving = true
	buf := c.recvBuf
	c.recvBuf = nil
	c.stateMu.Unlock()
	defer func() {
		c.stateMu.Lock()
		c.recvBuf = buf
		c.receiving = false
		c.stateMu.Unlock()
	}()

	conn := c.getConn()
	if conn == nil {
		c.stats.AddError()
		c.handler.OnError(c.ctx, nil, fmt.Errorf("client not connected"))
		return nil, fmt.Errorf("client not connected")
	}

	readBuf := make([]byte, 16*1024)
	for {
		if len(buf) > 0 {
			message, consumed, err := c.protocol.Unpack(buf)
			if err == nil {
				buf = advance(buf, consumed)
				c.stats.AddMessageReceived()
				c.stats.AddBytesRead(int64(consumed))
				return message, nil
			}
			if err != tcperr.ErrShortRead {
				// 非法数据：按协议给出的长度丢弃（没给出时丢弃全部），本次返回错误
				if consumed > 0 {
					buf = advance(buf, consumed)
				} else {
					buf = buf[:0]
				}
				c.stats.AddError()
				c.handler.OnError(c.ctx, conn, fmt.Errorf("unpack message failed: %v", err))
				return nil, fmt.Errorf("unpack message failed: %v", err)
			}
		}

		// 数据不足一帧：检查缓冲区上限后继续读取
		if uint32(len(buf)) > c.maxMsgSize {
			buf = buf[:0]
			c.stats.AddError()
			c.handler.OnError(c.ctx, conn, fmt.Errorf("buffer overflow"))
			return nil, fmt.Errorf("buffer overflow")
		}
		n, err := conn.Read(readBuf)
		if err != nil {
			if c.ctx.Err() != nil {
				// 本端 Close 打断了读取：与读协程一致，关闭流程本身不上报错误
				return nil, ErrClientClosed
			}
			if err == io.EOF {
				// 对端关闭：与读协程一致，上报一次、不计入错误数
				c.handler.OnError(c.ctx, conn, fmt.Errorf("connection closed"))
				return nil, fmt.Errorf("connection closed")
			}
			c.stats.AddError()
			c.handler.OnError(c.ctx, conn, fmt.Errorf("read error: %v", err))
			return nil, fmt.Errorf("read error: %v", err)
		}
		buf = append(buf, readBuf[:n]...)
	}
}

// advance 去掉 b 开头的 n 个字节，n 超过长度时返回空切片。
func advance(b []byte, n int) []byte {
	if n >= len(b) {
		return b[:0]
	}
	return b[n:]
}
