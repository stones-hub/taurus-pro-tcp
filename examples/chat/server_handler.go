package chat

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp"
	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol/binary"
	jsonproto "github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol/json"
)

// ServerHandler 服务器端消息处理器
type ServerHandler struct {
	// 连接管理
	connections sync.Map
	// 消息序列号
	sequence uint32
}

// NewServerHandler 创建新的服务器处理器
func NewServerHandler() *ServerHandler {
	return &ServerHandler{}
}

// OnConnect 处理新连接
func (h *ServerHandler) OnConnect(conn *tcp.Connection) {
	log.Printf("新连接建立: %s", conn.RemoteAddr())
	h.connections.Store(conn.ID(), conn)
}

// OnMessage 处理收到的消息
func (h *ServerHandler) OnMessage(conn *tcp.Connection, message interface{}) {
	var msg *ChatMessage
	var isJSON bool

	switch m := message.(type) {
	case *jsonproto.Message:
		msg = FromJSONMessage(m)
		isJSON = true
	case *binary.Message:
		msg = FromBinaryMessage(m)
		isJSON = false
	default:
		log.Printf("无效的消息类型: %T", message)
		return
	}

	// 根据消息类型处理
	switch msg.Type {
	case MsgTypeHeartbeat:
		// 心跳消息，直接返回
		response := &ChatMessage{
			Type:     MsgTypeHeartbeat,
			Sequence: atomic.AddUint32(&h.sequence, 1),
			From:     "server",
			To:       msg.From,
			Content:  "pong",
		}
		// 根据原消息类型选择响应格式
		var resp interface{}
		if isJSON {
			resp = response.ToJSONMessage()
		} else {
			resp = response.ToBinaryMessage()
		}
		if err := conn.Send(resp); err != nil {
			log.Printf("发送心跳响应失败: %v", err)
		}

	case MsgTypeUser:
		// 用户消息，广播给所有连接
		h.broadcast(msg, isJSON)

	case MsgTypeSystem:
		// 系统消息，记录日志
		log.Printf("系统消息: %s", msg.Content)
		// 广播系统消息
		h.broadcast(msg, isJSON)
	}
}

// OnClose 处理连接关闭
func (h *ServerHandler) OnClose(conn *tcp.Connection) {
	log.Printf("连接关闭: %s", conn.RemoteAddr())
	h.connections.Delete(conn.ID())

	// 广播用户离开消息
	msg := &ChatMessage{
		Type:     MsgTypeSystem,
		Sequence: atomic.AddUint32(&h.sequence, 1),
		From:     "system",
		Content:  fmt.Sprintf("用户 %s 离开了聊天室", conn.RemoteAddr()),
	}
	// 默认使用 JSON 格式广播离开消息
	h.broadcast(msg, true)
}

// OnError 处理错误
func (h *ServerHandler) OnError(conn *tcp.Connection, err error) {
	log.Printf("错误发生: %v", err)
}

// broadcast 广播消息给所有连接
func (h *ServerHandler) broadcast(msg *ChatMessage, isJSON bool) {
	h.connections.Range(func(key, value interface{}) bool {
		conn := value.(*tcp.Connection)
		var resp interface{}
		if isJSON {
			resp = msg.ToJSONMessage()
		} else {
			resp = msg.ToBinaryMessage()
		}
		if err := conn.Send(resp); err != nil {
			log.Printf("广播消息失败: %v", err)
		}
		return true
	})
}
