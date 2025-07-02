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

import "log"

// ---------------------------------------------------------------------------------------------------------------------
// Handler 定义了连接事件处理的接口, 注意如果handler中实现了子协程的逻辑切记需要监听ctx.Done()，否则子协程不会退出，造成协程泄漏
// ---------------------------------------------------------------------------------------------------------------------
// 实现者需要处理各种连接生命周期事件。
type Handler interface {
	OnConnect(conn *Connection)                      // 当新连接建立时调用
	OnMessage(conn *Connection, message interface{}) // 当收到消息时调用
	OnClose(conn *Connection)                        // 当连接关闭时调用
	OnError(conn *Connection, err error)             // 当发生错误时调用
}

var handlers = make(map[string]Handler)

func RegisterHandler(name string, handler Handler) {
	if _, ok := handlers[name]; ok {
		log.Printf("Handler %s already registered", name)
		return
	}
	handlers[name] = handler
}

func GetHandler(name string) Handler {
	if handler, ok := handlers[name]; ok {
		return handler
	}
	return &defaultHandler{}
}

type defaultHandler struct{}

func (h *defaultHandler) OnConnect(conn *Connection) {
	conn.SetAttr("handler", "default")
	conn.SetAttr("ip", conn.RemoteAddr())
	log.Printf("连接建立: %v", conn.RemoteAddr())
}

func (h *defaultHandler) OnMessage(conn *Connection, message interface{}) {
	log.Printf("收到消息: %v", message)
}

func (h *defaultHandler) OnClose(conn *Connection) {
	log.Printf("连接关闭: %v", conn.RemoteAddr())
}

func (h *defaultHandler) OnError(conn *Connection, err error) {
	log.Printf("连接错误: %v, 错误: %v", conn.RemoteAddr(), err)
}
