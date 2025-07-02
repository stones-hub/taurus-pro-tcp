# 聊天室示例

这个示例展示了如何使用 taurus-pro-tcp 库创建一个简单的聊天室应用。示例包含了 JSON 和二进制协议的实现。

## 功能特点

- 支持 JSON 和二进制协议
- 实现了基本的聊天功能
- 包含心跳检测
- 支持系统消息和用户消息
- 优雅的启动和关闭

## 目录结构

```
chat/
├── README.md
├── message.go              # 消息定义
├── client_handler.go       # 客户端处理器
├── server_handler.go       # 服务器处理器
├── json_server/           # JSON 协议服务器
│   └── main.go
├── json_client/           # JSON 协议客户端
│   └── main.go
├── binary_server/        # 二进制协议服务器
│   └── main.go
└── binary_client/        # 二进制协议客户端
    └── main.go
```

## 构建

在项目根目录下运行以下命令：

```bash
# 构建 JSON 协议服务器和客户端
go build -o bin/json_server examples/chat/json_server/main.go
go build -o bin/json_client examples/chat/json_client/main.go

# 构建二进制协议服务器和客户端
go build -o bin/binary_server examples/chat/binary_server/main.go
go build -o bin/binary_client examples/chat/binary_client/main.go
```

## 运行示例

1. JSON 协议示例：

```bash
# 启动服务器
./bin/json_server

# 在其他终端启动客户端
./bin/json_client localhost:8080 alice
./bin/json_client localhost:8080 bob
```

2. 二进制协议示例：

```bash
# 启动服务器
./bin/binary_server

# 在其他终端启动客户端
./bin/binary_client localhost:8081 alice
./bin/binary_client localhost:8081 bob
```

## 使用说明

1. 服务器启动后会监听指定端口（JSON: 8080, 二进制: 8081）
2. 客户端需要提供服务器地址和用户名
3. 输入消息后按回车发送
4. 输入 'quit' 退出聊天室
5. 客户端会自动发送心跳包（仅二进制协议）

## 消息类型

- 系统消息：用户加入/离开聊天室
- 用户消息：用户发送的聊天内容
- 心跳消息：用于保持连接活跃（仅二进制协议）

## 注意事项

1. 确保服务器端口未被占用
2. 建议先启动服务器，再启动客户端
3. 可以同时运行多个客户端进行群聊
4. 服务器可以优雅地处理 Ctrl+C 信号 