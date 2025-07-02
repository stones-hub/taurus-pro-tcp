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
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	tcperr "github.com/stones-hub/taurus-pro-tcp/pkg/tcp/errors"
	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol"
)

// Client TCP客户端
type Client struct {
	// 基础配置
	address string // 连接的服务端地址
	// 上下文控制
	ctx    context.Context
	cancel context.CancelFunc
	wg     *sync.WaitGroup
	// 连接相关
	conn     net.Conn
	protocol protocol.Protocol
	handler  Handler
	// 状态控制
	connected atomic.Bool
	closeOnce sync.Once
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
func (c *Client) Connect() error {
	if c.connected.Load() {
		c.handler.OnError(c.ctx, nil, fmt.Errorf("client already connected"))
		c.stats.AddError()
		return fmt.Errorf("client already connected")
	}

	// 建立TCP连接
	dialer := &net.Dialer{Timeout: c.connectionTimeout} // 使用连接超时时间
	conn, err := dialer.DialContext(c.ctx, "tcp", c.address)
	if err != nil {
		c.handler.OnError(c.ctx, nil, fmt.Errorf("failed to connect to server: %v", err))
		c.stats.AddError()
		return fmt.Errorf("failed to connect to server: %v", err)
	}

	c.conn = conn
	c.connected.Store(true)
	// 通知连接建立
	c.handler.OnConnect(c.ctx, c.conn)
	return nil
}

// Start 启动客户端, 阻塞等待所有协程退出
func (c *Client) Start() {
	// 启动收发协程
	c.wg.Add(2)
	go c.readLoop()
	go c.writeLoop()
	c.wg.Wait()
}

// readLoop 读取循环
func (c *Client) readLoop() {
	defer func() {
		c.wg.Done()
		c.Close() // 直接调用 Close 进行清理
	}()

	// 预分配读取缓冲区, 16kB
	readBuf := make([]byte, 16*1024)
	// 消息缓冲区, 最大消息大小
	msgBuf := make([]byte, 0, c.maxMsgSize)
	// 重试相关变量
	retryCount := 0
	retryDelay := c.baseDelay

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			// 检查连接状态
			if !c.connected.Load() {
				c.handler.OnError(c.ctx, c.conn, fmt.Errorf("connection closed"))
				c.stats.AddError()
				return
			}

			// 检查缓冲区大小，如果过大，说明可能有大量无效数据，直接清空
			if uint32(len(msgBuf)) > c.maxMsgSize {
				msgBuf = msgBuf[:0]
				c.stats.AddError()
				c.handler.OnError(c.ctx, c.conn, fmt.Errorf("buffer overflow"))
				continue
			}

			// 设置读取超时
			if err := c.conn.SetReadDeadline(time.Now().Add(c.idleTimeout)); err != nil {
				c.stats.AddError()
				c.handler.OnError(c.ctx, c.conn, fmt.Errorf("set read deadline failed: %v", err))
				return
			}

			// 从连接读取数据
			n, err := c.conn.Read(readBuf)
			if err != nil {
				if err == io.EOF {
					c.handler.OnError(c.ctx, c.conn, fmt.Errorf("connection closed"))
					return
				}
				if tcperr.IsTemporaryError(err) {
					retryCount++
					c.handler.OnError(c.ctx, c.conn, fmt.Errorf("temporary read error (attempt %d/%d): %v", retryCount, c.maxRetries, err))
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
					// 使用指数退避策略
					time.Sleep(retryDelay)
					continue
				} else {
					c.stats.AddError()
					c.handler.OnError(c.ctx, c.conn, fmt.Errorf("read error: %v", err))
					return
				}
			}

			// 读取成功，重置重试相关变量
			retryCount = 0
			retryDelay = c.baseDelay

			// 追加到消息缓冲区
			msgBuf = append(msgBuf, readBuf[:n]...)
			// 清空已读取的数据
			// readBuf = readBuf[:0]

			// 尝试解析消息
			message, consumed, err := c.protocol.Unpack(msgBuf)
			switch err {
			case nil:
				// 成功解析一个完整的消息
				c.stats.AddMessageReceived()
				c.stats.AddBytesRead(int64(consumed))
				c.handler.OnMessage(c.ctx, c.conn, message)
				// 移除已处理的数据
				msgBuf = msgBuf[consumed:]

			case tcperr.ErrShortRead:
				// 数据不足，等待更多数据
				c.stats.AddError()
				c.handler.OnError(c.ctx, c.conn, fmt.Errorf("short read"))
				continue

			case tcperr.ErrMessageTooLarge:
				// 消息过大，丢弃指定长度
				c.stats.AddError()
				c.handler.OnError(c.ctx, c.conn, err)
				if consumed > len(msgBuf) {
					msgBuf = msgBuf[:0]
				} else {
					msgBuf = msgBuf[consumed:]
				}

			case tcperr.ErrInvalidFormat:
				// 格式错误，丢弃指定长度的数据
				c.stats.AddError()
				c.handler.OnError(c.ctx, c.conn, err)
				msgBuf = msgBuf[consumed:]

			case tcperr.ErrChecksum:
				// 校验错误，丢弃整个包
				c.stats.AddError()
				c.handler.OnError(c.ctx, c.conn, err)
				msgBuf = msgBuf[consumed:]

			default:
				// 其他错误，丢弃整个包或指定长度
				c.stats.AddError()
				c.handler.OnError(c.ctx, c.conn, err)
				if consumed > 0 {
					msgBuf = msgBuf[consumed:]
				} else {
					msgBuf = msgBuf[:0]
				}
			}
		}
	}
}

// writeLoop 写入循环
func (c *Client) writeLoop() {
	defer func() {
		c.wg.Done()
		c.Close() // 直接调用 Close 进行清理
	}()

	for {
		select {
		case <-c.ctx.Done():
			c.handler.OnError(c.ctx, c.conn, fmt.Errorf("client closed"))
			c.stats.AddError()
			return
		case data, ok := <-c.sendChan:
			if !ok {
				c.handler.OnError(c.ctx, c.conn, fmt.Errorf("sendChan closed"))
				c.stats.AddError()
				return
			}

			// 检查连接状态
			if !c.connected.Load() {
				c.handler.OnError(c.ctx, c.conn, fmt.Errorf("connection closed"))
				c.stats.AddError()
				return
			}

			// 设置写入超时
			if err := c.conn.SetWriteDeadline(time.Now().Add(c.idleTimeout)); err != nil {
				c.stats.AddError()
				c.handler.OnError(c.ctx, c.conn, fmt.Errorf("set write deadline failed: %v", err))
				return
			}

			// 重试相关变量
			retryCount := 0
			retryDelay := c.baseDelay

			// 写入重试逻辑
			for {
				n, err := c.conn.Write(data.([]byte))
				if err != nil {
					if tcperr.IsTemporaryError(err) {
						retryCount++
						c.handler.OnError(c.ctx, c.conn, fmt.Errorf("temporary write error (attempt %d/%d): %v", retryCount, c.maxRetries, err))
						if retryCount > c.maxRetries {
							c.stats.AddError()
							return
						}
						// 使用指数退避策略
						retryDelay *= 2
						if retryDelay > c.maxDelay {
							retryDelay = c.maxDelay
						}
						c.stats.AddError()
						time.Sleep(retryDelay)
						continue
					} else {
						c.handler.OnError(c.ctx, c.conn, fmt.Errorf("write error: %v", err))
						c.stats.AddError()
						return
					}
				}

				// 更新统计信息
				c.stats.AddMessageSent()
				c.stats.AddBytesWritten(int64(n))
				break
			}
		}
	}
}

// Send 发送消息
func (c *Client) Send(msg interface{}) error {
	// 使用 defer-recover 来处理 channel 关闭导致的 panic
	defer func() {
		if r := recover(); r != nil {
			c.handler.OnError(c.ctx, c.conn, fmt.Errorf("send message failed: %v", r))
			c.stats.AddError()
		}
	}()

	if !c.connected.Load() {
		c.stats.AddError()
		c.handler.OnError(c.ctx, c.conn, fmt.Errorf("client not connected"))
		return fmt.Errorf("client not connected")
	}

	// 2. 打包消息
	data, err := c.protocol.Pack(msg)
	if err != nil {
		c.stats.AddError()
		c.handler.OnError(c.ctx, c.conn, fmt.Errorf("pack message failed: %v", err))
		return err
	}

	// 3. 判断消息大小是否超过最大消息大小
	if uint32(len(data)) > c.maxMsgSize {
		c.stats.AddError()
		c.handler.OnError(c.ctx, c.conn, fmt.Errorf("message too large"))
		return fmt.Errorf("message too large")
	}

	select {
	case c.sendChan <- data:
		return nil
	case <-c.ctx.Done():
		c.stats.AddError()
		c.handler.OnError(c.ctx, c.conn, fmt.Errorf("client closed"))
		return fmt.Errorf("client closed")
	default:
		c.stats.AddError()
		c.handler.OnError(c.ctx, c.conn, fmt.Errorf("send buffer full"))
		return fmt.Errorf("send buffer full")
	}
}

// Close 关闭客户端连接, 回收资源
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.handler.OnClose(c.ctx, c.conn)
		// 发出关闭信号
		c.cancel()

		// 先将连接标记为关闭
		c.connected.Store(false)

		// 关闭发送通道
		close(c.sendChan)

		// 清理连接资源
		if c.conn != nil {
			_ = c.conn.Close()
			c.conn = nil
		}
	})
	return nil
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
		c.stats.MessagesSent,
		c.stats.MessagesReceived,
		c.stats.BytesRead,
		c.stats.BytesWritten,
		c.stats.Errors,
	)
}

// IsConnected 检查是否已连接
func (c *Client) IsConnected() bool {
	return c.connected.Load()
}

// RemoteAddr 获取远程地址
func (c *Client) RemoteAddr() net.Addr {
	if c.conn != nil {
		return c.conn.RemoteAddr()
	}
	return nil
}

// LocalAddr 获取本地地址
func (c *Client) LocalAddr() net.Addr {
	if c.conn != nil {
		return c.conn.LocalAddr()
	}
	return nil
}

// 发送消息给服务器
func (c *Client) SimpleSend(msg interface{}) error {
	// 使用 defer-recover 来处理 channel 关闭导致的 panic
	defer func() {
		if r := recover(); r != nil {
			c.handler.OnError(c.ctx, c.conn, fmt.Errorf("send message failed: %v", r))
			c.stats.AddError()
		}
	}()

	if !c.connected.Load() {
		c.stats.AddError()
		c.handler.OnError(c.ctx, c.conn, fmt.Errorf("client not connected"))
		return fmt.Errorf("client not connected")
	}

	// 2. 打包消息
	data, err := c.protocol.Pack(msg)
	if err != nil {
		c.stats.AddError()
		c.handler.OnError(c.ctx, c.conn, fmt.Errorf("pack message failed: %v", err))
		return err
	}

	// 3. 判断消息大小是否超过最大消息大小
	if uint32(len(data)) > c.maxMsgSize {
		c.stats.AddError()
		c.handler.OnError(c.ctx, c.conn, fmt.Errorf("message too large"))
		return fmt.Errorf("message too large")
	}

	n, err := c.conn.Write(data)
	if err != nil {
		c.stats.AddError()
		c.handler.OnError(c.ctx, c.conn, fmt.Errorf("write message failed: %v", err))
		return err
	}

	c.stats.AddMessageSent()
	c.stats.AddBytesWritten(int64(n))
	return nil
}

// 接收消息
func (c *Client) SimpleReceive() (interface{}, error) {

	readBuf := make([]byte, 16*1024)
	n, err := c.conn.Read(readBuf)
	if err != nil {
		c.handler.OnError(c.ctx, c.conn, fmt.Errorf("connection closed"))
		if err == io.EOF {
			return nil, fmt.Errorf("connection closed")
		} else {
			c.handler.OnError(c.ctx, c.conn, fmt.Errorf("read error: %v", err))
			return nil, fmt.Errorf("read error: %v", err)
		}
	}

	// 将读取的数据解包
	message, consumed, err := c.protocol.Unpack(readBuf[:n])
	if err != nil {
		c.handler.OnError(c.ctx, c.conn, fmt.Errorf("unpack message failed: %v", err))
		return nil, fmt.Errorf("unpack message failed: %v", err)
	}

	c.stats.AddMessageReceived()
	c.stats.AddBytesRead(int64(consumed))
	return message, nil
}
