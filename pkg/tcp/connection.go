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
	once           sync.Once          // 确保清理只执行一次
	waitGroup      *sync.WaitGroup    // goroutine 同步等待组

	// 重试相关配置，用于解决从连接获取数据时，可能出现的临时错误
	maxRetryCount  int           // 最大重试次数, 默认3次
	baseRetryDelay time.Duration // 基础重试延迟, 默认1秒
	maxRetryDelay  time.Duration // 最大重试延迟, 默认10秒

	// 缓冲区设置
	bufferSize     int         // 缓冲区数量, 默认1024
	sendChan       chan []byte // 异步消息发送通道, 默认1024
	maxMessageSize uint32      // 连接允许单条传输的消息大小, 默认1MB

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
		once:           sync.Once{},
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
func (c *Connection) Start() {
	c.waitGroup.Add(3)
	go c.readLoop()
	go c.writeLoop()
	go c.checkIdleLoop()
	c.handler.OnConnect(c)
	c.waitGroup.Wait()
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
				// 如果达到发送速率限制，等待100ms
				c.metrics.AddError()
				c.handler.OnError(c, errors.ErrRateLimitExceeded)
				time.Sleep(c.silentTime)
				continue
			}

			// 1. 设置读取超时
			if err := c.conn.SetReadDeadline(time.Now().Add(c.idleTimeout)); err != nil {
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

			// 2. 检查连接状态
			if atomic.LoadInt32(&c.closed) == 1 {
				c.metrics.AddError()
				c.handler.OnError(c, errors.ErrConnectionClosed)
				return
			}

			// 3. 从连接读取数据
			// 读取到readBuf个长度的数据才会返回，否则阻塞
			n, err := c.conn.Read(readBuf)

			if err != nil {
				if err == io.EOF {
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
					time.Sleep(retryDelay)
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

			// 5. 尝试解析一个完整的消息
			start := time.Now()
			message, consumed, err := c.protocol.Unpack(msgBuf)

			// 6. 处理不同的错误情况
			switch err {
			case nil:
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

				// 移除已处理的数据
				msgBuf = msgBuf[consumed:]

			case errors.ErrShortRead:
				log.Printf("3. server connection %d unpack message short read, message: %+v", c.id, message)
				// 数据不足，保留所有数据等待更多数据
				c.metrics.AddError()
				c.handler.OnError(c, err)
				continue

			case errors.ErrMessageTooLarge:
				log.Printf("4. server connection %d unpack message too large, message: %+v", c.id, message)
				// 消息过大，丢弃指定长度
				c.metrics.AddError()
				c.handler.OnError(c, err)
				// 如果返回的consumed大于当前数据，说明需要丢弃所有数据
				if consumed > len(msgBuf) {
					msgBuf = msgBuf[:0]
				} else {
					msgBuf = msgBuf[consumed:]
				}

			case errors.ErrInvalidFormat:
				log.Printf("5. server connection %d unpack message invalid format, message: %+v", c.id, message)
				// 格式错误（比如魔数不在开头），丢弃指定长度的数据
				c.metrics.AddError()
				c.handler.OnError(c, err)
				// consumed表示魔数之前的数据长度，直接丢弃
				msgBuf = msgBuf[consumed:]

			case errors.ErrChecksum:
				log.Printf("6. server connection %d unpack message checksum error, message: %+v", c.id, message)
				// 校验错误，丢弃整个包
				c.metrics.AddError()
				c.handler.OnError(c, err)
				msgBuf = msgBuf[consumed:]

			default:
				log.Printf("7. server connection %d unpack message other error, message: %+v", c.id, message)
				// 其他错误（比如JSON解析错误），丢弃整个包
				c.metrics.AddError()
				c.handler.OnError(c, err)
				if consumed > 0 {
					msgBuf = msgBuf[consumed:]
				} else {
					// 无法恢复的错误，且没有指定丢弃长度，丢弃所有数据
					msgBuf = msgBuf[:0]
				}
			}
		}
	}
}

// writeLoop 处理发送消息。
func (c *Connection) writeLoop() {
	defer func() {
		c.Close()
		c.waitGroup.Done()
	}()

	for {
		// 判断是否达到发送速率限制
		if !c.rateLimiter.Allow() {
			c.metrics.AddError()
			c.handler.OnError(c, errors.ErrRateLimitExceeded)
			// 如果达到发送速率限制，等待100ms
			time.Sleep(100 * time.Millisecond)
			continue
		}

		select {
		case <-c.ctx.Done():
			c.metrics.AddError()
			c.handler.OnError(c, errors.ErrConnectionClosed)
			return
		case data, ok := <-c.sendChan:
			if !ok {
				c.metrics.AddError()
				c.handler.OnError(c, errors.WrapError(errors.ErrorTypeSystem, errors.ErrConnectionClosed, "sendChan closed"))
				return
			}

			// 检查连接状态，如果已关闭，直接返回
			if atomic.LoadInt32(&c.closed) == 1 {
				c.metrics.AddError()
				c.handler.OnError(c, errors.WrapError(errors.ErrorTypeSystem, errors.ErrConnectionClosed, "connection closed"))
				return
			}

			start := time.Now()
			err := c.conn.SetWriteDeadline(time.Now().Add(time.Second * 10))
			if err != nil {
				c.metrics.AddError()
				c.handler.OnError(c, errors.WrapError(errors.ErrorTypeSystem, err, "set write deadline failed"))
				return
			}

			// 写入重试逻辑
			retryCount := 0
			retryDelay := c.baseRetryDelay
			for {
				n, err := c.conn.Write(data)
				if err != nil {
					if errors.IsTemporaryError(err) {
						c.handler.OnError(c, errors.WrapError(errors.ErrorTypeSystem, err, "write error"))
						retryCount++
						// 重试次数超过最大值，关闭连接
						if retryCount > c.maxRetryCount {
							c.metrics.AddError()
							return
						}
						// 使用指数退避策略
						retryDelay *= 2
						if retryDelay > c.maxRetryDelay {
							retryDelay = c.maxRetryDelay
						}
						c.metrics.AddError()
						time.Sleep(retryDelay)
						continue
					} else {
						c.handler.OnError(c, errors.WrapError(errors.ErrorTypeSystem, err, "write error"))
						c.metrics.AddError()
						return
					}
				}
				// 写入成功
				c.updateActiveTime()
				c.metrics.AddMessageSent(int64(n))
				c.metrics.SetMessageLatency(time.Since(start))
				break
			}
		}
	}
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
			c.metrics.AddError()
			c.handler.OnError(c, errors.ErrConnectionClosed)
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
	// 使用 defer-recover 来处理 channel 关闭导致的 panic
	defer func() {
		if r := recover(); r != nil {
			// channel 已关闭，转换为错误返回
			c.handler.OnError(nil, errors.WrapError(errors.ErrorTypeSystem, errors.ErrConnectionClosed, fmt.Sprintf("%v", r)))
			c.metrics.AddError()
		}
	}()

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
	select {
	case c.sendChan <- data:
		return nil
	case <-c.ctx.Done(): // 使用context来判断连接是否关闭, 避免在发送的时候出现连接被关闭的情况
		c.metrics.AddError()
		c.handler.OnError(c, errors.ErrConnectionClosed)
		return errors.ErrConnectionClosed
	default: // 缓冲区已满
		c.metrics.AddError()
		c.handler.OnError(c, errors.ErrSendChannelFull)
		return errors.ErrSendChannelFull
	}
}

// Close 只调用一次，如果已经关闭，则直接返回
// 1. 将连接的标记设置为closed,
// 2. 通知所有的写协程退出,
// 3. 关闭原始连接
// 4. 调用OnClose回调
func (c *Connection) Close() {
	c.once.Do(func() {
		if !atomic.CompareAndSwapInt32(&c.closed, 0, 1) {
			log.Printf("server connection %d already closed", c.id)
			return
		}
		c.handler.OnClose(c)
		c.cancel()
		close(c.sendChan)
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

// RemoteAddr 获取远程地址
func (c *Connection) RemoteAddr() string {
	return c.conn.RemoteAddr().String()
}

// LocalAddr 获取本地地址
func (c *Connection) LocalAddr() net.Addr {
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
