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
	"log"
	"sync"
)

// ---------------------------------------------------------------------------------------------------------------------
// Handler 定义了连接事件处理的接口, 注意如果handler中实现了子协程的逻辑切记需要监听ctx.Done()，否则子协程不会退出，造成协程泄漏
// ---------------------------------------------------------------------------------------------------------------------
// 实现者需要处理各种连接生命周期事件。
//
// 注意：OnError 的 conn 可能为 nil。服务器级别的错误（Accept 失败、连接数达到上限等）
// 不属于任何连接，此时以 nil 调用 OnError，实现者必须先判空再使用 conn。
// OnConnect、OnMessage、OnClose 的 conn 不会为 nil。
//
// 每个连接的回调顺序：OnConnect 先于 OnClose；OnClose 只调用一次，在该连接的读写循环全部退出、
// 底层 socket 已关闭之后调用，所以 OnClose 之后不会再有该连接的 OnConnect 或 OnMessage
// （读循环先于 OnConnect 启动，所以 OnMessage 可能与 OnConnect 并发，甚至在 OnConnect 被调用之前到达）。
// 在处理协程开始前就被关闭的连接不会触发任何回调。
// 关闭流程本身不上报错误：本端关闭（Connection.Close / Server.Stop）不调用 OnError；
// 对端关闭时 OnError 收到一次 ErrConnectionClosed。与之不同，关闭之后（包括在 OnClose 里）
// 用户主动调用 Send 属于一次失败的操作：Send 返回 ErrConnectionClosed，并同时回调 OnError。
// 关闭时发送队列里还没写出的消息会被丢弃。
// 回调里可以安全调用 Connection.Close 和 Server.Stop，它们都不调用、也不等待用户回调。
//
// 回调 panic：库不 recover 回调里的 panic。OnConnect/OnMessage/OnClose/OnError 都在连接或服务器自己的
// 协程里执行，panic 会使进程崩溃；需要容错的业务请在回调内部自行 recover 并清理自己的资源。
type Handler interface {
	OnConnect(conn *Connection)                      // 当新连接建立时调用
	OnMessage(conn *Connection, message interface{}) // 当收到消息时调用
	OnClose(conn *Connection)                        // 当连接关闭时调用
	OnError(conn *Connection, err error)             // 当发生错误时调用, conn 可能为 nil
}

var (
	handlersMu sync.RWMutex
	handlers   = make(map[string]Handler)
)

// RegisterHandler 按名字注册处理器，同名重复注册时保留先注册的那个。
// GetHandler 只在调用那一刻查表，所以必须在 GetHandler 之前注册（通常放在包级 init 里）。
func RegisterHandler(name string, handler Handler) {
	handlersMu.Lock()
	defer handlersMu.Unlock()
	if _, ok := handlers[name]; ok {
		log.Printf("Handler %s already registered", name)
		return
	}
	handlers[name] = handler
}

// GetHandler 按名字获取处理器。找不到时不会返回 nil，而是回退到内置的默认处理器
// （只打日志、不回复消息）。
func GetHandler(name string) Handler {
	handlersMu.RLock()
	defer handlersMu.RUnlock()
	if handler, ok := handlers[name]; ok {
		return handler
	}
	return &defaultHandler{}
}

type defaultHandler struct{}

func (h *defaultHandler) OnConnect(conn *Connection) {
	if conn == nil {
		return
	}
	conn.SetAttr("handler", "default")
	conn.SetAttr("ip", conn.RemoteAddr())
	log.Printf("连接建立: %v", conn.RemoteAddr())
}

func (h *defaultHandler) OnMessage(conn *Connection, message interface{}) {
	log.Printf("收到消息: %v", message)
}

func (h *defaultHandler) OnClose(conn *Connection) {
	if conn == nil {
		return
	}
	log.Printf("连接关闭: %v", conn.RemoteAddr())
}

func (h *defaultHandler) OnError(conn *Connection, err error) {
	if conn == nil {
		// 服务器级别的错误，没有对应的连接
		log.Printf("服务器错误: %v", err)
		return
	}
	log.Printf("连接错误: %v, 错误: %v", conn.RemoteAddr(), err)
}
