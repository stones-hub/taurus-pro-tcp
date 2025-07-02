package chat

import (
	"encoding/json"

	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol/binary"
	jsonproto "github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol/json"
)

// MessageType 定义消息类型
type MessageType uint32

const (
	// 系统消息类型
	MsgTypeSystem MessageType = iota + 1
	// 用户消息类型
	MsgTypeUser
	// 心跳消息类型
	MsgTypeHeartbeat
)

// ChatMessage 定义聊天消息结构
type ChatMessage struct {
	Type     MessageType `json:"type"`     // 消息类型
	Sequence uint32      `json:"sequence"` // 序列号
	From     string      `json:"from"`     // 发送者
	To       string      `json:"to"`       // 接收者
	Content  string      `json:"content"`  // 消息内容
}

// GetMessageType 实现 protocol.Message 接口
func (m *ChatMessage) GetMessageType() uint32 {
	return uint32(m.Type)
}

// GetSequence 实现 protocol.Message 接口
func (m *ChatMessage) GetSequence() uint32 {
	return m.Sequence
}

// ToJSONMessage 将 ChatMessage 转换为 JSON 消息
func (m *ChatMessage) ToJSONMessage() *jsonproto.Message {
	data := make(map[string]interface{})
	data["from"] = m.From
	data["to"] = m.To
	data["content"] = m.Content

	return &jsonproto.Message{
		Type:     uint32(m.Type),
		Sequence: m.Sequence,
		Data:     data,
	}
}

// FromJSONMessage 从 JSON 消息转换为 ChatMessage
func FromJSONMessage(msg *jsonproto.Message) *ChatMessage {
	data := msg.Data
	return &ChatMessage{
		Type:     MessageType(msg.Type),
		Sequence: msg.Sequence,
		From:     data["from"].(string),
		To:       data["to"].(string),
		Content:  data["content"].(string),
	}
}

// ToBinaryMessage 将 ChatMessage 转换为二进制消息
func (m *ChatMessage) ToBinaryMessage() *binary.Message {
	// 将消息序列化为 JSON 字节
	data, _ := json.Marshal(m)
	return &binary.Message{
		Type:     uint32(m.Type),
		Sequence: m.Sequence,
		Data:     data,
	}
}

// FromBinaryMessage 从二进制消息转换为 ChatMessage
func FromBinaryMessage(msg *binary.Message) *ChatMessage {
	chatMsg := &ChatMessage{}
	json.Unmarshal(msg.Data, chatMsg)
	return chatMsg
}
