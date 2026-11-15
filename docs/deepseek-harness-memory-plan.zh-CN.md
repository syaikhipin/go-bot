# Go Harness 还原 DeepSeek Harness 长期记忆方案

> 历史设计与调研记录。当前行为、配置和命令以[文档索引](README.md)中的指南为准。

本文记录 DeepSeek Harness 的第三方 MCP 记忆接入调研。Go Harness 当前实施方向见 [Hermes 式内置记忆方案](hermes-agent-memory-design.zh-CN.md)。

## 上游实现

以 Go Harness 当前的会话兼容基线 `d743267388641bc76f17c45ce8b4c231aed1d32c` 为准，并核对了上游 `master` 的记忆指南。DeepSeek Harness 默认没有启用长期记忆服务。官方提供三份可选配置，都通过通用 MCP client 注册外部工具。会话日志和 compaction 继续负责同一会话的上下文，跨会话记忆由选中的服务保存和查询。 [固定基线的记忆指南](https://github.com/deepseek-ai/deepseek-harness/blob/d743267388641bc76f17c45ce8b4c231aed1d32c/docs/user/guide/mcp-memory.zh.md) · [上游当前指南](https://github.com/deepseek-ai/deepseek-harness/blob/master/docs/user/guide/mcp-memory.md)

| 上游示例 | 接入方式 | 数据与范围 |
| --- | --- | --- |
| MCP Reference Memory `2026.7.4` | stdio，`mcp-server-memory`，工具前缀 `mcp__reference_memory__` | JSONL 知识图谱，包含实体、关系和观察。由 `MEMORY_FILE_PATH` 决定文件，按实体名称、类型和观察做字符串搜索。 |
| Engram `v1.20.0` | stdio，`engram mcp`，工具前缀 `mcp__engram__` | 由 Engram 管理存储和项目。支持 `ENGRAM_DATA_DIR` 和 `ENGRAM_PROJECT`，固定版本的存储实现启用 SQLite WAL。 |
| Memorix `1.3.0` | stdio，`memorix serve`，工具前缀 `mcp__memorix__` | 由 Memorix 管理存储与 Git 项目标识，支持 `MEMORIX_DATA_DIR`，可在无 embedding 的本地模式运行。 |

来源为 [官方配置样例](https://github.com/deepseek-ai/deepseek-harness/tree/d743267388641bc76f17c45ce8b4c231aed1d32c/apps/cli/config/examples/mcp-memory)、[Reference Memory 源码](https://github.com/modelcontextprotocol/servers/blob/6dd0a683e198783e30feabf7abaf42f925bd18b1/src/memory/index.ts) 和 [Engram 存储源码](https://github.com/Gentleman-Programming/engram/blob/ba9e46ced152c37a7cb9e576153c41995873e2fc/internal/store/store.go)。

## Go Harness 的实现

**默认关闭，通过部署配置选择一个记忆 MCP server。** 首个验证后端选官方示例里的 **MCP Reference Memory**。它直接验证写入、检索和删除，JSONL 可以放进现有 Host 存储。三个官方示例作为可选部署样例交付，各服务由部署者预先安装或通过自定义 Host 镜像提供。启用后端无需新建一套 Go 记忆 API。 [上游配置与启用流程](https://github.com/deepseek-ai/deepseek-harness/blob/d743267388641bc76f17c45ce8b4c231aed1d32c/docs/user/guide/mcp-memory.zh.md)

| 组件 | 职责 |
| --- | --- |
| Controller | 读取可选部署配置，按现有实例启动链路给 Docker 和 ECI Host 传递配置。配置只接受管理员提供的 server 条目，凭证通过显式环境变量引用。 |
| Host | 每个账号 Host 持有一条连接。stdio 由 Host 启停预装的程序，Streamable HTTP 连接已运行的服务。按 upstream 规则清除继承环境中的凭证型变量与 `DSH_*`，再加入该条目显式传入的变量。 |
| Go MCP 适配 | 通过官方 Go SDK 处理协议、发现和调用。第三方 SDK 接口封装放 `thirdparty/mcp/`，Host 连接生命周期由 Host 负责。工具名沿用 `mcp__<serverName>__<tool>`，需要归一化或截断时沿用上游的稳定后缀规则。 |
| AgentLoop | 通过现有工具调用链暴露 server 的工具及 schema。模型按需要调用写入、查询或删除。工具调用和返回仍可在 v4 会话日志及前端工具卡查看，实际记忆以 provider 的存储为准。 |
| MCP server | 定义工具、数据格式、搜索方式、作用域和持久化。远程部署的身份和数据隔离由该 server 的连接配置负责。 |

对应接口见 [Host 组装](../internal/host/host.go)、[工具组装](../harness/assembly/native_runtime.go)、[工具契约](../harness/agent/native_contract.go)、[上游 MCP client](https://github.com/deepseek-ai/deepseek-harness/blob/d743267388641bc76f17c45ce8b4c231aed1d32c/packages/mcp/mcp-client/README.md) 和 [官方 Go SDK](https://github.com/modelcontextprotocol/go-sdk)。

启用配置使用现有部署 YAML 的可选段。缺省时没有记忆工具。stdio 使用 `server_name`、`transport`、`command`、`args`、`cwd`、`env`，HTTP 使用 `server_name`、`transport`、`url`、`headers`。stdio 程序以参数数组启动，HTTP 的认证头从指定凭证变量解析。Controller 的启动检查校验条目形状和凭证引用，连接是否就绪由 Host 检查。第一版在 Host 启动时加载配置，通过重建 Host 更换配置。工具表在连接变化时仍需更新。热改配置和 MCP resources 属于上游通用客户端的额外能力，可分别对齐。

Reference Memory 的拟议配置如下。Host 在启动子进程前创建数据文件的父目录，路径内置在该示例中。

```yaml
mcp:
  server_name: reference_memory
  transport: stdio
  command: mcp-server-memory
  cwd: /root/harness
  env:
    MEMORY_FILE_PATH: /root/harness/.dsh/memory/reference-memory.jsonl
```

## Docker、ECI 与账号范围

Reference Memory 示例把 `MEMORY_FILE_PATH` 指向 `/root/harness/.dsh/memory/reference-memory.jsonl`。这是 Go Harness 的部署适配。上游示例默认写在 `$HOME`，Go Harness 的持久挂载点是 `/root/harness`。Controller 为每个账号分配自己的卷或 NAS 子目录，因此同账号各会话能访问同一记忆库。stdio 程序从稳定的账号级目录启动，使用项目识别的后端由其自身配置决定范围，避免让每个会话 UUID 目录意外成为新的记忆项目。远程 HTTP 的账号连接必须有独立命名空间或凭证，由部署侧配置并验证。 [上游默认路径与作用域](https://github.com/deepseek-ai/deepseek-harness/blob/d743267388641bc76f17c45ce8b4c231aed1d32c/docs/user/guide/mcp-memory.zh.md) · [Go Harness 存储](deployment.md#storage-choices)

| 部署 | 存储行为 |
| --- | --- |
| Docker | 同账号新旧容器挂同一个命名卷，重建后使用同一份记忆。 |
| ECI + NAS | 同账号的新实例挂回原 NAS 子目录，文件型 provider 沿用原文件。Reference Memory 需在真实 NAS 上验证写入、重启和重建。 |
| ECI 容器层 | 本地 provider 的记忆随实例释放而丢失。需要跨实例保留时使用 NAS 支持的后端或外部 Streamable HTTP 服务。 |

Reference Memory 的固定版本通过读改写保存整个 JSONL 文件。Go Harness 对这组工具在同一个 Host 级连接上串行调度，避免不同会话同时写入；文件的崩溃一致性仍取决于后端。Engram 示例放在 Docker 本地卷，ECI 上可连接存储位于服务自身本地磁盘的外部实例。SQLite WAL 直接写 NAS 需要 provider 提供明确支持并通过实测。 [Reference Memory 保存实现](https://github.com/modelcontextprotocol/servers/blob/6dd0a683e198783e30feabf7abaf42f925bd18b1/src/memory/index.ts) · [SQLite WAL 文件系统约束](https://www.sqlite.org/wal.html)

## 工具发现、恢复与日志

1. Host 连接后发现工具，完成分页读取、名称冲突检查和 schema 编译，再原子发布一份工具表。已有会话在下一轮采用新表。一轮内固定 schema，发送给模型的定义、参数校验、工具执行和 v4 请求记录使用同一份快照。当前 Go Harness 在创建 AgentLoop 时固定工具表，这一步是对齐上游重连行为所需的代码改动。 [当前工具表初始化](../harness/agent/native_loop.go) · [上游工具注册](https://github.com/deepseek-ai/deepseek-harness/blob/d743267388641bc76f17c45ce8b4c231aed1d32c/packages/mcp/mcp-client/src/tools.ts)
2. 掉线期间保留上次工具定义，调用返回暂不可用。按上游默认值从 500 ms 退避到最多 30 秒，连续 10 次失败后撤下工具，待重启后再连接。恢复时重新发现工具。stdio 确认旧进程退出后再启动下一进程。遇到写入超时，核对 provider 的实际结果后再决定是否重试。 [上游连接和恢复](https://github.com/deepseek-ai/deepseek-harness/blob/d743267388641bc76f17c45ce8b4c231aed1d32c/packages/mcp/mcp-client/src/connection.ts)
3. 初始兼容范围以三个记忆示例的工具调用为准，支持文本和结构化结果。后端工具描述不足时，可在现有 `AGENTS.md` 加一条指导，让 Agent 在用户明确要求记住时写入，在历史信息相关时查询。删除 provider 中的记忆影响后续查询，已经写入会话日志的历史工具结果按会话历史管理。 [上游模型指令与验证流程](https://github.com/deepseek-ai/deepseek-harness/blob/d743267388641bc76f17c45ce8b4c231aed1d32c/docs/user/guide/mcp-memory.zh.md)

## 实施与验收

1. 先实现 Host 内的 stdio MCP 接入和 Reference Memory 示例，保留默认关闭。用测试 MCP server 覆盖发现、参数校验、凭证隔离和连接关闭。
2. 对齐工具表更新、断线恢复和 Streamable HTTP。验证正在聊天的会话在下一轮发现工具变更，掉线期间能继续用其他工具，恢复后能继续查询。
3. 对每个官方样例执行同一套功能测试。会话 A 写入唯一偏好，会话 B 不带 A 的历史，通过工具查询并应用该偏好。再从 provider 删除该记忆，用新会话确认查询结果，另用不同账号确认隔离。
4. 用 Docker 新容器复用原卷，再用真实 ECI + NAS 重建 Host，分别验证读写和恢复。ECI 容器层测试实例重建后的数据行为。验收以真实工具调用和后端状态为准，连接成功只作为前置检查。

方案依据官方指南和源码核对，服务接入、实际模型调用和 NAS 重建属于实施阶段的验收项。
