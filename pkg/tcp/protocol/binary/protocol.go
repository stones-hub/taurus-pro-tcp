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

package binary

import (
	"encoding/binary"
	"hash/crc32"

	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/errors"
)

// 格式：
// +---------------+---------------+---------------+---------------+---------------+---------------+---------------+
// |  Magic(2B)    | Version(1B)   |   Type(4B)    | Sequence(4B)  | DataLen(4B)   |     Data      | Checksum(4B)  |
// +---------------+---------------+---------------+---------------+---------------+---------------+---------------+

const (
	// HeaderSize = 2(magic) + 1(version) + 4(type) + 4(sequence) + 4(length)
	HeaderSize = 15
	// FooterSize = 4(crc32)
	FooterSize  = 4
	MagicNumber = 0xCAFE
)

// Message 二进制协议的消息结构
type Message struct {
	Version  uint8
	Type     uint32
	Sequence uint32
	Data     []byte
}

func (m *Message) GetMessageType() uint32 { return m.Type }
func (m *Message) GetSequence() uint32    { return m.Sequence }

// Protocol 二进制协议的实现
type Protocol struct {
	maxMessageSize uint32
}

// New 创建新的二进制协议实例
func New(maxMessageSize uint32) *Protocol {
	return &Protocol{
		maxMessageSize: maxMessageSize,
	}
}

// Pack 将消息打包成字节流
func (p *Protocol) Pack(message interface{}) ([]byte, error) {
	msg, ok := message.(*Message)
	if !ok {
		return nil, errors.WrapError(errors.ErrorTypeProtocol, nil, "message must be *binary.Message")
	}

	// 1. 计算总长度
	totalLen := HeaderSize + len(msg.Data) + FooterSize
	if uint32(totalLen) > p.maxMessageSize {
		return nil, errors.ErrMessageTooLarge
	}

	// 2. 构造消息
	packet := make([]byte, totalLen)

	// 3. 写入包头
	binary.BigEndian.PutUint16(packet[0:2], MagicNumber)
	packet[2] = msg.Version
	binary.BigEndian.PutUint32(packet[3:7], msg.Type)
	binary.BigEndian.PutUint32(packet[7:11], msg.Sequence)
	binary.BigEndian.PutUint32(packet[11:15], uint32(len(msg.Data)))

	// 4. 写入数据
	copy(packet[HeaderSize:], msg.Data)

	// 5. 计算并写入CRC32
	crc := crc32.ChecksumIEEE(packet[:HeaderSize+len(msg.Data)])
	binary.BigEndian.PutUint32(packet[HeaderSize+len(msg.Data):], crc)

	return packet, nil
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

	// 3. 检查包头数据是否足够
	if len(data) < HeaderSize {
		return nil, 0, errors.ErrShortRead
	}

	// 4. 获取并检查长度
	dataLen := binary.BigEndian.Uint32(data[11:15])
	totalLen := HeaderSize + int(dataLen) + FooterSize

	// 5. 检查长度是否合理
	if uint32(totalLen) > p.maxMessageSize {
		// 长度超过限制，丢弃整个包的长度
		// 如果数据不足，就丢弃现有的所有数据
		if len(data) < totalLen {
			return nil, len(data), errors.ErrMessageTooLarge
		}
		return nil, totalLen, errors.ErrMessageTooLarge
	}

	// 6. 检查数据是否足够
	if len(data) < totalLen {
		return nil, 0, errors.ErrShortRead
	}

	// 7. 验证CRC32
	crc := binary.BigEndian.Uint32(data[totalLen-4 : totalLen])
	if crc32.ChecksumIEEE(data[:totalLen-4]) != crc {
		// CRC错误，丢弃整个包
		return nil, totalLen, errors.ErrChecksum
	}

	// 8. 解析消息
	msg := &Message{
		Version:  data[2],
		Type:     binary.BigEndian.Uint32(data[3:7]),
		Sequence: binary.BigEndian.Uint32(data[7:11]),
		Data:     make([]byte, dataLen),
	}
	copy(msg.Data, data[HeaderSize:HeaderSize+int(dataLen)])

	return msg, totalLen, nil
}
