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

import "sync/atomic"

// Stats 统计信息, 用于统计客户端的连接状态
type Stats struct {
	// 消息统计
	MessagesSent     int64
	MessagesReceived int64
	BytesRead        int64
	BytesWritten     int64
	Errors           int64
}

// NewStats 创建并初始化统计信息
func NewStats() Stats {
	return Stats{}
}

// AddMessageSent 增加发送消息计数
func (s *Stats) AddMessageSent() {
	atomic.AddInt64(&s.MessagesSent, 1)
}

// AddMessageReceived 增加接收消息计数
func (s *Stats) AddMessageReceived() {
	atomic.AddInt64(&s.MessagesReceived, 1)
}

// AddBytesRead 增加读取字节计数
func (s *Stats) AddBytesRead(n int64) {
	atomic.AddInt64(&s.BytesRead, n)
}

// AddBytesWritten 增加写入字节计数
func (s *Stats) AddBytesWritten(n int64) {
	atomic.AddInt64(&s.BytesWritten, n)
}

// AddError 增加错误计数
func (s *Stats) AddError() {
	atomic.AddInt64(&s.Errors, 1)
}
