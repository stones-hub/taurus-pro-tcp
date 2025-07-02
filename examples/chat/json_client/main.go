package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"

	"github.com/stones-hub/taurus-pro-tcp/examples/chat"
	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/client"
	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Println("使用方法: json_client <服务器地址> <用户名>")
		fmt.Println("示例: json_client localhost:8080 alice")
		os.Exit(1)
	}

	serverAddr := os.Args[1]
	username := os.Args[2]

	// 创建客户端处理器
	handler := chat.NewClientHandler(username, func(msg *chat.ChatMessage) {
		// 这里可以添加自定义的消息处理逻辑
	})

	// 创建客户端
	cli, cleanup, err := client.New(
		serverAddr,
		protocol.JSON,
		handler,
		client.WithMaxMsgSize(1024*1024), // 1MB
		client.WithBufferSize(100),
	)
	if err != nil {
		log.Fatalf("创建客户端失败: %v", err)
	}
	defer cleanup()

	// 连接服务器
	if err := cli.Connect(); err != nil {
		log.Fatalf("连接服务器失败: %v", err)
	}

	// 启动客户端
	go cli.Start()

	// 发送加入消息
	sequence := uint32(0)
	joinMsg := &chat.ChatMessage{
		Type:     chat.MsgTypeSystem,
		Sequence: atomic.AddUint32(&sequence, 1),
		From:     username,
		Content:  "加入了聊天室",
	}
	if err := cli.Send(joinMsg.ToJSONMessage()); err != nil {
		log.Printf("发送加入消息失败: %v", err)
	}

	// 读取用户输入
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Println("开始聊天（输入 'quit' 退出）:")
	for scanner.Scan() {
		text := scanner.Text()
		if strings.ToLower(text) == "quit" {
			break
		}

		// 发送消息
		msg := &chat.ChatMessage{
			Type:     chat.MsgTypeUser,
			Sequence: atomic.AddUint32(&sequence, 1),
			From:     username,
			Content:  text,
		}
		if err := cli.Send(msg.ToJSONMessage()); err != nil {
			log.Printf("发送消息失败: %v", err)
		}
	}

	// 发送离开消息
	leaveMsg := &chat.ChatMessage{
		Type:     chat.MsgTypeSystem,
		Sequence: atomic.AddUint32(&sequence, 1),
		From:     username,
		Content:  "离开了聊天室",
	}
	if err := cli.Send(leaveMsg.ToJSONMessage()); err != nil {
		log.Printf("发送离开消息失败: %v", err)
	}
}
