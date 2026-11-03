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
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/errors"
	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol"
)

// Server 表示一个处理多个客户端连接的 TCP 服务器。
// 它管理连接生命周期、执行资源限制并提供监控功能。
type Server struct {
	mu        sync.Mutex    // 保护 state 和 listener，并保证 Accept 到登记连接之间与 Stop 互斥
	state     serverState   // 生命周期状态, 只会前进: new -> running -> stopped
	done      chan struct{} // 所有服务器协程（acceptLoop 与连接处理协程，含其中的用户回调）退出后关闭
	acceptErr error         // acceptLoop 因致命错误退出时记录, 由 Start 返回

	silentTime time.Duration      // 静默时间, 避免服务器空转
	addr       string             // 网络监听地址
	ctx        context.Context    // 生命周期管理的上下文
	cancel     context.CancelFunc // 取消上下文的函数
	baseDelay  time.Duration      // 初始重试延迟时间
	maxDelay   time.Duration      // 最大重试延迟时间
	maxRetries int                // 最大重试次数
	conns      sync.Map           // 线程安全的连接存储
	wg         *sync.WaitGroup    // 优雅关闭的等待组
	metrics    *Metrics           // 服务器指标收集器
	listener   net.Listener       // TCP 监听器, 由 mu 保护

	listen      func(network, address string) (net.Listener, error) // 创建监听器, 默认 net.Listen, 测试可替换
	afterAccept func()                                              // 仅测试使用: Accept 成功后、登记连接前调用

	// 默认配置, 可以被配置选项覆盖
	protocol protocol.Protocol // 消息处理的协议实现
	handler  Handler           // 业务逻辑处理器
	maxConns int32             // 最大并发连接数, 默认1000, 用于创建connChan
	connChan chan struct{}     // 连接限制信号量

	// 每个连接相关配置, 可以被配置选项覆盖, 对server其实没用，为了兼容链接的配置
	bufferSize     int           // 缓冲区数量, 默认1024
	maxMessageSize uint32        // 连接允许单条传输的消息大小, 默认1MB
	idleTimeout    time.Duration // 连接最大空闲超时时间
	rateLimiter    int           // 消息频率限制器, 每秒100条消息
}

// serverState 表示服务器的生命周期状态。
type serverState int

const (
	stateNew     serverState = iota // 已创建, 尚未监听
	stateRunning                    // 正在监听
	stateStopped                    // 已停止, 不能再次启动
)

// ServerOption 定义了配置服务器的函数类型。
// 这遵循函数式选项模式以实现灵活配置。
type ServerOption func(*Server)

// WithConnectionBufferSize 设置连接的缓冲区大小
func WithConnectionBufferSize(size int) ServerOption {
	return func(s *Server) {
		s.bufferSize = size
	}
}

// WithConnectionMaxMessageSize 设置连接允许单条传输的消息大小
func WithConnectionMaxMessageSize(size uint32) ServerOption {
	return func(s *Server) {
		s.maxMessageSize = size
	}
}

// WithConnectionIdleTimeout 设置连接最大空闲超时时间
func WithConnectionIdleTimeout(timeout time.Duration) ServerOption {
	return func(s *Server) {
		s.idleTimeout = timeout
	}
}

// WithConnectionRateLimiter 设置连接的消息频率限制器
func WithConnectionRateLimiter(messagesPerSecond int) ServerOption {
	return func(s *Server) {
		s.rateLimiter = messagesPerSecond
	}
}

// WithProtocol 设置服务器的协议实现。
// 协议定义了消息如何编码和解码。
func WithProtocol(protocol protocol.Protocol) ServerOption {
	return func(s *Server) {
		s.protocol = protocol
	}
}

// WithHandler 设置服务器的消息处理器。
// 处理器实现了消息处理的业务逻辑。
func WithHandler(handler Handler) ServerOption {
	return func(s *Server) {
		s.handler = handler
	}
}

// WithMaxConnections 设置最大并发连接数。
// 同时初始化连接信号量通道。
func WithMaxConnections(maxConns int32) ServerOption {
	return func(s *Server) {
		s.maxConns = maxConns
		s.connChan = make(chan struct{}, maxConns)
	}
}

// NewServer 创建一个新的 TCP 服务器实例。
// 使用默认值初始化服务器并应用提供的选项。
func NewServer(addr string, protocol protocol.Protocol, handler Handler, opts ...ServerOption) (*Server, func(), error) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		addr:       addr,
		ctx:        ctx,
		cancel:     cancel,
		done:       make(chan struct{}),
		listen:     net.Listen,
		silentTime: 1 * time.Second,   // 静默时间, 避免服务器空转
		baseDelay:  1 * time.Second,   // 初始重试延迟时间
		maxDelay:   10 * time.Second,  // 最大重试延迟时间
		maxRetries: 3,                 // 最大重试次数
		conns:      sync.Map{},        // 线程安全的连接存储
		wg:         &sync.WaitGroup{}, // 优雅关闭的等待组
		metrics:    NewMetrics(),      // 服务器层面的统计指标

		// 默认配置, 可以被配置选项覆盖
		protocol: protocol, // 消息处理的协议实现
		handler:  handler,  // 业务逻辑处理器
		maxConns: 1000,     // 默认最大连接数
		// 每个连接相关配置, 可以被配置选项覆盖
		bufferSize:     1024,             // 缓冲区数量, 默认1024
		maxMessageSize: 1 * 1024 * 1024,  // 连接允许单条传输的消息大小, 默认1MB
		idleTimeout:    30 * time.Minute, // 连接最大空闲超时时间
		rateLimiter:    100,              // 消息频率限制器, 每秒100条消息
	}

	// 应用所有配置选项
	for _, opt := range opts {
		opt(s)
	}

	// 如果未设置则初始化连接信号量
	if s.connChan == nil {
		s.connChan = make(chan struct{}, s.maxConns)
	}

	return s, s.Stop, nil
}

// Start 开始接受客户端连接。
// 确保服务器只启动一次并具备所需组件。
// Start 会阻塞，直到服务器停止且所有连接处理协程（含其中的用户回调）退出后才返回。
// 正常停止返回 nil；Accept 遇到致命错误导致服务器退出时，返回同时包装了 ErrSystemFatal 和
// 底层错误的 error（可用 errors.Is 判断）。
// 已经启动过返回 ErrServerAlreadyStarted；在 Stop 之后调用返回 ErrServerStopped，不会再监听。
func (s *Server) Start() error {
	// 验证必需组件
	if s.protocol == nil {
		return errors.ErrProtocolNotSet
	}
	if s.handler == nil {
		return errors.ErrHandlerNotSet
	}

	s.mu.Lock()
	switch s.state {
	case stateRunning:
		s.mu.Unlock()
		return errors.ErrServerAlreadyStarted
	case stateStopped:
		s.mu.Unlock()
		return errors.ErrServerStopped
	}

	// 创建 TCP 监听器。持锁监听，使并发的 Stop 要么在监听前生效（这里返回 ErrServerStopped），
	// 要么一定能看到并关闭这个 listener
	listener, err := s.listen("tcp", s.addr)
	if err != nil {
		s.mu.Unlock()
		log.Printf("server listen on %s failed: %v", s.addr, err)
		return errors.ErrServerListenerFailed
	}
	s.listener = listener
	s.state = stateRunning

	s.wg.Add(1)
	// 开协程后台处理连接
	go s.acceptLoop(listener)
	s.mu.Unlock()

	// 阻塞等待所有协程退出
	s.wg.Wait()
	close(s.done)
	log.Printf("server stopped on %s", s.addr)
	// acceptErr 在 acceptLoop 退出前写入, wg.Wait 保证这里能看到
	return s.acceptErr
}

// acceptLoop 在独立的 goroutine 中运行并处理传入连接。
// 它实现了连接限制、错误处理和重试机制。
func (s *Server) acceptLoop(listener net.Listener) {
	defer func() {
		// 协程退出，需要清理服务器资源
		s.Stop()
		s.wg.Done()
	}()

	retries := 0         // 当前重试次数
	delay := s.baseDelay // 当前重试延迟时间

	for {
		// 先检查服务器状态
		if s.ctx.Err() != nil {
			return
		}

		// 先检查是否有可用槽位和服务状态
		select {
		case s.connChan <- struct{}{}: // 获取到槽位
			// 获取到槽位后接受连接, 有可能阻塞，不过没事，因为你已经获取到槽位了
			conn, err := listener.Accept()
			if err != nil {
				<-s.connChan // 释放槽位
				// 如果服务器已关闭，直接返回
				if s.ctx.Err() != nil {
					return
				}

				// 判断错误是否为临时性的
				if errors.IsTemporaryError(err) {
					if retries < s.maxRetries {
						retries++
						s.metrics.AddError()
						s.handler.OnError(nil, errors.ErrSystemOverload)
						if !s.sleep(delay) {
							return
						}
						delay *= 2
						if delay > s.maxDelay {
							delay = s.maxDelay
						}
						continue
					}
					// 重试次数达到最大值，退出协程
					s.acceptErr = fmt.Errorf("%w: accept on %s failed after %d retries: %w", errors.ErrSystemFatal, s.addr, retries, err)
					s.metrics.AddError()
					s.handler.OnError(nil, errors.ErrSystemFatal)
					return
				}
				// 非临时性错误，退出协程
				s.acceptErr = fmt.Errorf("%w: accept on %s: %w", errors.ErrSystemFatal, s.addr, err)
				s.metrics.AddError()
				s.handler.OnError(nil, errors.ErrSystemFatal)
				return
			}

			// 重置重试相关计数
			retries = 0
			delay = s.baseDelay

			if s.afterAccept != nil {
				s.afterAccept()
			}

			// 创建并登记新连接。与 Stop 互斥：Stop 已开始时直接关闭这个连接，
			// 否则登记后 Stop 一定能在 conns 里看到它
			s.mu.Lock()
			if s.state == stateStopped {
				s.mu.Unlock()
				_ = conn.Close()
				<-s.connChan // 释放槽位
				return
			}
			c := NewConnection(conn, s.protocol, s.handler,
				WithSendChanSize(s.bufferSize),
				WithMaxMessageSize(s.maxMessageSize),
				WithIdleTimeout(s.idleTimeout),
				WithRateLimit(s.rateLimiter))
			s.conns.Store(c.ID(), c)
			s.metrics.AddConnection()
			s.wg.Add(1)
			s.mu.Unlock()

			// 启动协程处理单个连接
			go func() {
				// 当处理连接的协程退出，只需要清理当前连接的资源
				defer func() {
					s.conns.Delete(c.ID())
					s.metrics.RemoveConnection()
					<-s.connChan // 释放连接槽
					s.wg.Done()
				}()
				// Connection.Start()内部会处理连接的关闭
				c.Start()
			}()

		case <-s.ctx.Done(): // 服务器正在关闭
			return

		default: // 没有可用槽位，等待一会再试
			s.metrics.AddConnectionRefused()
			s.handler.OnError(nil, errors.ErrTooManyConnections)
			if !s.sleep(s.silentTime) { // 避免空转
				return
			}
		}
	}
}

// sleep 等待 d，服务器关闭时提前返回 false。
func (s *Server) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.ctx.Done():
		return false
	}
}

// Stop 关闭服务器的网络层：停止接入新连接，并关闭所有现有连接的底层 socket。
//
// Stop 是同步的：返回时监听器已关闭（新的连接会被拒绝），已登记连接的 socket 都已关闭。
// Stop 不调用、也不等待任何用户回调：每个连接的 OnClose 由该连接的处理协程在其读写循环退出后
// 异步调用，正在执行的 OnMessage 也会继续执行到返回。需要等待这些回调完成时使用 StopContext。
//
// Stop 可以重复调用、并发调用、在 Start 之前调用（之后的 Start 返回 ErrServerStopped），
// 也可以在任意 Handler 回调（包括 OnClose）里调用；每次调用都只做上述有界的关闭步骤。
func (s *Server) Stop() {
	// 1. 先将服务器标记为已关闭，之后 Accept 到的连接会被直接关闭、不会登记
	s.mu.Lock()
	prev := s.state
	s.state = stateStopped
	listener := s.listener
	s.mu.Unlock()

	// 2. 取消上下文，确保 acceptLoop 收到退出信号
	s.cancel()

	// 3. 关闭监听器，停止接受新连接。重复调用时再关一次无害，
	// 保证每个调用方返回时监听器都已关闭
	if listener != nil {
		_ = listener.Close()
	}

	if prev == stateNew {
		// 从未成功启动, 没有任何服务器协程
		close(s.done)
	}

	// 4. 关闭所有已登记连接的传输层。Connection.Close 不调用用户回调，不会阻塞；
	// 与 Stop 并发登记的连接要么已在 conns 中，要么会被 acceptLoop 看到 stopped 后直接关闭
	s.conns.Range(func(key, value interface{}) bool {
		if conn, ok := value.(*Connection); ok {
			conn.Close()
		}
		return true
	})

	if prev != stateStopped {
		log.Println("server stopped completely !")
	}
}

// StopContext 关闭服务器，并在 ctx 结束前等待所有处理协程退出。
//
// 它分两步：
//  1. 同步执行与 Stop 相同的网络层关闭（关闭监听器和所有连接的 socket）。这一步不调用用户回调、
//     不受 ctx 影响，即使 ctx 已经取消也会先完成，所以 StopContext 返回时新连接一定被拒绝、
//     现有连接的对端一定能看到关闭。
//  2. 等待 acceptLoop 和所有连接处理协程（包括其中正在执行的 OnMessage 与随后的 OnClose）退出。
//
// 全部完成返回 nil；ctx 先结束时返回包装了 ctx.Err() 的错误（可用 errors.Is 判断
// context.DeadlineExceeded / context.Canceled），此时网络层已关闭，但仍有用户回调在执行：
// Go 无法强制终止忽略退出信号的回调，它们会继续运行直到自行返回。
//
// StopContext 不额外启动协程，可以重复调用（例如超时后以更长的期限再调用一次），
// 也可以在回调里调用，但在回调里等待会一直等到 ctx 结束，因为当前回调本身还没有返回。
func (s *Server) StopContext(ctx context.Context) error {
	s.Stop()
	if waitClosed(ctx, s.done) {
		return nil
	}
	return s.stopTimeoutError(ctx)
}

// waitClosed 等待 ch 关闭，ctx 先结束返回 false。两者同时就绪时以 ch 为准。
func waitClosed(ctx context.Context, ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
	}
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	}
}

// stopTimeoutError 构造 StopContext 超时时返回的错误。
func (s *Server) stopTimeoutError(ctx context.Context) error {
	active := 0
	s.conns.Range(func(key, value interface{}) bool {
		active++
		return true
	})
	return fmt.Errorf("tcp server on %s: transport stopped but %d connection handler(s) still running: %w", s.addr, active, ctx.Err())
}

// GetConnection 根据 ID 获取连接
func (s *Server) GetConnection(id uint64) (*Connection, bool) {
	if value, ok := s.conns.Load(id); ok {
		return value.(*Connection), true
	}
	return nil, false
}

// Broadcast 向所有连接广播消息
func (s *Server) Broadcast(message interface{}) {
	s.conns.Range(func(key, value interface{}) bool {
		conn := value.(*Connection)
		_ = conn.Send(message)
		return true
	})
}

// ConnectionCount 获取当前已登记的连接数（处理协程尚未退出的连接）
//
// 不使用连接槽位通道的长度：acceptLoop 阻塞在 Accept 时也占着一个槽位，会多算 1。
func (s *Server) ConnectionCount() int32 {
	return int32(atomic.LoadInt64(&s.metrics.currentConnections))
}

// GetMetrics 返回当前服务器指标。
func (s *Server) GetMetrics() map[string]interface{} {
	return s.metrics.GetStats()
}
