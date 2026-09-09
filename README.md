# MewCode

MewCode 是一个使用 Go 编写的终端 AI 编程助手（coding agent）。它可以在当前代码仓库中读取和修改文件、执行命令、调用 MCP 工具、运行子 Agent，并通过会话、记忆、技能和团队协作能力完成复杂的开发任务。

项目的核心设计是：命令行入口负责选择运行模式，internal/agent 负责 Agent 主循环，模型、工具、权限、记忆和协作能力分别由独立包提供。

## 功能概览

- 交互式 TUI：默认启动 Bubble Tea 终端界面。
- 非交互式执行：使用 -p/--print 执行一次任务，可输出文本或 JSON。
- 远程服务：使用 --remote 启动 WebSocket/Web 服务。
- 多模型接入：支持 Anthropic、OpenAI 和 OpenAI 兼容协议。
- 工具调用：内置文件读写、编辑、搜索、Shell、Diff、权限控制等工具，也可以加载 MCP 工具。
- 子 Agent 与团队：支持一次性子 Agent，以及通过 tmux/iTerm 或进程内运行的长期协作团队。
- 项目记忆与技能：支持项目级/用户级记忆、技能目录、项目指令文件和自动上下文压缩。
- 工作树隔离：可为任务创建独立 Git worktree。

## 环境要求

- Go 1.25+
- Git
- 一个可用的 LLM Provider 和 API Key
- 若配置命令型 MCP Server，还需要对应运行时，例如 Node.js 和 npx

## 快速开始

### 获取代码并安装依赖

~~~
git clone <仓库地址>
cd mewcode
go mod download
~~~

### 创建配置

MewCode 按以下顺序查找配置，并合并存在的文件：

1. ~/.mewcode/config.yaml
2. 当前项目的 .mewcode/config.yaml
3. 当前项目的 .mewcode/config.local.yaml

可以从示例复制：

~~~
cp .mewcode/config.yaml.example .mewcode/config.yaml
~~~

最小配置示例：

~~~
providers:
  - name: my-provider
    protocol: openai-compat
    base_url: https://api.example.com/v1
    api_key: ${OPENAI_API_KEY}
    model: model-name

permission_mode: default
~~~

protocol 支持 anthropic、openai 和 openai-compat。API Key 也可以通过环境变量提供：Anthropic 使用 ANTHROPIC_API_KEY，OpenAI 和 OpenAI 兼容服务使用 OPENAI_API_KEY。

请不要把真实 API Key 提交到 Git。建议使用环境变量，并将本地覆盖配置放入 .mewcode/config.local.yaml。

### 启动

直接运行源码：

~~~
go run ./cmd/mewcode
~~~

或构建二进制：

~~~
go build -o mewcode ./cmd/mewcode
./mewcode
~~~

默认启动交互式 TUI，建议从希望 MewCode 操作的项目目录启动。

## 命令行运行模式

### 交互式模式

~~~
mewcode
~~~

TUI 中直接输入自然语言任务；以 / 开头的是斜杠命令。常用命令包括：

- /help：查看命令列表
- /clear：清空当前会话
- /compact：压缩会话上下文
- /mcp：查看 MCP 状态
- /resume：恢复历史会话（当前版本支持时）
- /plan：进入计划模式（当前版本支持时）

### 非交互式模式

~~~
mewcode -p "请检查这个项目的错误处理并给出修改建议"
cat task.txt | mewcode --print
mewcode -p "列出项目的主要模块" --output-format json
~~~

-p/--print 没有提示词参数时会从标准输入读取。输出格式默认为纯文本，也可以使用 --output-format json。

### 远程服务模式

~~~
mewcode --remote
mewcode --remote 127.0.0.1:9000
~~~

默认监听 :18888，使用与普通模式相同的配置和 Provider。

### 团队队员模式

团队由 Lead Agent 自动创建和管理。队员进程的格式为：

~~~
mewcode --teammate --team-name <team-name> --agent-name <agent-name>
~~~

通常不需要手动执行，这是 tmux/iTerm 后端启动队员时使用的内部入口。

## 配置字段

| 字段 | 作用 |
| --- | --- |
| providers | LLM Provider 列表，默认使用第一个 |
| providers[].protocol | anthropic、openai 或 openai-compat |
| providers[].base_url | Provider API 地址 |
| providers[].api_key | API Key；留空时从环境变量读取 |
| providers[].model | 模型名称 |
| providers[].thinking | 是否启用思考模式 |
| providers[].context_window | 可选，覆盖上下文窗口 |
| providers[].max_output_tokens | 可选，覆盖最大输出 Token 数 |
| permission_mode | 默认权限模式 |
| mcp_servers | MCP Server 配置，可使用命令或 HTTP/SSE |
| hooks | 工具生命周期 Hook |
| sandbox | 沙箱开关、自动放行和网络开关 |
| enable_coordinator_mode | 是否启用团队 Coordinator 模式 |
| enable_fork | 是否允许使用 fork 子 Agent |

命令型 MCP 示例：

~~~
mcp_servers:
  - name: context7
    command: npx
    args: ["-y", "@upstash/context7-mcp"]
~~~

HTTP MCP 示例：

~~~
mcp_servers:
  - name: my-http-mcp
    url: https://example.com/mcp
    transport: http
    headers:
      Authorization: "Bearer ${MY_TOKEN}"
~~~

## 项目目录结构

测试文件通常与实现文件位于同一目录，并使用 _test.go 后缀。

~~~
mewcode/
├── cmd/mewcode/              # 可执行程序入口和命令行模式
│   ├── main.go               # 解析模式、加载配置、启动 TUI/远程服务
│   ├── print.go              # -p/--print 非交互式执行和输出格式
│   └── teammate.go           # --teammate 队员进程入口
├── internal/                 # 项目业务代码（不对外暴露 Go 包）
│   ├── agent/                # Agent 主循环、事件和流式工具执行
│   ├── agents/               # 子 Agent 定义、加载器、任务管理
│   ├── llm/                  # Anthropic/OpenAI/兼容客户端
│   ├── tools/                # 内置工具、工具注册表、MCP 调用
│   ├── tui/                  # Bubble Tea 终端 UI 和命令分发
│   ├── config/               # YAML 配置读取、合并和校验
│   ├── permissions/          # 权限模式、路径沙箱和规则引擎
│   ├── sandbox/               # 不同操作系统的沙箱实现
│   ├── mcp/                  # MCP 连接、工具注册和加载策略
│   ├── prompt/               # 系统提示词和环境信息构建
│   ├── conversation/          # 对话消息和 Token 状态
│   ├── compact/               # 上下文压缩与恢复
│   ├── toolresult/            # 工具结果大小控制和落盘
│   ├── memory/                # 用户/项目记忆、提取和召回
│   ├── skills/                # Skill 扫描、解析、安装和执行
│   ├── commands/              # 斜杠命令和项目命令加载
│   ├── teams/                 # 多 Agent 团队、任务板和信箱
│   ├── worktree/              # Git worktree 创建、切换和清理
│   ├── session/               # 会话 ID 和会话持久化
│   ├── history/               # 用户提示词历史
│   ├── filehistory/           # 文件修改历史和回滚
│   ├── todo/                  # Todo/任务列表持久化
│   ├── planfile/              # 计划文件读写
│   ├── hooks/                 # 工具生命周期 Hook
│   ├── remote/                # WebSocket 服务和远程 Web 入口
│   └── crashlog/              # 崩溃记录
├── .mewcode/                 # 当前项目配置和运行时数据
│   ├── config.yaml.example    # 配置模板
│   ├── config.yaml            # 项目配置
│   ├── config.local.yaml      # 本机覆盖配置
│   ├── agents/                # 项目级自定义 Agent
│   ├── skills/                # 项目级 Skill
│   ├── memory/                # 项目级记忆
│   ├── sessions/              # 会话记录和超长工具结果
│   ├── tasks/                 # Todo/任务列表
│   ├── file-history/          # 文件修改历史
│   ├── permissions.yaml       # 项目权限规则
│   ├── permissions.local.yaml # 本机权限规则
│   └── crash.log              # 崩溃日志
├── MEWCODE.md                 # 项目级开发指令
├── go.mod                     # Go 模块和依赖
├── go.sum                     # 依赖校验和
├── todo.md                    # 当前待办记录
└── README.md                  # 项目说明文档
~~~

### 模块协作流程

1. cmd/mewcode/main.go 解析参数，并由 internal/config 加载配置。
2. internal/tui 创建终端界面和会话对象。
3. internal/agent 接收用户消息，通过 internal/prompt 生成上下文，再由 internal/llm 请求模型。
4. 模型返回工具调用后，internal/tools 执行工具；涉及权限时交给 internal/permissions 和 internal/sandbox。
5. 工具结果经过 internal/toolresult 控制大小，消息由 internal/conversation 管理；接近上限时由 internal/compact 压缩。
6. 文件修改、会话、任务和记忆等状态分别写入 .mewcode/ 下对应目录。
7. 子 Agent 和团队功能由 internal/agents、internal/teams 和 internal/worktree 协同完成。

## 指令、Agent、Skill 和记忆

### 指令文件

MewCode 会读取：

- ~/.mewcode/MEWCODE.md、~/.mewcode/AGENTS.md：用户级指令
- 项目目录层级中的 MEWCODE.md、AGENTS.md
- 项目目录层级中的 .mewcode/MEWCODE.md
- 当前目录的 MEWCODE.local.md：本机私有覆盖

根目录的 MEWCODE.md 适合放共享的代码规范、测试命令和架构约束。

### 自定义 Agent

定义文件放在 ~/.mewcode/agents/ 或 .mewcode/agents/，至少需要 name 和 description，也可以声明模型、工具、权限模式、记忆范围、隔离模式和初始提示词。

### Skill

Skill 是带 YAML frontmatter 的标准操作流程文件，放在 ~/.mewcode/skills/ 或 .mewcode/skills/。Skill 可以内联注入当前 Agent，也可以在隔离的 fork 子 Agent 中运行。

### 记忆

- 用户级记忆：~/.mewcode/memory/，跨项目复用。
- 项目级记忆：.mewcode/memory/，只服务于当前项目。
- 会话记录：.mewcode/sessions/，用于恢复上下文和保存超长工具结果。

## 开发与验证

~~~
go test ./...
gofmt -w cmd internal
go build ./cmd/mewcode
~~~

运行单个包的测试：

~~~
go test ./internal/agent
~~~

部分端到端测试需要 MEWCODE_TEST_API_KEY、MEWCODE_TEST_BASE_URL 和 MEWCODE_TEST_MODEL；未配置时会自动跳过。

