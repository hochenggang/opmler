# Opmler

一个基于 OPML 的 RSS 文章抓取、内容提取与智能摘要工具。

## 功能特性

- 📥 解析 OPML 文件，自动抓取 RSS 订阅源
- 💾 使用 SQLite 存储元数据、文章内容和 LLM 摘要
- 🔄 网络请求支持指数退避重试（最多3次）
- 🌐 支持代理池随机选择
- 🤖 集成 LLM API 自动生成文章摘要
- 📝 智能提取网页正文并转换为 Markdown

## 快速开始

### 1. 编译项目

```bash
# 克隆项目
git clone https://github.com/hochenggang/opmler.git
cd opmler

# 编译
go build -o opmler main.go
```

### 2. 配置 LLM API

在程序所在目录创建 `llm.json`：

```json
{
  "server": "https://api.siliconflow.cn/v1/chat/completions",
  "api_keys": ["Bearer sk-your-api-key-here"],
  "model": "THUDM/glm-4-9b-chat",
  "prompt": "你是一个聪明的人工智能助手..."
}
```

### 3. 配置代理（可选）

在程序所在目录创建 `proxys.json`：

```json
{
  "proxies": ["http://proxy1:port", "http://proxy2:port"]
}
```

### 4. 运行程序

```bash
./opmler -url "https://example.com/feeds.opml" -db "my_rss"
```

## 参数说明

- `-url`: OPML 文件的 URL 地址（必需）
- `-db`: 数据库文件名前缀（默认：default）

## 输出数据库

程序会生成三个 SQLite 数据库文件：

### `{db_name}_meta.db`
- `rss` 表：RSS 源信息
- `article` 表：文章元数据

### `{db_name}_content.db`
- `content_store` 表：文章的 Markdown 格式内容

### `{db_name}_llm.db`
- `llm_store` 表：文章的 AI 摘要

## 执行流程

1. **解析 OPML**：下载并解析 OPML 文件，提取所有 RSS 源
2. **抓取 RSS**：遍历所有 RSS 源，获取文章列表
3. **提取内容**：下载每篇文章，提取正文转为 Markdown
4. **生成摘要**：调用 LLM API 为文章生成智能摘要
5. **数据存储**：将所有数据分别存入三个数据库

## 错误处理

- 网络请求失败时自动重试（指数退避：1s、2s、4s）
- 优先使用代理配置，若无则直连
- 数据库操作异常时会记录日志并继续执行

## 依赖库

```go
github.com/JohannesKaufmann/html-to-markdown
github.com/mattn/go-sqlite3
github.com/mmcdole/gofeed
```

## 注意事项

1. LLM 配置是必需的，否则摘要功能无法使用
2. 代理配置是可选的，不配置则使用直连
3. 程序会避免重复处理已存在的文章
4. 内容过长时会自动截断以适配 LLM 限制

## 许可证

MIT License