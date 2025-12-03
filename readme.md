# RSS 聚合与处理工具

一个用于聚合、抓取和处理 RSS 源内容的 Go 语言工具，支持多步骤异步处理，包括文章元数据提取、正文清洗和 AI 摘要生成。

## 功能特性

- **多步骤处理流程**：支持按步骤执行 OPML 导入、RSS 解析、正文抓取和 AI 摘要生成
- **并发控制**：可配置并发数，避免资源过载
- **代理支持**：支持代理池轮询，提高可用性
- **数据持久化**：MySQL 存储，结构清晰
- **内容清洗**：HTML 转 Markdown，净化内容
- **AI 集成**：支持 OpenAI 兼容 API 进行内容摘要

## 快速开始

### 1. 配置准备

创建以下配置文件：

**`proxys.json`**（可选，代理配置）
```json
{
  "proxies": ["http://proxy1:port", "http://proxy2:port"]
}
```

**`db.json`**（数据库配置）
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
go get -u github.com/go-sql-driver/mysql
go get -u github.com/mmcdole/gofeed
go get -u github.com/JohannesKaufmann/html-to-markdown
```

### 3. 运行程序

```bash
# 完整流程（包含所有步骤）
go run main.go -url "https://example.com/opml.xml" -c 5

# 仅执行特定步骤
go run main.go -step 1 -url "https://example.com/opml.xml"           # 仅导入OPML
go run main.go -step 2 -c 10                                         # 仅解析RSS
go run main.go -step 3 -c 5                                          # 仅抓取正文
go run main.go -step 4 -c 2                                          # 仅生成AI摘要

# 帮助信息
go run main.go -h
```

## 参数说明

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `-url` | OPML 文件的 URL 地址（步骤 0,1 必需） | "" |
| `-c` | 并发控制数 | 1 |
| `-step` | 运行步骤：0=全部，1=获取OPML，2=解析RSS，3=抓取正文，4=LLM摘要 | 0 |

## 数据库结构

程序会自动创建以下表：

- **`rss`**：存储 RSS 源信息
- **`article`**：存储文章元数据
- **`content_store`**：存储清洗后的正文内容（Markdown）
- **`llm_store`**：存储 AI 生成的摘要

## 处理流程

1. **OPML 导入**：从指定 URL 导入 RSS 订阅列表
2. **RSS 解析**：并发获取所有 RSS 源，提取文章元数据
3. **正文抓取**：抓取文章正文并转换为 Markdown 格式
4. **AI 摘要**：调用 AI 接口生成文章摘要

## 注意事项

1. 首次运行需要创建数据库和表，程序会自动初始化
2. 建议逐步执行，先测试少量 RSS 源
3. 使用代理时请确保代理服务器可用
4. AI 摘要步骤需要正确配置 API 密钥和服务地址
5. 程序包含指数退避重试机制，网络不稳定时可自动重试

## 许可证

MIT License