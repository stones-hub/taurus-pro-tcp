package chat

import (
	"context"
	"log"
	"net"

	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol/binary"
	jsonproto "github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol/json"
)

// ClientHandler 客户端消息处理器
type ClientHandler struct {
	username  string
	onMessage func(msg *ChatMessage)
}

// NewClientHandler 创建新的客户端处理器
func NewClientHandler(username string, onMessage func(msg *ChatMessage)) *ClientHandler {
	return &ClientHandler{
		username:  username,
		onMessage: onMessage,
	}
}

// OnConnect 处理连接建立
func (h *ClientHandler) OnConnect(ctx context.Context, conn net.Conn) {
	log.Printf("连接到服务器: %s", conn.RemoteAddr())
}

// OnMessage 处理收到的消息
func (h *ClientHandler) OnMessage(ctx context.Context, conn net.Conn, message interface{}) {
	var msg *ChatMessage

	switch m := message.(type) {
	case *jsonproto.Message:
		msg = FromJSONMessage(m)
	case *binary.Message:
		msg = FromBinaryMessage(m)
	default:
		log.Printf("无效的消息类型: %T", message)
		return
	}

	// 调用消息回调函数
	if h.onMessage != nil {
		h.onMessage(msg)
	}

	// 根据消息类型处理
	switch msg.Type {
	case MsgTypeHeartbeat:
		log.Printf("收到心跳响应: %s", msg.Content)
	case MsgTypeUser:
		log.Printf("[%s] %s", msg.From, msg.Content)
	case MsgTypeSystem:
		log.Printf("[系统] %s", msg.Content)
	}
}

// OnClose 处理连接关闭
func (h *ClientHandler) OnClose(ctx context.Context, conn net.Conn) {
	log.Printf("与服务器的连接已关闭: %s", conn.RemoteAddr())
}

// OnError 处理错误
func (h *ClientHandler) OnError(ctx context.Context, conn net.Conn, err error) {
	log.Printf("发生错误: %v", err)
}
