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

// Package protocol 提供了 TCP 通信中消息编解码的核心接口和工具。
// 它定义了统一的协议接口，支持多种协议实现，包括基于长度的协议、JSON协议和二进制协议。
package protocol

import (
	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/errors"
	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol/binary"
	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol/json"
)

// ProtocolType 是协议类型的枚举。
// 用于标识不同的协议实现，方便协议的创建和管理。
type ProtocolType string

const (
	// JSON 表示 JSON 协议。
	// 使用 JSON 格式编解码消息，具有良好的可读性和跨语言特性。
	// 适用于调试环境或对性能要求不高的场景。
	JSON ProtocolType = "json"

	// BINARY 表示二进制协议。
	// 使用自定义的二进制格式编解码消息，具有最高的性能和最小的数据大小。
	// 适用于对性能和带宽要求极高的场景。
	BINARY ProtocolType = "binary"
)

// Protocol 定义了消息编解码的核心接口。
// 每种协议实现都必须提供消息的序列化（Pack）和反序列化（Unpack）能力。
// 不同的协议实现可以针对不同的场景优化，比如追求效率的二进制协议，或者追求可读性的JSON协议。
type Protocol interface {
	// Unpack 尝试从数据中解析一个完整的消息
	// 返回：解析出的消息，已处理的字节数，错误
	// - 如果数据不足，返回(nil, 0, errors.ErrShortRead)
	// - 如果消息格式错误，返回(nil, n, errors.ErrInvalidFormat)，n为需要跳过的字节数
	// - 如果消息过大，返回(nil, n, errors.ErrMessageTooLarge)，n为消息的完整长度
	// - 如果校验失败，返回(nil, n, errors.ErrChecksum)，n为消息的完整长度
	Unpack(data []byte) (message interface{}, n int, err error)

	// Pack 将消息打包成字节流
	// 返回：打包后的字节流，错误
	// - 如果消息格式错误，返回(nil, errors.ErrInvalidFormat)
	// - 如果消息过大，返回(nil, errors.ErrMessageTooLarge)
	Pack(message interface{}) ([]byte, error)
}

// Message 定义了基础消息的接口。
// 所有协议中传输的消息都应该实现这个接口，提供消息类型和序列号信息。
// 这些信息用于消息的路由、追踪和去重。
type Message interface {
	// GetMessageType 返回消息的类型标识。
	// 消息类型用于区分不同种类的消息，便于消息的分发和处理。
	// 返回值是一个 uint32 类型的标识符。
	GetMessageType() uint32

	// GetSequence 返回消息的序列号。
	// 序列号用于消息的排序、去重和追踪。
	// 返回值是一个 uint32 类型的序列号。
	GetSequence() uint32
}

type ProtocolOption func(*Options)

type Options struct {
	MaxMessageSize uint32       // 最大消息大小
	Type           ProtocolType // 协议类型
}

func WithMaxMessageSize(size uint32) ProtocolOption {
	return func(o *Options) {
		o.MaxMessageSize = size
	}
}

func WithType(pt ProtocolType) ProtocolOption {
	return func(o *Options) {
		o.Type = pt
	}
}

// 初始化协议
func NewProtocol(opts ...ProtocolOption) (Protocol, error) {
	// 默认选项
	options := &Options{
		MaxMessageSize: 10 * 1024 * 1024,
		Type:           JSON, // 默认使用 JSON 协议
	}

	// 应用自定义选项
	for _, opt := range opts {
		opt(options)
	}

	// 根据协议类型创建相应的协议实例
	switch options.Type {
	case JSON:
		return json.New(options.MaxMessageSize), nil
	case BINARY:
		return binary.New(options.MaxMessageSize), nil
	default:
		return nil, errors.ErrProtocolTypeNotSupported
	}
}
