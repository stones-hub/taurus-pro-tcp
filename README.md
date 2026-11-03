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
- **连接状态**：当前连接数、拒绝次数等（不包含内存等进程资源指标）

### 🔄 可靠性保障
- **重试退避**：读写遇到临时性网络错误时按指数退避重试（客户端不支持断线自动重连，`Close` 之后需新建客户端）
- **错误处理**：完善的错误分类和处理机制
- **空闲检测**：超过空闲超时没有读写活动的连接会被关闭（心跳消息需由业务协议自行实现，见聊天示例）

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
    "context"
    "log"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/stones-hub/taurus-pro-tcp/pkg/tcp"
    "github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol"
)

type EchoHandler struct{}

func (h *EchoHandler) OnConnect(conn *tcp.Connection) {
    log.Printf("新连接: %s", conn.RemoteAddr())
}

func (h *EchoHandler) OnMessage(conn *tcp.Connection, message interface{}) {
    // 原样回复
    if err := conn.Send(message); err != nil {
        log.Printf("回复失败: %v", err)
    }
}

func (h *EchoHandler) OnClose(conn *tcp.Connection) {
    log.Printf("连接关闭: %s", conn.RemoteAddr())
}

// conn 可能为 nil：服务器级别的错误（Accept 失败、连接数达到上限）不属于任何连接
func (h *EchoHandler) OnError(conn *tcp.Connection, err error) {
    if conn == nil {
        log.Printf("服务器错误: %v", err)
        return
    }
    log.Printf("连接 %s 错误: %v", conn.RemoteAddr(), err)
}

func main() {
    // 创建协议实例（json 或 binary）
    proto, err := protocol.NewProtocol(protocol.WithType(protocol.JSON))
    if err != nil {
        log.Fatal(err)
    }

    // 创建服务器，第二个返回值是清理函数（等同于 server.Stop）
    server, _, err := tcp.NewServer(":8080", proto, &EchoHandler{})
    if err != nil {
        log.Fatal(err)
    }

    // Start 会阻塞到服务器停止，所以放在协程里
    go func() {
        if err := server.Start(); err != nil {
            log.Printf("服务器启动失败: %v", err)
        }
    }()

    sig := make(chan os.Signal, 1)
    signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
    <-sig

    // 有界地停止：最多等 5 秒
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    if err := server.StopContext(ctx); err != nil {
        log.Printf("停止未在期限内完成: %v", err)
    }
}
```

JSON 协议的帧格式为 `0xFEFF`（2 字节魔数）+ 4 字节大端长度 + JSON 内容，JSON 消息类型是
`*json.Message`（`pkg/tcp/protocol/json`），`type`、`sequence`、`timestamp` 都不能为 0。

### 基础客户端示例

```go
package main

import (
    "context"
    "log"
    "net"
    "time"

    "github.com/stones-hub/taurus-pro-tcp/pkg/tcp/client"
    "github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol"
    "github.com/stones-hub/taurus-pro-tcp/pkg/tcp/protocol/json"
)

type MyClientHandler struct{}

func (h *MyClientHandler) OnConnect(ctx context.Context, conn net.Conn) {
    log.Println("已连接到服务器")
}

func (h *MyClientHandler) OnMessage(ctx context.Context, conn net.Conn, msg interface{}) {
    log.Printf("收到服务器消息: %v", msg)
}

func (h *MyClientHandler) OnClose(ctx context.Context, conn net.Conn) {
    log.Println("与服务器断开连接")
}

// 拨号失败等连接建立前的错误 conn 为 nil
func (h *MyClientHandler) OnError(ctx context.Context, conn net.Conn, err error) {
    log.Printf("客户端错误: %v", err)
}

func main() {
    cli, cleanup, err := client.New("localhost:8080", protocol.JSON, &MyClientHandler{})
    if err != nil {
        log.Fatal("创建客户端失败:", err)
    }
    defer cleanup()

    if err := cli.Connect(); err != nil {
        log.Fatal("连接服务器失败:", err)
    }
    // Start 启动收发协程并阻塞，放在协程里
    go cli.Start()

    msg := &json.Message{
        Type:     1,
        Sequence: 1,
        Data:     map[string]interface{}{"text": "Hello, Server!"},
    }
    if err := cli.Send(msg); err != nil {
        log.Printf("发送消息失败: %v", err)
    }

    time.Sleep(5 * time.Second)
}
```

## 📚 详细文档

### 服务器配置选项

```go
server, cleanup, err := tcp.NewServer(":8080", proto, handler,
    // 连接相关配置
    tcp.WithConnectionBufferSize(2048),             // 每个连接的发送队列长度（条）
    tcp.WithConnectionMaxMessageSize(2*1024*1024),  // 单条消息上限 (2MB)
    tcp.WithConnectionIdleTimeout(10*time.Minute),  // 空闲超时
    tcp.WithConnectionRateLimiter(200),             // 每连接每秒消息数上限

    // 服务器配置
    tcp.WithMaxConnections(5000),                   // 最大并发连接数
)
```

### 连接数上限

连接数达到 `WithMaxConnections` 后，服务器暂停 Accept：新连接留在内核的 listen 队列里（客户端看到的是
连接成功但没有响应），等已有连接关闭、空出槽位后再按顺序接入，不会被主动关闭。暂停期间服务器每秒
以 `OnError(nil, errors.ErrTooManyConnections)` 上报一次，并累加指标 `connection_refused`。

### Handler 注册与查找

`RegisterHandler(name, h)` 按名字注册，`GetHandler(name)` 按名字查找：

- `GetHandler` 只在调用的那一刻查表，所以必须**先注册、后查找**。框架代码通常在启动阶段就调用
  `GetHandler`，因此注册应放在包级 `init()` 里，放在服务启动后的钩子里就太晚了。
- 名字找不到时 `GetHandler` **不会返回 nil，也不会报错**，而是回退到内置的默认 handler：只打日志、
  不回复任何消息。
- 同名重复注册时保留先注册的那个，并打印一条日志。

```go
func init() {
    tcp.RegisterHandler("echo", &EchoHandler{})
}
```

### 停止服务器

停止分两层：**网络层**（监听器、连接的 socket）和**用户回调**（`OnMessage`、`OnClose` 等）。

- `Stop()`（也就是 `NewServer` 返回的清理函数）：同步关闭网络层后返回。返回时监听器已关闭（新连接被拒绝），
  所有已登记连接的 socket 已关闭（对端看到连接关闭）。`Stop` **不调用、也不等待**任何用户回调：
  每个连接的 `OnClose` 由该连接的处理协程随后调用，正在执行的 `OnMessage` 会继续执行到返回。
  `Stop` 可以重复调用、并发调用、在 `Start` 之前调用，也可以在任意回调（包括 `OnClose`）里调用。
- `StopContext(ctx)`：先同步执行与 `Stop` 相同的网络层关闭（这一步不受 `ctx` 影响，即使 `ctx` 已取消
  也会先完成），再等待接入协程和所有连接处理协程（包括其中的 `OnMessage` 和随后的 `OnClose`）退出。
  全部完成返回 `nil`；`ctx` 先结束时返回包装了 `ctx.Err()` 的错误，可用
  `errors.Is(err, context.DeadlineExceeded)` 判断，这表示网络层已关闭、但仍有用户回调在执行——
  Go 无法强制终止回调，它们会继续运行直到自行返回。`StopContext` 不额外启动协程，可以超时后再调用一次。
  在回调里调用 `StopContext` 会一直等到 `ctx` 结束，因为当前回调本身还没有返回。
- `Start()` 会阻塞到服务器停止且所有连接处理协程（含用户回调）退出后才返回：正常停止返回 `nil`；
  Accept 遇到致命错误导致服务器退出时返回同时包装了 `errors.ErrSystemFatal` 和底层错误的 error。
  服务器只能启动一次：重复 `Start` 返回 `ErrServerAlreadyStarted`，`Stop` 之后再 `Start` 返回 `ErrServerStopped`。
- 需要在继续退出流程前确认 `OnClose` 都已执行完（例如之后要关闭 `OnClose` 用到的数据库），
  请使用 `StopContext` 并检查返回值，而不是只调用 `Stop`。

### 连接回调顺序

- 每个连接 `OnConnect` 先于 `OnClose`，`OnClose` 只调用一次；`OnClose` 在该连接的读写协程全部退出、
  socket 已关闭之后调用，之后不会再有该连接的 `OnConnect` 或 `OnMessage`。
- 读协程先于 `OnConnect` 启动，所以 `OnMessage` 可能与 `OnConnect` 并发，甚至在 `OnConnect` 被调用之前到达；
  需要先初始化连接状态再处理消息时，请在 `OnMessage` 里自行同步。
- 在处理协程开始前就被关闭的连接（例如 `Stop` 恰好发生在接入的瞬间）不会触发任何回调。
- `Connection.Close()` 只关闭网络层并立即返回，不调用 `OnClose`；在回调里调用它是安全的。
- **关闭流程本身是静默的**：本端关闭（`Connection.Close`、`Server.Stop`）不调用 `OnError`；
  对端关闭时 `OnError` 收到一次 `ErrConnectionClosed`。
- **关闭之后主动 `Send` 会报错**：在 `OnClose` 里或关闭之后调用 `Send` 是一次失败的操作，
  返回 `ErrConnectionClosed`，并同时回调 `OnError`。
- 关闭时（包括 `Stop`）发送队列里还没写出的消息会被直接丢弃；需要保证送达请在业务协议里做确认。
- 速率限制按连接计，读写共用一个令牌桶：读方向每次读取和同一次读取中的每条额外消息各消耗一个令牌，
  写方向每条实际要发出的消息消耗一个令牌；没有消息要发时不消耗令牌，也不上报 `ErrRateLimitExceeded`。
- `ConnectionCount()` 返回已登记、处理协程尚未退出的连接数。

### 客户端生命周期

- 客户端只能连接一次，不支持重连：`Connect` 在已连接时返回 `client.ErrAlreadyConnected`，`Close` 之后
  （包括拨号进行中被 `Close`）返回 `client.ErrClientClosed`，拨号得到的连接会被立即关闭。并发调用
  `Connect` 只有一个成功。`Close` 返回后 `IsConnected()` 一定为 `false`。
- `OnClose` 只调用一次，并且一定在已开始的 `OnConnect` 返回之后：在 `OnConnect` 里登记、在 `OnClose`
  里释放的资源不会因为并发而泄漏。如果关闭发生在 `OnConnect` 执行期间（并发的 `Close`、在 `OnConnect`
  里调用 `Close`），`Close` 立即返回，`OnClose` 由 `Connect` 在 `OnConnect` 返回后调用，`Connect` 返回
  `client.ErrClientClosed`。注意这里的"关闭"也包括对端断开：在 `OnConnect` 里调用 `Start` 并一直运行到
  对端断开时，`Connect` 同样返回 `client.ErrClientClosed`（客户端此时确实已关闭）。
- `Start` 启动读写协程并阻塞；读写协程退出后 `Start` 调用 `OnClose` 再返回，所以 `Start` 返回时
  `OnClose` 已执行完，之后不会再有 `OnMessage`。例外：`Start` 与 `Connect` 在不同协程并发、读写结束时
  `OnConnect` 还没返回，`Start` 不等待它，`OnClose` 改由 `Connect` 调用。
- `Close` 先同步关闭传输层（取消 ctx、关闭发送队列和 socket），然后立即返回，不等待任何回调；
  从未 `Start` 且没有 `OnConnect` 在执行时由第一次 `Close` 调用 `OnClose`。`Close` 可以重复调用，
  也可以在任何回调里调用。所有回调都不在锁内调用。
- 一次读取到多帧时客户端会全部解析并依次调用 `OnMessage`。读写遇到临时错误时的退避等待可被 `Close` 打断；
  写重试前会重新设置写入期限。
- 本端 `Close` 不调用 `OnError`；`Close` 之后 `Send` 返回 `client.ErrClientClosed` 并回调 `OnError`。
- `SimpleReceive` 同步接收一条完整消息：数据不足一帧时继续读取；一次读到的多余数据（下一帧或半帧）
  缓存给下一次调用，之后 `Start` 时交给读协程继续解析。它不能与 `Start` 的读协程或另一个
  `SimpleReceive` 同时使用，否则立即返回 `client.ErrReceiveConflict`；反过来，`SimpleReceive` 进行中调用
  `Start` 会回调 `OnError(ErrReceiveConflict)` 并立即返回，之后仍可再次 `Start`。阻塞中的 `SimpleReceive`
  被本端 `Close` 打断时返回 `client.ErrClientClosed`，不回调 `OnError`。
- 写入时 `Write` 只写出一部分并返回临时错误（例如写期限到达）时，只重发尚未写出的部分，不会重复发送帧的前缀。
- 回调 panic 不会被库 recover：读写协程里的 panic 会使进程崩溃；如果在 `Connect` 的调用方 recover 了
  `OnConnect` 的 panic，之后 `OnClose` 不会再被调用，在 `OnConnect` 里登记的资源需要自行清理。

### 消息大小上限

有两层上限，实际生效的是**两者中较小的那个**：

| 位置 | 默认值 | 配置方式 |
|---|---|---|
| 协议（`protocol.NewProtocol`） | 10MB | `protocol.WithMaxMessageSize` |
| 服务端连接 | 1MB | `tcp.WithConnectionMaxMessageSize` |
| 客户端 | 1MB | `client.WithMaxMsgSize`（客户端用同一个值创建自己的协议实例） |

服务端的协议实例由调用方传入，所以用默认值时协议允许 10MB、连接只允许 1MB：
`Send` 超过 1MB 的消息返回 `ErrMessageTooLarge`；收到超过 1MB 的帧时，接收缓冲区超过连接上限后被清空、
上报 `ErrBufferOverflow`，这一帧被丢弃。建议把协议上限和连接上限设成同一个值。

### 客户端配置选项

```go
cli, cleanup, err := client.New("localhost:8080", protocol.JSON, handler,
    // 连接配置
    client.WithConnectionTimeout(10*time.Second),  // 连接超时
    client.WithIdleTimeout(5*time.Minute),         // 空闲超时
    client.WithMaxMsgSize(1024*1024),              // 最大消息大小

    // 重试配置
    client.WithMaxRetries(5),                      // 最大重试次数
    client.WithBaseRetryDelay(time.Second),        // 基础重试延迟
    client.WithMaxRetryDelay(30*time.Second),      // 最大重试延迟

    // 缓冲区配置
    client.WithBufferSize(2048),                   // 发送队列长度（条）
)
```

### 协议实现

```go
// 通过类型创建（默认 JSON，默认单条上限 10MB）
proto, err := protocol.NewProtocol(
    protocol.WithType(protocol.JSON),          // 或 protocol.BINARY
    protocol.WithMaxMessageSize(1024*1024),    // 1MB
)
```

#### JSON 协议（`pkg/tcp/protocol/json`）
```go
// 帧：Magic 0xFEFF(2B) + Length(4B, 大端) + JSON
type Message struct {
    Type      uint32                 `json:"type"`
    Sequence  uint32                 `json:"sequence"`
    Data      map[string]interface{} `json:"data"`
    Timestamp int64                  `json:"timestamp"` // 为 0 时 Pack 自动填充
}
```

#### 二进制协议（`pkg/tcp/protocol/binary`）
```go
// 帧：Magic 0xCAFE(2B) + Version(1B) + Type(4B) + Sequence(4B) + DataLen(4B) + Data + CRC32(4B)
type Message struct {
    Version  uint8
    Type     uint32
    Sequence uint32
    Data     []byte
}
```

一次读取到多帧时，服务器会把缓冲区里的完整帧全部解析并依次调用 `OnMessage`；不完整的帧保留到
下一次读取，非法数据只丢弃协议指定的长度。

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
- **连接超时**：客户端默认 5 秒连接超时
- **空闲超时**：默认 30 分钟空闲超时
- **发送队列**：每个连接默认可缓存 1024 条待发送消息

### 消息处理
- **最大消息大小**：服务端连接和客户端默认 1MB，协议默认 10MB，实际生效的是较小值（见"消息大小上限"）
- **速率限制**：默认每秒 100 条消息
- **消息队列**：异步消息处理，非阻塞

### 监控指标
- **连接统计**：总连接数、当前连接数、拒绝连接数（`connection_refused` 统计的是连接数已满时的拒绝轮询次数，不是被拒绝的客户端个数）
- **消息统计**：接收/发送消息数、字节数
- **性能指标**：消息延迟、错误率
- **系统指标**：运行时长

## 🧪 测试

### 运行测试
```bash
# 运行所有测试
go test ./...

# 运行特定包的测试
go test ./pkg/tcp

# 带数据竞争检测运行
go test -race ./...
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
