# Taurus Pro TCP

[![Go Version](https://img.shields.io/badge/Go-1.27.3+-blue.svg)](https://golang.org)
[![License](https://img.shields.io/badge/License-Apache%202.0-green.svg)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/stones-hub/taurus-pro-tcp)](https://goreportcard.com/report/github.com/stones-hub/taurus-pro-tcp)

**Taurus Pro TCP** 是一个高性能、可扩展的 Go 语言 TCP 通信框架，专为构建高并发、低延迟的网络应用而设计。该框架提供了完整的服务器端和客户端实现，支持多种协议格式，并内置了丰富的监控和错误处理机制。

## ✨ 主要特性

### 🚀 高性能设计
- **异步 I/O 处理**：基于 Go 的 goroutine 和 channel 实现高效的并发处理
- **连接池管理**：智能的连接生命周期管理，支持连接复用和自动清理
- **内存优化**：可配置的缓冲区大小和消息大小限制，防止内存泄漏

### 🔧 灵活的协议支持
- **多协议支持**：内置 JSON 和二进制协议实现
- **协议扩展**：提供统一的协议接口，易于扩展自定义协议
- **消息格式**：支持结构化消息和二进制消息传输

### 🛡️ 企业级特性
- **连接限制**：可配置的最大连接数限制，防止资源耗尽
- **速率限制**：内置消息频率限制器，防止 DoS 攻击
- **超时控制**：支持连接超时、空闲超时等多种超时机制
- **优雅关闭**：支持优雅关闭，确保数据完整性

### 📊 全面的监控
- **实时指标**：连接数、消息数、字节数等关键指标实时监控
- **性能统计**：消息延迟、错误率等性能指标统计
- **资源使用**：内存使用、连接状态等资源监控

### 🔄 可靠性保障
- **自动重连**：客户端支持自动重连和指数退避重试
- **错误处理**：完善的错误分类和处理机制
- **心跳检测**：支持心跳机制，及时检测连接状态

## 📦 安装

### 前置要求
- Go 1.27.3 或更高版本
- 支持的操作系统：Linux、macOS、Windows

### 安装命令
```bash
go get github.com/stones-hub/taurus-pro-tcp
```

### 依赖管理
```bash
go mod tidy
```

## 🚀 快速开始

### 基础服务器示例

```go
package main

import (
    "log"
    "net/http"
    
    "github.com/stones-hub/taurus-pro-tcp/pkg/tcp"
    "github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol"
)

func main() {
    // 创建协议实例
    jsonProtocol := protocol.NewJSONProtocol()
    
    // 创建服务器
    server := tcp.NewServer(":8080", jsonProtocol, &MyHandler{})
    
    // 启动服务器
    if err := server.Start(); err != nil {
        log.Fatal("服务器启动失败:", err)
    }
    
    // 等待服务器关闭
    server.Wait()
}

type MyHandler struct{}

func (h *MyHandler) OnConnect(conn tcp.Connection) {
    log.Printf("新连接: %s", conn.RemoteAddr())
}

func (h *MyHandler) OnMessage(conn tcp.Connection, message interface{}) {
    log.Printf("收到消息: %v", message)
    // 处理消息逻辑
}

func (h *MyHandler) OnDisconnect(conn tcp.Connection) {
    log.Printf("连接断开: %s", conn.RemoteAddr())
}
```

### 基础客户端示例

```go
package main

import (
    "log"
    "time"
    
    "github.com/stones-hub/taurus-pro-tcp/pkg/tcp/client"
    "github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol"
)

func main() {
    // 创建客户端
    client, cleanup, err := client.New("localhost:8080", protocol.JSON, &MyClientHandler{})
    if err != nil {
        log.Fatal("创建客户端失败:", err)
    }
    defer cleanup()
    
    // 连接服务器
    if err := client.Connect(); err != nil {
        log.Fatal("连接服务器失败:", err)
    }
    
    // 发送消息
    message := map[string]interface{}{
        "type": "hello",
        "data": "Hello, Server!",
    }
    
    if err := client.Send(message); err != nil {
        log.Printf("发送消息失败: %v", err)
    }
    
    // 保持连接一段时间
    time.Sleep(5 * time.Second)
}

type MyClientHandler struct{}

func (h *MyClientHandler) OnConnect() {
    log.Println("已连接到服务器")
}

func (h *MyClientHandler) OnMessage(message interface{}) {
    log.Printf("收到服务器消息: %v", message)
}

func (h *MyClientHandler) OnDisconnect() {
    log.Println("与服务器断开连接")
}
```

## 📚 详细文档

### 服务器配置选项

```go
server := tcp.NewServer(":8080", protocol, handler,
    // 连接相关配置
    tcp.WithConnectionBufferSize(2048),                    // 缓冲区大小
    tcp.WithConnectionMaxMessageSize(2*1024*1024),        // 最大消息大小 (2MB)
    tcp.WithConnectionIdleTimeout(10*time.Minute),        // 空闲超时
    tcp.WithConnectionRateLimiter(200),                   // 每秒消息限制
    
    // 服务器配置
    tcp.WithMaxConnections(5000),                         // 最大连接数
    tcp.WithSilentTime(100*time.Millisecond),            // 静默时间
)
```

### 客户端配置选项

```go
client, cleanup, err := client.New("localhost:8080", protocol.JSON, handler,
    // 连接配置
    client.WithConnectionTimeout(10*time.Second),         // 连接超时
    client.WithIdleTimeout(5*time.Minute),               // 空闲超时
    client.WithMaxMessageSize(1024*1024),                // 最大消息大小
    
    // 重试配置
    client.WithMaxRetries(5),                            // 最大重试次数
    client.WithBaseDelay(time.Second),                   // 基础重试延迟
    client.WithMaxDelay(30*time.Second),                 // 最大重试延迟
    
    // 缓冲区配置
    client.WithBufferSize(2048),                         // 缓冲区大小
)
```

### 协议实现

#### JSON 协议
```go
// 创建 JSON 协议实例
jsonProtocol := protocol.NewJSONProtocol(
    protocol.WithMaxMessageSize(1024*1024),              // 1MB
    protocol.WithType(protocol.JSON),
)

// 消息结构
type ChatMessage struct {
    Type      string `json:"type"`
    User      string `json:"user"`
    Content   string `json:"content"`
    Timestamp int64  `json:"timestamp"`
}
```

#### 二进制协议
```go
// 创建二进制协议实例
binaryProtocol := protocol.NewBinaryProtocol(
    protocol.WithMaxMessageSize(1024*1024),              // 1MB
    protocol.WithType(protocol.BINARY),
)

// 消息结构
type BinaryMessage struct {
    Type      uint32
    User      [32]byte
    Content   []byte
    Timestamp int64
}
```

## 🏗️ 项目结构

```
taurus-pro-tcp/
├── pkg/tcp/                    # 核心 TCP 包
│   ├── server.go              # 服务器实现
│   ├── client/                # 客户端实现
│   │   ├── client.go         # 客户端核心逻辑
│   │   ├── handler.go        # 客户端处理器接口
│   │   ├── options.go        # 客户端配置选项
│   │   └── stat.go           # 客户端统计信息
│   ├── connection.go          # 连接管理
│   ├── handler.go             # 服务器处理器接口
│   ├── metrics.go             # 监控指标收集
│   ├── protocol/              # 协议实现
│   │   ├── protocol.go       # 协议接口定义
│   │   ├── json/             # JSON 协议实现
│   │   └── binary/           # 二进制协议实现
│   └── errors/                # 错误定义
├── examples/                   # 使用示例
│   └── chat/                  # 聊天室示例
│       ├── json_server/       # JSON 协议服务器
│       ├── json_client/       # JSON 协议客户端
│       ├── binary_server/     # 二进制协议服务器
│       └── binary_client/     # 二进制协议客户端
├── bin/                       # 编译后的二进制文件
├── go.mod                     # Go 模块文件
└── LICENSE                    # 许可证文件
```

## 🔧 构建和运行

### 构建项目
```bash
# 构建所有示例
make build

# 或者手动构建
go build -o bin/json_server examples/chat/json_server/main.go
go build -o bin/json_client examples/chat/json_client/main.go
go build -o bin/binary_server examples/chat/binary_server/main.go
go build -o bin/binary_client examples/chat/binary_client/main.go
```

### 运行示例
```bash
# 启动 JSON 协议服务器
./bin/json_server

# 启动 JSON 协议客户端
./bin/json_client localhost:8080 alice

# 启动二进制协议服务器
./bin/binary_server

# 启动二进制协议客户端
./bin/binary_client localhost:8081 bob
```

## 📊 性能特性

### 连接管理
- **最大连接数**：默认 1000，可配置
- **连接超时**：默认 5 秒连接超时
- **空闲超时**：默认 30 分钟空闲超时
- **缓冲区大小**：默认 1024 字节

### 消息处理
- **最大消息大小**：默认 1MB，可配置
- **速率限制**：默认每秒 100 条消息
- **消息队列**：异步消息处理，非阻塞

### 监控指标
- **连接统计**：总连接数、当前连接数、拒绝连接数
- **消息统计**：接收/发送消息数、字节数
- **性能指标**：消息延迟、错误率
- **系统指标**：运行时长、资源使用

## 🧪 测试

### 运行测试
```bash
# 运行所有测试
go test ./...

# 运行特定包的测试
go test ./pkg/tcp

# 运行基准测试
go test -bench=. ./pkg/tcp
```

### 测试覆盖率
```bash
# 生成测试覆盖率报告
go test -coverprofile=coverage.out ./...

# 查看覆盖率报告
go tool cover -html=coverage.out
```

## 🤝 贡献指南

我们欢迎所有形式的贡献！请查看以下指南：

### 提交 Issue
- 使用清晰的标题描述问题
- 提供详细的复现步骤
- 包含环境信息和错误日志

### 提交 Pull Request
- Fork 项目并创建特性分支
- 确保代码通过所有测试
- 添加必要的文档和测试
- 遵循项目的代码风格

### 代码规范
- 遵循 Go 官方代码规范
- 添加必要的注释和文档
- 确保代码的可读性和可维护性

## 📄 许可证

本项目采用 [Apache License 2.0](LICENSE) 许可证。

## 👥 作者

- **作者**: yelei
- **邮箱**: 61647649@qq.com
- **日期**: 2025-06-13

## 🙏 致谢

感谢所有为这个项目做出贡献的开发者和用户。

## 📞 联系我们

- **项目地址**: [https://github.com/stones-hub/taurus-pro-tcp](https://github.com/stones-hub/taurus-pro-tcp)
- **问题反馈**: [Issues](https://github.com/stones-hub/taurus-pro-tcp/issues)
- **讨论交流**: [Discussions](https://github.com/stones-hub/taurus-pro-tcp/discussions)

---

**Taurus Pro TCP** - 构建高性能 TCP 应用的理想选择 🚀
