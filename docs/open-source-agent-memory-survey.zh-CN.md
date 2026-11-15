# 开源 Agent 记忆系统调研

> 历史设计与调研记录。当前行为、配置和命令以[文档索引](README.md)中的指南为准。

调研日期为 2026-11-13。星数来自当日 GitHub API，取近似值。OpenClaw 和 Hermes 的星数覆盖整个 Agent 项目。本次以官方仓库、实现和设计文档核对写入、查询及部署方式，结论属于设计评估，尚未对候选服务做同条件实测。

## 适合 Go Harness 的结论

Go Harness 的实施方向是由 Go Host 内置 Hermes 式记忆。下面的项目对比用于调研。 [当前记忆方案](hermes-agent-memory-design.zh-CN.md)

## Agent 内置机制与轻量插件

| 项目 | GitHub 星数 | 写入、读取与更新 | 对 Go Harness 的参考 |
| --- | ---: | --- | --- |
| [OpenClaw](https://github.com/openclaw/openclaw) | 394.4k | 小型核心文件在会话开始时按资格加载。每日笔记和历史会话按需召回。压缩前可写入未保留的事实，后台整理可合并旧条目。索引记录来源、时间和替换关系，支持关键词、向量及混合检索。 | 优先借鉴来源分级、更新关系、索引可重建，以及避免把已召回的内容重复写回。当前已有 v4 日志，先用它承载历史证据。 [架构](https://github.com/openclaw/openclaw/blob/main/docs/concepts/memory-architecture.md) · [索引配置](https://github.com/openclaw/openclaw/blob/main/docs/reference/memory-config.md) |
| [Hermes Agent](https://github.com/NousResearch/hermes-agent) | 263.0k | 模型用 `memory` 增、改、删两份有容量上限的 Markdown。新会话自动加载快照。用 SQLite FTS5 的 `session_search` 找旧对话。也支持后台复盘和可选审批。 | 最适合作为内置记忆基线。用少量偏好形成可立即感知的跨会话体验，把长历史留给按需查询。 [固定提交的实现](https://github.com/NousResearch/hermes-agent/blob/415fa335b4d98ecf00e29e88625bb478dc63fb67/tools/memory_tool_store.py) · [会话搜索](https://hermes-agent.nousresearch.com/docs/user-guide/sessions/#session-search-tool) |
| [claude-mem](https://github.com/thedotmack/claude-mem) | 116.6k | 通过生命周期 hook 收集活动，由 worker 生成可检索的观察和摘要。SQLite FTS5，另有可选 Chroma。检索先找候选，再看时间线和完整记录。 | 借鉴结果先返回标题、命中片段和锚点，Agent 需要时再展开原文。我们已经记录工具生命周期，可以用已提交事件作为输入。 [架构](https://github.com/thedotmack/claude-mem/blob/main/docs/public/architecture/overview.mdx) · [检索](https://github.com/thedotmack/claude-mem/blob/main/docs/public/architecture/search-architecture.mdx) |
| [MemPalace](https://github.com/MemPalace/mempalace) | 60.5k | 保留原文，按人、项目和主题限制检索范围，通过本地 embedding 召回。默认 ChromaDB，后端可替换。 | 适合做历史语义搜索对照。复用 v4 原文并保留消息锚点，比较本地向量对中文同义表达的实际增益。 [仓库架构说明](https://github.com/MemPalace/mempalace#what-it-is) |
| [Engram](https://github.com/Gentleman-Programming/engram) | 8.1k | 单 Go 二进制加 SQLite FTS5，提供 MCP 和 HTTP。保存结构化观察，用稳定的 `topic_key` 更新同一主题，查询先返回预览，再读完整记录。 | 可做 Docker 的轻量 MCP 对照，也可借鉴主题更新和渐进读取。项目默认范围与当前工作目录有关，Go Harness 要显式验证同账号跨会话。其 SQLite 主库的 ECI + NAS 部署另行验证，远程服务可使用自己的本地盘。 [核心](https://github.com/Gentleman-Programming/engram/blob/main/docs/codebase/memory-core.md) · [SQLite 文件系统说明](https://www.sqlite.org/wal.html) |

## 可独立接入的记忆系统

| 项目 | GitHub 星数 | 机制与接入成本 | 对 Go Harness 的判断 |
| --- | ---: | --- | --- |
| [Mem0](https://github.com/mem0ai/mem0) | 68.9k | 自托管 REST 提供增、查、改、删。新版 OSS 提取采用一次模型调用产生新增候选，检索结合语义、BM25 与实体信号，具体取决于配置。显式更新和删除由 API 提供。 | 独立记忆服务的调研样本。 [OSS 迁移说明](https://github.com/mem0ai/mem0/blob/main/docs/migration/oss-v2-to-v3.mdx) · [OSS REST](https://github.com/mem0ai/mem0/blob/main/docs/open-source/features/rest-api.mdx) |
| [Hindsight](https://github.com/vectorize-io/hindsight) | 60.7k | `retain` 提取事实并维护文档和实体关系。 `recall` 综合语义、关键词、图和时间信号。 `reflect` 在记忆上做进一步推理。提供自托管方案，常用部署使用 PostgreSQL 和模型服务。 | 第二个外部对照。适合验证跨多个会话、时间相关的问题。借鉴把检索结果和模型综合答案分开，以及用稳定文档 ID 保持重试幂等。 [写入](https://github.com/vectorize-io/hindsight/blob/main/hindsight-docs/docs/developer/retain.md) · [召回](https://hindsight.vectorize.io/developer/api/recall) |
| [OpenViking](https://github.com/volcengine/OpenViking) | 42.1k | 将记忆、知识与 Skills 放在统一上下文目录，按摘要、概览、详情分层加载。 | 适合未来同时检索文档、Skills 和历史时评估。当前可先借鉴分层加载，不必统一这些数据的存储。仓库标注 AGPL-3.0，实际引入前按集成方式核对许可。 [仓库与分层设计](https://github.com/volcengine/OpenViking/) |
| [Graphiti](https://github.com/getzep/graphiti) | 32.8k | 以事件增量更新时序知识图谱，记录事实有效期和观察时间，处理新事实让旧关系失效。可通过 MCP 或参考 REST 服务接入，部署需匹配图存储和模型。 | 值得借鉴旧事实失效和来源保留。只有需要频繁查询人物、项目关系或「当时是什么状态」时，再验证整套后端。 [仓库架构与部署](https://github.com/getzep/graphiti) |
| [memU](https://github.com/NevaMind-AI/memU) | 14.6k | workspace 模式使用有追踪信息的文件和条目，查询可一次向量检索并对文件、条目、来源排序。 | 可参考从归纳后的条目追溯原始文件。选型时按目标版本阅读 ADR 和实际许可文件。 [Workspace ADR](https://github.com/NevaMind-AI/memU/blob/main/docs/adr/0006-from-memory-item-category-to-tracked-workspace-memorization.md) |
| [MemOS](https://github.com/MemTensor/MemOS) | 12.4k | DeepSeek Harness 本地适配器是独立插件。它把有上限的召回结果作为带来源的上下文注入，提供读取详情工具。云插件另有首模型步骤前召回、成功轮次结束后写入的流程。 | 与现有 DeepSeek 风格事件链最贴近。重点参考触发时机、上下文标记和故障降级。若引入其服务，按 Go Host 的边界重新适配。 [本地适配器](https://github.com/MemTensor/MemOS/blob/main/apps/memos-local-plugin/adapters/deepseek-harness/README.md) · [云插件](https://github.com/MemTensor/MemOS-Cloud-Dsh-Plugin) |

另核对了 Letta。原 `letta-ai/letta` 仓库约 25.4k 星，当前 Agent 实现在约 3.8k 星的 Letta Code。可参考它把提示词里的记忆块映射到可编辑、可版本化的 MemFS。它的运行时与 Go Harness 有较多职责重叠，参考设计的收益高于替换现有 AgentLoop。 [仓库迁移说明](https://github.com/letta-ai/letta/blob/main/AGENTS.md) · [Letta Code 的记忆块](https://github.com/letta-ai/letta-code/blob/main/src/agent/prompts/letta.md)
