# RSS 聚合与处理工具

一个用于聚合、抓取和处理 RSS 源内容的 Go 语言工具，采用单线程顺序处理，包括文章元数据提取、正文清洗和 AI 摘要生成。

## 功能特性

- **链式步骤执行**：指定起始步骤，自动执行后续所有步骤
- **错误容错**：单点错误不影响全局，支持多次重试
- **代理支持**：支持代理池轮询，提高可用性
- **数据持久化**：MySQL 存储，结构清晰
- **内容清洗**：HTML 转 Markdown，净化内容
- **AI 集成**：支持 OpenAI 兼容 API 进行内容摘要
- **路径无关**：配置文件基于可执行文件位置查找，支持 CRON 运行

## 快速开始

### 1. 配置准备

在可执行文件同级目录创建以下配置文件：

**`db.json`**（必需，数据库配置）
```json
{
  "mysql": {
    "host": "localhost",
    "port": 3306,
    "user": "root",
    "password": "your_password",
    "database": "rss_collector",
    "charset": "utf8mb4"
  }
}
```

**`proxys.json`**（可选，代理配置）
```json
{
  "proxies": ["http://proxy1:port", "http://proxy2:port"]
}
```

**`llm.json`**（可选，AI 配置）
```json
{
  "server": "https://api.openai.com/v1/chat/completions",
  "api_keys": ["Bearer sk-xxx"],
  "model": "gpt-3.5-turbo",
  "prompt": "请为以下文章生成摘要："
}
```

### 2. 安装依赖

```bash
go mod init rss-collector
go mod tidy
```

### 3. 编译运行

```bash
# 编译
go build -o opmler main.go

# 从步骤1开始执行全部流程
./opmler -url "https://example.com/opml.xml"

# 从步骤2开始执行（跳过OPML导入）
./opmler -step=2

# 仅执行步骤4（LLM摘要）
./opmler -step=4

# 帮助信息
./opmler -h
```

## 参数说明

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `-url` | OPML 文件的 URL 地址（步骤1必需） | "" |
| `-step` | 起始步骤：1=OPML, 2=RSS, 3=正文, 4=LLM | 1 |

## 步骤执行逻辑

| 参数 | 执行步骤 |
|------|----------|
| `-step=1` | 执行 1 → 2 → 3 → 4 |
| `-step=2` | 执行 2 → 3 → 4 |
| `-step=3` | 执行 3 → 4 |
| `-step=4` | 仅执行 4 |

## 数据库结构

程序会自动创建以下表：

| 表名 | 说明 |
|------|------|
| `rss` | 存储 RSS 源信息 |
| `article` | 存储文章元数据 |
| `content_store` | 存储清洗后的正文内容（Markdown） |
| `llm_store` | 存储 AI 生成的摘要 |
| `keyword_store` | 存储关键词索引 |

## 处理流程

1. **步骤1 - OPML 导入**：从指定 URL 导入 RSS 订阅列表
2. **步骤2 - RSS 解析**：获取所有 RSS 源，提取文章元数据
3. **步骤3 - 正文抓取**：抓取文章正文并转换为 Markdown 格式
4. **步骤4 - AI 摘要**：调用 AI 接口生成文章摘要和关键词

## 错误处理

- **重试机制**：HTTP 请求、数据库连接、LLM 调用均支持 3 次重试，指数退避
- **单点容错**：单个 RSS 源或文章处理失败不影响整体流程
- **Panic 恢复**：Markdown 转换等可能 panic 的操作已做恢复处理
- **统计输出**：每步完成后输出成功/跳过/失败统计

## CRON 定时任务

配置文件基于可执行文件位置查找，适合 CRON 运行：

```cron
# 每小时执行一次（从步骤2开始，跳过OPML导入）
0 * * * * cd /path/to/app && /path/to/app/opmler -step=2 >> /path/to/app/cron.log 2>&1
```

## 日志

日志同时输出到控制台和 `log.txt` 文件（位于可执行文件同级目录）。

## 注意事项

1. 首次运行需要创建数据库，程序会自动初始化表结构
2. 步骤1需要提供 `-url` 参数
3. 使用代理时请确保代理服务器可用
4. AI 摘要步骤需要正确配置 API 密钥和服务地址
5. 已处理的文章会自动跳过，支持增量执行

## 许可证

MIT License
