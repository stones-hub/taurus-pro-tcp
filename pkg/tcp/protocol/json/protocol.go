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

// Package json 实现了基于 JSON 的消息协议。
// 该协议使用 JSON 格式编码消息内容，提供了良好的可读性和跨语言兼容性。
// 适用于调试环境或对性能要求不高的场景。
//
// 协议格式：
// +----------------+----------------+--------------------------------+
// |   Magic(2B)    |   Length(4B)   |          JSON Data            |
// +----------------+----------------+--------------------------------+
//
// 字段说明：
// - Magic: 2字节魔数，固定为 0xFEFF，用于快速定位包边界
// - Length: 4字节，JSON数据长度，大端序
// - JSON Data: 变长，JSON编码的消息内容
//   {
//     "type": uint32,     // 消息类型
//     "sequence": uint32,  // 序列号
//     "data": object,     // 消息数据
//     "timestamp": int64  // 时间戳，纳秒
//   }
//
// 注意：
// 1. 所有整数字段采用大端序编码
// 2. JSON数据必须是UTF-8编码
// 3. timestamp字段自动填充，用于消息追踪
// 4. 适合开发调试，不建议用于生产环境的性能敏感场景

package json

import (
	"encoding/binary"
	"encoding/json"
	"time"

	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/errors"
)

const (
	// MagicNumber 魔数，用于快速定位包边界
	MagicNumber = 0xFEFF
	// HeaderSize = 2(magic) + 4(length)
	HeaderSize = 6
)

// Message JSON协议的消息结构
type Message struct {
	Type      uint32                 `json:"type"`
	Sequence  uint32                 `json:"sequence"`
	Data      map[string]interface{} `json:"data"`
	Timestamp int64                  `json:"timestamp"`
}

func (m *Message) GetMessageType() uint32 { return m.Type }
func (m *Message) GetSequence() uint32    { return m.Sequence }

// Protocol JSON协议的实现
type Protocol struct {
	maxMessageSize uint32
}

// New 创建新的JSON协议实例
func New(maxMessageSize uint32) *Protocol {
	return &Protocol{
		maxMessageSize: maxMessageSize,
	}
}

// findNextMagic 在数据中查找下一个魔数位置
func findNextMagic(data []byte) int {
	for i := 0; i <= len(data)-2; i++ {
		if binary.BigEndian.Uint16(data[i:i+2]) == MagicNumber {
			return i
		}
	}
	return -1
}

// Pack 将消息打包成字节流
func (p *Protocol) Pack(message interface{}) ([]byte, error) {
	msg, ok := message.(*Message)
	if !ok {
		return nil, errors.WrapError(errors.ErrorTypeProtocol, nil, "message must be *json.Message")
	}

	// 1. 设置时间戳
	if msg.Timestamp == 0 {
		msg.Timestamp = time.Now().UnixNano()
	}

	// 2. 序列化消息体
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, errors.WrapError(errors.ErrorTypeProtocol, err, "failed to marshal message")
	}

	// 3. 计算总长度
	totalLen := HeaderSize + len(data)
	if uint32(totalLen) > p.maxMessageSize {
		return nil, errors.ErrMessageTooLarge
	}

	// 4. 构造消息包
	packet := make([]byte, totalLen)
	// 写入魔数
	binary.BigEndian.PutUint16(packet[0:2], MagicNumber)
	// 写入长度
	binary.BigEndian.PutUint32(packet[2:6], uint32(len(data)))
	// 写入数据
	copy(packet[HeaderSize:], data)

	return packet, nil
}

// Unpack 从字节流中解析消息
func (p *Protocol) Unpack(data []byte) (interface{}, int, error) {
	// 1. 查找魔数
	pos := findNextMagic(data)
	if pos == -1 {
		// 没找到魔数，保留所有数据等待更多数据
		return nil, 0, errors.ErrShortRead
	}

	// 2. 如果魔数不在开头，丢弃之前的数据
	if pos > 0 {
		return nil, pos, errors.ErrInvalidFormat
	}

	// 3. 检查头部数据是否完整
	if len(data) < HeaderSize {
		return nil, 0, errors.ErrShortRead
	}

	// 4. 获取数据长度
	dataLen := binary.BigEndian.Uint32(data[2:6])
	totalLen := HeaderSize + int(dataLen)

	// 5. 检查长度是否合理
	if uint32(totalLen) > p.maxMessageSize {
		if len(data) < totalLen {
			return nil, len(data), errors.ErrMessageTooLarge
		}
		return nil, totalLen, errors.ErrMessageTooLarge
	}

	// 6. 检查数据是否完整
	if len(data) < totalLen {
		return nil, 0, errors.ErrShortRead
	}

	// 7. 解析JSON数据
	var msg Message
	if err := json.Unmarshal(data[HeaderSize:totalLen], &msg); err != nil {
		// JSON解析错误，丢弃整个包
		return nil, totalLen, errors.WrapError(errors.ErrorTypeProtocol, err, "unmarshal json failed")
	}

	// 8. 验证必要字段
	if msg.Type == 0 || msg.Sequence == 0 || msg.Timestamp == 0 {
		// 无效消息，丢弃整个包
		return nil, totalLen, errors.ErrInvalidFormat
	}

	return &msg, totalLen, nil
}
