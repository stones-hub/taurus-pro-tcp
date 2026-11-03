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
	"log"
	"net"
)

// Handler 定义了客户端事件处理接口
//
// 回调顺序：OnConnect 在 Connect 登记连接后调用；OnMessage 由 Start 启动的读协程调用；
// OnClose 只调用一次，在传输层（ctx、发送队列、socket）已关闭之后、并且在已开始的 OnConnect
// 返回之后调用，所以在 OnConnect 里登记、在 OnClose 里释放的资源不会泄漏：
//   - Start 正在运行时，由 Start 在读写协程都退出后调用，之后不会再有 OnMessage，Start 随后返回；
//   - OnConnect 正在执行时（并发的 Close、在 OnConnect 里调用 Close、或并发运行的 Start 先结束），
//     由 Connect 在 OnConnect 返回后调用，Connect 随后返回 ErrClientClosed；
//   - 否则由第一次 Close 调用。
//
// 所有回调都不在锁或 once 内部调用，回调里可以调用 Close（立即返回）。
// 例外：Start 与 Connect 在不同协程里并发时，OnMessage 可能早于 OnConnect 返回。
// 关闭流程本身不上报错误（本端 Close 不调用 OnError，对端关闭时上报一次 "connection closed"）；
// Close 之后主动调用 Send 会返回 ErrClientClosed 并回调 OnError。Close 时发送队列里还没写出的消息会被丢弃。
//
// 回调 panic：库不 recover 回调里的 panic。读写协程里的 OnMessage/OnError/OnClose panic 会使进程崩溃；
// 在 Connect 的调用方 recover 了 OnConnect 的 panic 时，客户端会一直处于"OnConnect 执行中"的状态，
// 之后 OnClose 不会再被调用（传输层仍可正常 Close），在 OnConnect 里登记的资源需要调用方自行清理。
type Handler interface {
	// OnConnect 当连接建立时调用
	OnConnect(ctx context.Context, conn net.Conn)
	// OnMessage 当收到消息时调用
	OnMessage(ctx context.Context, conn net.Conn, msg interface{})
	// OnClose 当连接关闭时调用, 未连接成功就 Close 时 conn 为 nil
	OnClose(ctx context.Context, conn net.Conn)
	// OnError 当发生错误时调用, 连接建立之前的错误（如拨号失败）conn 为 nil
	OnError(ctx context.Context, conn net.Conn, err error)
}

// TODO: 需要实现一个默认的处理器实现
type DefaultHandler struct{}

func (h *DefaultHandler) OnConnect(ctx context.Context, conn net.Conn) {
	log.Printf("连接建立: %s", conn.RemoteAddr())
}
func (h *DefaultHandler) OnMessage(ctx context.Context, conn net.Conn, msg interface{}) {
	log.Printf("收到消息: %v", msg)
}
func (h *DefaultHandler) OnClose(ctx context.Context, conn net.Conn) {
	if conn == nil {
		// 未连接成功就 Close 时 conn 为 nil
		return
	}
	log.Printf("连接关闭: %s", conn.RemoteAddr())
}
func (h *DefaultHandler) OnError(ctx context.Context, conn net.Conn, err error) {
	if conn == nil {
		log.Printf("发生错误: %v", err)
		return
	}
	log.Printf("发生错误: %s, %v", conn.RemoteAddr(), err)
}
