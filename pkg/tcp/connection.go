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

package tcp

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/errors"
	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol"

	"golang.org/x/time/rate"
)

// ConnectionOption 定义了配置连接的函数类型
type ConnectionOption func(*Connection)

func WithSendChanSize(size int) ConnectionOption {
	return func(c *Connection) {
		c.bufferSize = size
		c.sendChan = make(chan []byte, size)
	}
}

// WithIdleTimeout 设置连接的空闲超时时间
func WithIdleTimeout(timeout time.Duration) ConnectionOption {
	return func(c *Connection) {
		c.idleTimeout = timeout
	}
}

// WithRateLimit 设置消息速率限制
func WithRateLimit(messagesPerSecond int) ConnectionOption {
	return func(c *Connection) {
		c.rateLimiter = rate.NewLimiter(rate.Limit(messagesPerSecond), messagesPerSecond)
	}
}

// WithMaxMessageSize 设置连接允许传输的最大消息大小
func WithMaxMessageSize(bytes uint32) ConnectionOption {
	return func(c *Connection) {
		c.maxMessageSize = bytes
	}
}

// Connection 表示单个 TCP 连接并管理其生命周期。
// 它处理消息读写、流量控制和资源管理。
type Connection struct {
	id             uint64             // 唯一连接标识符
	silentTime     time.Duration      // 静默时间, 避免服务器空转
	conn           net.Conn           // 底层 TCP 连接
	protocol       protocol.Protocol  // 消息编码解码协议
	handler        Handler            // 连接事件处理器
	ctx            context.Context    // 生命周期管理上下文
	cancel         context.CancelFunc // 取消上下文的函数
	metrics        *Metrics           // 连接统计指标
	lastActiveTime atomic.Value       // 连接最后活动时间戳
	closed         int32              // 连接状态的原子标志, 0: 连接正常, 1: 连接已关闭，
	attrs          sync.Map           // 线程安全的属性存储, 用于存储连接的属性
	once           sync.Once          // 确保传输层只关闭一次
	stateMu        sync.Mutex         // 保护 started 与 closed 的状态转换, 使 Start 和 Close 互斥判定
	started        bool               // Start 是否已经开始分发回调
	waitGroup      *sync.WaitGroup    // goroutine 同步等待组

	// 重试相关配置，用于解决从连接获取数据时，可能出现的临时错误
	maxRetryCount  int           // 最大重试次数, 默认3次
	baseRetryDelay time.Duration // 基础重试延迟, 默认1秒
	maxRetryDelay  time.Duration // 最大重试延迟, 默认10秒

	// 缓冲区设置
	bufferSize     int          // 缓冲区数量, 默认1024
	sendChan       chan []byte  // 异步消息发送通道, 默认1024
	sendMu         sync.RWMutex // Send 持读锁入队、Close 持写锁关闭 sendChan，避免向已关闭的通道发送
	maxMessageSize uint32       // 连接允许单条传输的消息大小, 默认1MB

	idleTimeout time.Duration // 连接最大空闲超时时间
	rateLimiter *rate.Limiter // 消息频率限制器
}

var globalConnectionID uint64 // 生成唯一连接 ID 的全局计数器

// NewConnection 创建一个新的连接实例。
// 它接受可选的配置选项，使用函数式选项模式进行配置。
func NewConnection(conn net.Conn, protocol protocol.Protocol, handler Handler, opts ...ConnectionOption) *Connection {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Connection{
		id:             atomic.AddUint64(&globalConnectionID, 1),
		silentTime:     time.Second * 1, // 默认1秒静默时间
		conn:           conn,
		protocol:       protocol,
		handler:        handler,
		ctx:            ctx,
		cancel:         cancel,
		metrics:        NewMetrics(),
		closed:         0,
		attrs:          sync.Map{},
		waitGroup:      &sync.WaitGroup{},
		maxRetryCount:  3,
		baseRetryDelay: 1 * time.Second,
		maxRetryDelay:  10 * time.Second,

		// 默认配置, 可以被配置选项覆盖
		bufferSize:     1024,
		maxMessageSize: 1 * 1024 * 1024,                       // 默认单条最大消息大小1MB
		idleTimeout:    time.Minute * 30,                      // 默认30分钟空闲超时
		rateLimiter:    rate.NewLimiter(rate.Limit(100), 100), // 默认每秒100条消息
	}

	// 应用所有配置选项
	for _, opt := range opts {
		opt(c)
	}

	if c.sendChan == nil {
		c.sendChan = make(chan []byte, c.bufferSize)
	}

	c.lastActiveTime.Store(time.Now())
	return c
}

// Start 启动连接的读写循环。
// 它启动独立的 goroutine 用于读取、写入和空闲检查。
// 必须阻塞，直到链接关闭才能返回, 否则会退出上游协程
//
// 回调顺序：OnConnect 先于 OnClose；三个循环协程全部退出之后才调用 OnClose，且只调用一次，
// 所以 OnClose 之后不会再有 OnConnect 或 OnMessage。在 Start 之前就已 Close 的连接，
// 或重复调用 Start，不会触发任何回调并立即返回。
func (c *Connection) Start() {
	c.stateMu.Lock()
	if c.started || atomic.LoadInt32(&c.closed) == 1 {
		c.stateMu.Unlock()
		return
	}
	c.started = true
	c.stateMu.Unlock()

	c.waitGroup.Add(3)
	go c.readLoop()
	go c.writeLoop()
	go c.checkIdleLoop()
	c.handler.OnConnect(c)
	c.waitGroup.Wait()
	// 循环都已退出、传输层已关闭，不会再分发任何消息
	c.handler.OnClose(c)
}

// readLoop 持续读取和处理传入消息。
func (c *Connection) readLoop() {
	// 任意一个协程退出，都要将所有的协程退出
	defer func() {
		c.Close()
		c.waitGroup.Done()
	}()

	// 预分配读取缓冲区, 16kB
	readBuf := make([]byte, 16*1024)
	// 消息缓冲区, 最大消息大小
	msgBuf := make([]byte, 0, c.maxMessageSize)
	// 重试相关变量, 用于处理临时错误
	retryCount := 0
	retryDelay := c.baseRetryDelay

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			// 判断是否达到发送速率限制
			if !c.rateLimiter.Allow() {
				// 如果达到发送速率限制，等待一个静默周期；关闭时由循环顶部的 ctx 检查退出
				c.metrics.AddError()
				c.handler.OnError(c, errors.ErrRateLimitExceeded)
				c.sleep(c.silentTime)
				continue
			}

			// 1. 设置读取超时
			if err := c.conn.SetReadDeadline(time.Now().Add(c.idleTimeout)); err != nil {
				if c.ctx.Err() != nil {
					return // 本端已关闭
				}
				c.metrics.AddError()
				c.handler.OnError(c, errors.WrapError(errors.ErrorTypeSystem, err, "set read deadline failed"))
				return
			}

			// 检查缓冲区大小，如果过大，说明可能有大量无效数据，直接清空
			if uint32(len(msgBuf)) > c.maxMessageSize {
				log.Printf("1. server connection %d buffer overflow, buffer size: %d, max message size: %d", c.id, len(msgBuf), c.maxMessageSize)
				// 缓冲区过大，说明可能有大量无效数据，直接清空
				msgBuf = msgBuf[:0]
				c.metrics.AddError()
				c.handler.OnError(c, errors.ErrBufferOverflow)
				continue
			}

			// 2. 检查连接状态, 本端关闭属于正常退出, 不上报错误
			if atomic.LoadInt32(&c.closed) == 1 {
				return
			}

			// 3. 从连接读取数据
			// 读取到readBuf个长度的数据才会返回，否则阻塞
			n, err := c.conn.Read(readBuf)

			if err != nil {
				if c.ctx.Err() != nil {
					// 本端关闭导致的读错误（use of closed network connection），正常退出
					return
				}
				if err == io.EOF {
					// 对端关闭：每个连接只会从这里上报一次 ErrConnectionClosed
					c.handler.OnError(c, errors.ErrConnectionClosed)
					return
				}
				if errors.IsTemporaryError(err) {
					retryCount++
					c.handler.OnError(c, errors.WrapError(errors.ErrorTypeSystem, err, fmt.Sprintf("temporary read error (attempt %d/%d)", retryCount, c.maxRetryCount)))
					if retryCount > c.maxRetryCount {
						// 重试次数超过最大值，关闭连接
						c.metrics.AddError()
						return
					}
					// 使用指数退避策略计算下一次重试延迟
					retryDelay *= 2
					if retryDelay > c.maxRetryDelay {
						retryDelay = c.maxRetryDelay
					}
					c.metrics.AddError()
					c.sleep(retryDelay)
					continue
				} else {
					c.metrics.AddError()
					// 非临时错误，直接关闭连接
					c.handler.OnError(c, errors.WrapError(errors.ErrorTypeSystem, err, "read error"))
					return
				}
			}

			// 读取成功，重置重试相关变量
			retryCount = 0
			retryDelay = c.baseRetryDelay

			// 4. 追加到消息缓冲区
			msgBuf = append(msgBuf, readBuf[:n]...)
			// 重点： 这里不能使用readBuf = readBuf[:0]清空readBuf，清空会导致readBuf的切片长度为0，那c.conn.Read(readBuf)读取到0个字节也会立即返回
			// 因为c.conn.Read(readBuf)的含义是读取到readBuf个长度的数据才会返回，否则阻塞
			// readBuf = readBuf[:0]

			// 5. 解析缓冲区里所有完整的消息。一次 Read 可能带来多帧，只解析一帧的话，
			// 剩下的帧要等下一次 Read 才会被处理，而对端可能正在等回复、不会再发数据
			msgBuf = c.unpackAll(msgBuf)
		}
	}
}

// unpackAll 循环解析 msgBuf 中的消息并分发，返回尚未处理的剩余数据。
// 数据不足一帧，或协议实现没有让缓冲区前进（consumed <= 0）时停止，等待下一次 Read。
func (c *Connection) unpackAll(msgBuf []byte) []byte {
	messages := 0 // 本次 Read 后已分发的消息数
	for attempt := 0; len(msgBuf) > 0; attempt++ {
		if c.ctx.Err() != nil {
			// 连接已关闭，不再分发
			return msgBuf
		}

		start := time.Now()
		message, consumed, err := c.protocol.Unpack(msgBuf)

		// 处理不同的错误情况
		switch err {
		case nil:
			// 读循环在每次 Read 前取一个速率令牌；同一次 Read 中多出来的每条消息再各取一个，
			// 保持"每秒消息数上限"的语义
			if messages > 0 && !c.waitRateLimit(c.silentTime) {
				return msgBuf
			}
			messages++
			log.Printf("2. server connection %d unpack message success, message: %+v", c.id, message)
			// 成功解析一个完整的消息
			// 更新接收的消息数量
			c.metrics.AddMessageReceived(int64(consumed))
			// 设置消息最后处理时间
			c.metrics.SetMessageLatency(time.Since(start))
			// 更新连接最后活动时间
			c.updateActiveTime()

			// 处理消息
			c.handler.OnMessage(c, message)

		case errors.ErrShortRead:
			// 数据不足，保留所有数据等待更多数据。
			// 与只解析一帧时一样，只有本次 Read 后第一次解析就不足一帧才上报
			if attempt == 0 {
				log.Printf("3. server connection %d unpack message short read, message: %+v", c.id, message)
				c.metrics.AddError()
				c.handler.OnError(c, err)
			}
			return msgBuf

		case errors.ErrMessageTooLarge:
			log.Printf("4. server connection %d unpack message too large, message: %+v", c.id, message)
			// 消息过大，丢弃指定长度（consumed 大于当前数据时丢弃所有数据）
			c.metrics.AddError()
			c.handler.OnError(c, err)

		case errors.ErrInvalidFormat:
			log.Printf("5. server connection %d unpack message invalid format, message: %+v", c.id, message)
			// 格式错误（比如魔数不在开头），丢弃指定长度的数据
			c.metrics.AddError()
			c.handler.OnError(c, err)

		case errors.ErrChecksum:
			log.Printf("6. server connection %d unpack message checksum error, message: %+v", c.id, message)
			// 校验错误，丢弃整个包
			c.metrics.AddError()
			c.handler.OnError(c, err)

		default:
			log.Printf("7. server connection %d unpack message other error, message: %+v", c.id, message)
			// 其他错误（比如JSON解析错误），丢弃整个包
			c.metrics.AddError()
			c.handler.OnError(c, err)
			if consumed <= 0 {
				// 无法恢复的错误，且没有指定丢弃长度，丢弃所有数据
				return msgBuf[:0]
			}
		}

		// 移除已处理的数据
		if consumed <= 0 {
			// 协议没有让缓冲区前进，继续解析只会原地打转，等待下一次 Read
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

// waitRateLimit 等待一个速率令牌，令牌不足时上报 ErrRateLimitExceeded 并每隔 retry 再试；
// 连接关闭时返回 false。
func (c *Connection) waitRateLimit(retry time.Duration) bool {
	for !c.rateLimiter.Allow() {
		if c.ctx.Err() != nil {
			return false // 本端已关闭，不再上报限速错误
		}
		c.metrics.AddError()
		c.handler.OnError(c, errors.ErrRateLimitExceeded)
		if !c.sleep(retry) {
			return false
		}
	}
	return true
}

// sleep 等待 d，连接关闭时提前返回 false。
func (c *Connection) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-c.ctx.Done():
		return false
	}
}

// writeLoop 处理发送消息。
//
// 先等到真正出队的消息，再为它取一个速率令牌（与读方向共用同一个限速器，每条发出的消息消耗一个）：
// 没有消息要发时不消耗令牌、也不上报限速错误。等待令牌、重试退避都能被关闭立即打断。
func (c *Connection) writeLoop() {
	defer func() {
		c.Close()
		c.waitGroup.Done()
	}()

	for {
		// 本端关闭（ctx 取消、sendChan 关闭）属于正常退出, 不上报错误；
		// 对端关闭由 readLoop 统一上报一次
		select {
		case <-c.ctx.Done():
			return
		case data, ok := <-c.sendChan:
			if !ok {
				return
			}

			// 检查连接状态，如果已关闭，直接返回
			if atomic.LoadInt32(&c.closed) == 1 {
				return
			}

			// 判断是否达到发送速率限制，达到时每 100ms 重试一次；连接关闭时立即退出
			if !c.waitRateLimit(100 * time.Millisecond) {
				return
			}

			if !c.writeFull(data) {
				return
			}
		}
	}
}

// writeFull 把一帧完整写出，失败时已上报错误，返回 false 表示写循环应退出。
//
// 只重发尚未写出的部分：Write 可能写出一部分后返回错误（例如写期限到达时返回已写出的字节数和
// 超时错误），从帧开头重发会让对端收到重复的前缀、帧边界错位。临时错误和"零进展"（n==0 且无错误）
// 都按重试处理，共用 maxRetryCount 次上限；每次尝试前刷新写期限；关闭后立即停止，不再刷新期限。
// Write 报告的字节数超出 [0, 剩余长度] 视为底层连接异常，上报错误并关闭连接。
func (c *Connection) writeFull(data []byte) bool {
	start := time.Now()
	written := 0
	retryCount := 0
	retryDelay := c.baseRetryDelay
	for written < len(data) {
		if c.ctx.Err() != nil {
			return false // 本端已关闭
		}
		// 每次尝试前重新设置写入超时：否则超时导致的临时错误之后，重试会因为期限已过而立即失败
		if err := c.conn.SetWriteDeadline(time.Now().Add(time.Second * 10)); err != nil {
			if c.ctx.Err() != nil {
				return false
			}
			c.metrics.AddError()
			c.handler.OnError(c, errors.WrapError(errors.ErrorTypeSystem, err, "set write deadline failed"))
			return false
		}
		remaining := len(data) - written
		n, err := c.conn.Write(data[written:])
		if n < 0 || n > remaining {
			c.metrics.AddError()
			c.handler.OnError(c, errors.WrapError(errors.ErrorTypeSystem, err, fmt.Sprintf("invalid write count %d for %d bytes", n, remaining)))
			return false
		}
		written += n

		var retryErr error
		switch {
		case err == nil && n > 0:
			continue // 有进展，继续写剩余部分
		case err == nil:
			retryErr = errors.WrapError(errors.ErrorTypeSystem, io.ErrShortWrite, "write made no progress")
		case c.ctx.Err() != nil:
			return false // 本端已关闭
		case written == len(data) && errors.IsTemporaryError(err):
			continue // 数据已全部写出，临时错误不影响这一帧
		case !errors.IsTemporaryError(err):
			if written == len(data) {
				// 这一帧已全部写出，照实统计；连接出错，随后关闭
				c.metrics.AddMessageSent(int64(written))
			}
			c.handler.OnError(c, errors.WrapError(errors.ErrorTypeSystem, err, "write error"))
			c.metrics.AddError()
			return false
		default:
			retryErr = errors.WrapError(errors.ErrorTypeSystem, err, "write error")
		}

		// 重试：临时错误或零进展
		c.handler.OnError(c, retryErr)
		retryCount++
		// 重试次数超过最大值，关闭连接
		if retryCount > c.maxRetryCount {
			c.metrics.AddError()
			return false
		}
		// 使用指数退避策略
		retryDelay *= 2
		if retryDelay > c.maxRetryDelay {
			retryDelay = c.maxRetryDelay
		}
		c.metrics.AddError()
		if !c.sleep(retryDelay) {
			return false
		}
	}
	// 写入成功，统计实际写出的字节数
	c.updateActiveTime()
	c.metrics.AddMessageSent(int64(written))
	c.metrics.SetMessageLatency(time.Since(start))
	return true
}

// checkIdleLoop 监控连接活动并关闭空闲连接。
func (c *Connection) checkIdleLoop() {
	defer func() {
		c.Close()
		c.waitGroup.Done()
	}()

	ticker := time.NewTicker(time.Second * 30)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			lastActive := c.lastActiveTime.Load().(time.Time)
			if time.Since(lastActive) > c.idleTimeout {
				c.handler.OnError(c, errors.ErrConnectionIdle)
				return
			}
		}
	}
}

// Send 将消息写入发送队列，等待发送。
func (c *Connection) Send(message interface{}) error {
	// 1. 检查连接是否关闭
	if atomic.LoadInt32(&c.closed) == 1 {
		c.metrics.AddError()
		c.handler.OnError(c, errors.ErrConnectionClosed)
		return errors.ErrConnectionClosed
	}

	// 2. 打包消息
	data, err := c.protocol.Pack(message)
	if err != nil {
		c.metrics.AddError()
		c.handler.OnError(c, errors.WrapError(errors.ErrorTypeSystem, err, "pack message failed"))
		return err
	}

	// 3. 检查单条消息大小
	if uint32(len(data)) > c.maxMessageSize {
		c.metrics.AddError()
		c.handler.OnError(c, errors.ErrMessageTooLarge)
		return errors.ErrMessageTooLarge
	}

	// 4. 发送消息
	if err := c.enqueue(data); err != nil {
		c.metrics.AddError()
		c.handler.OnError(c, err)
		return err
	}
	return nil
}

// enqueue 把数据放入发送队列。
func (c *Connection) enqueue(data []byte) error {
	// 持读锁检查关闭标志并入队：Close 先置关闭标志、再持写锁关闭 sendChan，
	// 所以这里要么在关闭前完成入队，要么看到已关闭，不会向已关闭的通道发送
	c.sendMu.RLock()
	defer c.sendMu.RUnlock()
	if atomic.LoadInt32(&c.closed) == 1 {
		return errors.ErrConnectionClosed
	}

	select {
	case c.sendChan <- data:
		return nil
	default: // 缓冲区已满
		return errors.ErrSendChannelFull
	}
}

// Close 关闭连接的传输层：置关闭标志、通知所有循环协程退出、关闭发送队列和底层 socket。
//
// Close 是同步的、可重复调用、可并发调用（并发的调用方会等到传输层关闭完成后才返回），
// 不调用、也不等待任何用户回调，所以可以在任意回调里安全调用。
// OnClose 不在这里调用：由 Start 在所有循环协程退出后调用（见 Start）。
func (c *Connection) Close() {
	c.once.Do(func() {
		c.stateMu.Lock()
		atomic.StoreInt32(&c.closed, 1)
		c.stateMu.Unlock()

		c.cancel()
		c.sendMu.Lock()
		close(c.sendChan)
		c.sendMu.Unlock()
		if err := c.conn.Close(); err != nil {
			log.Printf("server connection %d close failed: %v", c.id, err)
		} else {
			log.Printf("server connection %d closed", c.id)
		}
	})
}

// 工具方法
func (c *Connection) ID() uint64 {
	return c.id
}

// SetAttr 设连接属性
func (c *Connection) SetAttr(key, value interface{}) {
	c.attrs.Store(key, value)
}

// GetAttr 获取连接属性
func (c *Connection) GetAttr(key interface{}) (interface{}, bool) {
	return c.attrs.Load(key)
}

// RemoteAddr 获取远程地址, c 为 nil 时返回空字符串
// （Handler.OnError 的 conn 可能为 nil，这样在 OnError 里直接打印 conn.RemoteAddr() 也不会 panic）
func (c *Connection) RemoteAddr() string {
	if c == nil || c.conn == nil {
		return ""
	}
	addr := c.conn.RemoteAddr()
	if addr == nil {
		return ""
	}
	return addr.String()
}

// LocalAddr 获取本地地址, c 为 nil 时返回 nil
func (c *Connection) LocalAddr() net.Addr {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.LocalAddr()
}

// GetMetrics 获取连接的统计指标
func (c *Connection) GetMetrics() map[string]interface{} {
	return c.metrics.GetStats()
}

// updateActiveTime 更新连接最后活动时间
func (c *Connection) updateActiveTime() {
	c.lastActiveTime.Store(time.Now())
}
