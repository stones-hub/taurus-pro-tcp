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
	"sync/atomic"
	"time"
)

// Metrics 为服务器和连接实例提供全面的监控功能。
// 它使用原子操作跟踪各种性能指标和运行统计，以确保线程安全。
type Metrics struct {
	// 连接指标
	totalConnections   int64 // 启动以来的总连接数
	currentConnections int64 // 当前活动连接数
	connectionRefused  int64 // 因限制而被拒绝的连接数

	// 消息指标
	totalMessagesReceived int64 // 接收的消息总数
	totalMessagesSent     int64 // 发送的消息总数
	totalBytesReceived    int64 // 接收的字节总数
	totalBytesSent        int64 // 发送的字节总数

	// 错误指标
	totalErrors int64 // 遇到的错误总数

	// 性能指标
	lastMessageLatency int64 // 最后一条消息的处理时间（纳秒）

	// 系统指标
	startTime time.Time // 实例启动时间，用于计算运行时长
}

// NewMetrics 创建一个新的指标收集器实例。
// 它将所有计数器初始化为零并设置启动时间。
func NewMetrics() *Metrics {
	return &Metrics{
		startTime: time.Now(),
	}
}

// 连接相关的指标方法

// AddConnection 增加总连接数和当前连接数。
func (m *Metrics) AddConnection() {
	atomic.AddInt64(&m.totalConnections, 1)
	atomic.AddInt64(&m.currentConnections, 1)
}

// RemoveConnection 减少当前连接数。
func (m *Metrics) RemoveConnection() {
	atomic.AddInt64(&m.currentConnections, -1)
}

// AddConnectionRefused 增加被拒绝的连接计数。
func (m *Metrics) AddConnectionRefused() {
	atomic.AddInt64(&m.connectionRefused, 1)
}

// 消息相关的指标方法

// AddMessageReceived 更新接收消息和字节的计数器。
func (m *Metrics) AddMessageReceived(bytes int64) {
	atomic.AddInt64(&m.totalMessagesReceived, 1)
	atomic.AddInt64(&m.totalBytesReceived, bytes)
}

// AddMessageSent 更新发送消息和字节的计数器。
func (m *Metrics) AddMessageSent(bytes int64) {
	atomic.AddInt64(&m.totalMessagesSent, 1)
	atomic.AddInt64(&m.totalBytesSent, bytes)
}

// 错误跟踪

// AddError 增加错误总数。
func (m *Metrics) AddError() {
	atomic.AddInt64(&m.totalErrors, 1)
}

// 性能监控

// SetMessageLatency 更新最后一条消息的处理时间。
func (m *Metrics) SetMessageLatency(latency time.Duration) {
	atomic.StoreInt64(&m.lastMessageLatency, int64(latency))
}

// GetStats 返回所有当前指标的映射。
// 这提供了调用时所有监控值的快照。
func (m *Metrics) GetStats() map[string]interface{} {
	uptime := time.Since(m.startTime)
	return map[string]interface{}{
		"uptime":              uptime.String(),
		"total_connections":   atomic.LoadInt64(&m.totalConnections),
		"current_connections": atomic.LoadInt64(&m.currentConnections),
		"connection_refused":  atomic.LoadInt64(&m.connectionRefused),
		"messages_received":   atomic.LoadInt64(&m.totalMessagesReceived),
		"messages_sent":       atomic.LoadInt64(&m.totalMessagesSent),
		"bytes_received":      atomic.LoadInt64(&m.totalBytesReceived),
		"bytes_sent":          atomic.LoadInt64(&m.totalBytesSent),
		"total_errors":        atomic.LoadInt64(&m.totalErrors),
		"last_latency_ns":     atomic.LoadInt64(&m.lastMessageLatency),
	}
}
