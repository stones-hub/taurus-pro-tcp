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

import "time"

// TCPClientOption 定义客户端选项函数类型
type TCPClientOption func(*Client)

// WithMaxMsgSize 设置最大消息大小
func WithMaxMsgSize(size uint32) TCPClientOption {
	return func(c *Client) {
		c.maxMsgSize = size
	}
}

// WithBufferSize 设置缓冲区大小
func WithBufferSize(size int) TCPClientOption {
	return func(c *Client) {
		c.bufferSize = size
		c.sendChan = make(chan interface{}, size)
	}
}

// WithConnectionTimeout 设置连接超时时间
func WithConnectionTimeout(timeout time.Duration) TCPClientOption {
	return func(c *Client) {
		c.connectionTimeout = timeout
	}
}

// WithIdleTimeout 设置空闲超时时间
func WithIdleTimeout(timeout time.Duration) TCPClientOption {
	return func(c *Client) {
		c.idleTimeout = timeout
	}
}

// WithMaxRetries 设置最大重试次数
func WithMaxRetries(maxRetries int) TCPClientOption {
	return func(c *Client) {
		c.maxRetries = maxRetries
	}
}

// WithBaseRetryDelay 设置初始重试等待时间
func WithBaseRetryDelay(baseDelay time.Duration) TCPClientOption {
	return func(c *Client) {
		c.baseDelay = baseDelay
	}
}

// WithMaxRetryDelay 设置最大重试等待时间
func WithMaxRetryDelay(maxDelay time.Duration) TCPClientOption {
	return func(c *Client) {
		c.maxDelay = maxDelay
	}
}
