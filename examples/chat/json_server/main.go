package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/stones-hub/taurus-pro-tcp/examples/chat"
	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp"
	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol"
)

func main() {
	// 创建 JSON 协议
	proto, err := protocol.NewProtocol(protocol.WithType(protocol.JSON))
	if err != nil {
		log.Fatalf("创建协议失败: %v", err)
	}

	// 创建服务器处理器
	handler := chat.NewServerHandler()

	// 创建服务器
	server, cleanup, err := tcp.NewServer(
		":8080",
		proto,
		handler,
		tcp.WithMaxConnections(100),
		tcp.WithConnectionBufferSize(1024),
		tcp.WithConnectionMaxMessageSize(1024*1024), // 1MB
	)
	if err != nil {
		log.Fatalf("创建服务器失败: %v", err)
	}
	defer cleanup()

	// 处理系统信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// 启动服务器
	go func() {
		log.Printf("服务器启动在 :8080")
		if err := server.Start(); err != nil {
			log.Printf("服务器停止: %v", err)
		}
	}()

	// 等待系统信号
	<-sigCh
	log.Println("收到系统信号，正在关闭服务器...")
}
