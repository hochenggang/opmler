package main

import (
	"bytes"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	md "github.com/JohannesKaufmann/html-to-markdown"
	_ "github.com/mattn/go-sqlite3"
	"github.com/mmcdole/gofeed"
)

// --- 配置结构体 ---

type ProxyConfig struct {
	Proxies []string `json:"proxies"`
}

type LLMConfig struct {
	Server  string   `json:"server"`
	ApiKeys []string `json:"api_keys"`
	Model   string   `json:"model"`
	Prompt  string   `json:"prompt"`
}

// --- 数据模型 ---

type OPML struct {
	XMLName xml.Name `xml:"opml"`
	Body    struct {
		Outlines []struct {
			Text   string `xml:"text,attr"`
			Title  string `xml:"title,attr"`
			Type   string `xml:"type,attr"`
			XMLUrl string `xml:"xmlUrl,attr"`
			HTMLUrl string `xml:"htmlUrl,attr"`
		} `xml:"outline"`
	} `xml:"body"`
}

type RSSMeta struct {
	Host     string
	Title    string
	URL      string
	UpdateAt time.Time
}

type ArticleMeta struct {
	Key       string
	Link      string
	Title     string
	Published int64
	Author    string
}

// --- 全局变量与工具 ---

var (
	httpClient *http.Client
	proxies    []string
	llmConf    *LLMConfig
)

// 初始化网络客户端
func initNetwork() {
	// 1. 读取代理配置
	file, err := os.ReadFile("./proxys.json")
	if err == nil {
		var pc ProxyConfig
		if json.Unmarshal(file, &pc) == nil && len(pc.Proxies) > 0 {
			proxies = pc.Proxies
			log.Printf("成功加载 %d 个代理服务器", len(proxies))
		}
	}

	if len(proxies) == 0 {
		log.Println("未找到有效代理配置，使用直连模式")
	}

	// 基础 Client，具体的 Transport 在请求时动态决定（如果需要随机代理）
	// 这里为了简单，我们自定义 doRequest 函数来处理代理逻辑
	httpClient = &http.Client{
		Timeout: 30 * time.Second,
	}
}

// 指数退避重试请求
func doRequest(method, reqUrl string, body io.Reader, headers map[string]string) ([]byte, error) {
	var lastErr error

	for i := 0; i < 3; i++ {
		// 1. 随机选择代理 (如果存在)
		var transport *http.Transport
		if len(proxies) > 0 {
			proxyUrlStr := proxies[rand.Intn(len(proxies))]
			proxyUrl, err := url.Parse(proxyUrlStr)
			if err == nil {
				transport = &http.Transport{
					Proxy: http.ProxyURL(proxyUrl),
				}
			}
		}

		client := &http.Client{
			Timeout:   60 * time.Second,
			Transport: transport, // 如果 nil 则使用默认
		}

		req, err := http.NewRequest(method, reqUrl, body)
		if err != nil {
			return nil, err
		}

		// 设置默认 User-Agent
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; RSSCollector/1.0)")
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return io.ReadAll(resp.Body)
			}
			err = fmt.Errorf("HTTP status %d", resp.StatusCode)
		}

		lastErr = err
		// 指数退避: 1s, 2s, 4s
		sleepDuration := time.Duration(math.Pow(2, float64(i))) * time.Second
		log.Printf("请求失败 [%s]: %v. %v 后重试...", reqUrl, err, sleepDuration)
		time.Sleep(sleepDuration)
	}

	return nil, fmt.Errorf("request failed after 3 retries: %v", lastErr)
}

// 辅助：生成大写 MD5
func md5Upper(s string) string {
	hash := md5.Sum([]byte(s))
	return strings.ToUpper(hex.EncodeToString(hash[:]))
}

// --- 数据库管理 ---

type DBManager struct {
	MetaDB    *sql.DB
	ContentDB *sql.DB
	LLMDB     *sql.DB
}

func initDB(prefix string) (*DBManager, error) {
	mgr := &DBManager{}
	var err error

	// 1. Meta DB
	mgr.MetaDB, err = sql.Open("sqlite3", prefix+"_meta.db")
	if err != nil {
		return nil, err
	}
	// 创建 RSS 表
	_, err = mgr.MetaDB.Exec(`CREATE TABLE IF NOT EXISTS rss (
		rss_host TEXT PRIMARY KEY,
		rss_title TEXT,
		rss_url TEXT,
		update_at DATETIME
	)`)
	if err != nil { return nil, err }
	// 创建 Article 表
	_, err = mgr.MetaDB.Exec(`CREATE TABLE IF NOT EXISTS article (
		article_key TEXT PRIMARY KEY,
		article_link TEXT,
		article_title TEXT,
		article_published INTEGER,
		article_author TEXT
	)`)
	if err != nil { return nil, err }

	// 2. Content DB
	mgr.ContentDB, err = sql.Open("sqlite3", prefix+"_content.db")
	if err != nil { return nil, err }
	_, err = mgr.ContentDB.Exec(`CREATE TABLE IF NOT EXISTS content_store (
		key TEXT PRIMARY KEY,
		value TEXT
	)`)
	if err != nil { return nil, err }

	// 3. LLM DB
	mgr.LLMDB, err = sql.Open("sqlite3", prefix+"_llm.db")
	if err != nil { return nil, err }
	_, err = mgr.LLMDB.Exec(`CREATE TABLE IF NOT EXISTS llm_store (
		key TEXT PRIMARY KEY,
		value TEXT
	)`)
	if err != nil { return nil, err }

	return mgr, nil
}

func (mgr *DBManager) Close() {
	mgr.MetaDB.Close()
	mgr.ContentDB.Close()
	mgr.LLMDB.Close()
}

// --- 核心业务逻辑 ---

// 第一步 & 第三步.1: 获取 OPML 并入库
func processOPML(opmlUrl string, mgr *DBManager) error {
	log.Println("正在获取 OPML...", opmlUrl)
	data, err := doRequest("GET", opmlUrl, nil, nil)
	if err != nil {
		return err
	}

	var opml OPML
	if err := xml.Unmarshal(data, &opml); err != nil {
		return err
	}

	tx, _ := mgr.MetaDB.Begin()
	stmt, _ := tx.Prepare("INSERT OR REPLACE INTO rss (rss_host, rss_title, rss_url, update_at) VALUES (?, ?, ?, ?)")
	defer stmt.Close()

	for _, outline := range opml.Body.Outlines {
		if outline.XMLUrl == "" {
			continue
		}
		u, err := url.Parse(outline.XMLUrl)
		host := ""
		if err == nil {
			host = u.Host
		}
		
		// 默认标题处理
		title := outline.Title
		if title == "" { title = outline.Text }

		log.Printf("入库 RSS: %s (%s)", title, host)
		_, err = stmt.Exec(host, title, outline.XMLUrl, time.Now())
		if err != nil {
			log.Printf("RSS入库失败: %v", err)
		}
	}
	return tx.Commit()
}

// 第三步.2: 遍历 RSS 并采集文章元数据
func processFeeds(mgr *DBManager) {
	rows, err := mgr.MetaDB.Query("SELECT rss_host, rss_url FROM rss")
	if err != nil {
		log.Println("无法查询 RSS 表:", err)
		return
	}
	defer rows.Close()

	// 预编译插入语句
	tx, _ := mgr.MetaDB.Begin()
	stmt, _ := tx.Prepare(`INSERT OR IGNORE INTO article (article_key, article_link, article_title, article_published, article_author) VALUES (?, ?, ?, ?, ?)`)
	defer stmt.Close()

	parser := gofeed.NewParser()
	// 配置 gofeed 使用我们自定义的 Client (主要是为了复用我们的逻辑，但 gofeed 只需要 http.Client)
	// 由于 gofeed 内部重试机制有限，我们这里手动 fetch 内容再 feed 给 parser 解析更稳妥
	// 或者直接让 gofeed 使用默认 client，但这样没有代理。
	// 最佳方案：手动 fetch XML，传给 parser.ParseString

	type rssItem struct {
		Host string
		URL  string
	}
	var feeds []rssItem
	for rows.Next() {
		var r rssItem
		rows.Scan(&r.Host, &r.URL)
		feeds = append(feeds, r)
	}
	rows.Close()

	for _, f := range feeds {
		log.Printf("正在解析 RSS: %s", f.URL)
		xmlData, err := doRequest("GET", f.URL, nil, nil)
		if err != nil {
			log.Printf("获取 RSS 失败 %s: %v", f.URL, err)
			continue
		}

		feed, err := parser.ParseString(string(xmlData))
		if err != nil {
			log.Printf("解析 XML 失败 %s: %v", f.URL, err)
			continue
		}

		for _, item := range feed.Items {
			link := item.Link
			// 补全链接
			if link == "" {
				continue
			}
			u, err := url.Parse(link)
			if err == nil {
				if u.Scheme == "" || u.Host == "" {
					// 尝试补全
					if !strings.HasPrefix(link, "http") {
						scheme := "https" // 默认
						if strings.HasPrefix(f.URL, "http:") { scheme = "http" }
						link = fmt.Sprintf("%s://%s%s", scheme, f.Host, link)
					}
				}
			}

			key := md5Upper(link)
			
			// 处理时间
			var pubTime int64
			if item.PublishedParsed != nil {
				pubTime = item.PublishedParsed.Unix()
			} else if item.UpdatedParsed != nil {
				pubTime = item.UpdatedParsed.Unix()
			} else {
				pubTime = time.Now().Unix()
			}

			author := ""
			if item.Author != nil {
				author = item.Author.Name
			} else if len(item.Authors) > 0 {
				author = item.Authors[0].Name
			}

			_, err = stmt.Exec(key, link, item.Title, pubTime, author)
			if err != nil {
				log.Printf("文章元数据插入错误: %v", err)
			}
		}
	}
	tx.Commit()
}

// 第三步.3: 获取正文 -> 转 Markdown -> LLM 摘要
func processContentAndLLM(mgr *DBManager) {
	// 加载 LLM 配置
	llmData, err := os.ReadFile("./llm.json")
	if err == nil {
		llmConf = &LLMConfig{}
		json.Unmarshal(llmData, llmConf)
	}

	rows, err := mgr.MetaDB.Query("SELECT article_key, article_link, article_title FROM article")
	if err != nil {
		log.Println(err)
		return
	}
	defer rows.Close()

	// 准备 Markdown 转换器
	converter := md.NewConverter("", true, nil)
	
	// 查询是否已经存在内容，避免重复抓取
	checkStmt, _ := mgr.ContentDB.Prepare("SELECT 1 FROM content_store WHERE key = ?")
	
	type task struct {
		Key string
		Link string
		Title string
	}
	var tasks []task
	for rows.Next() {
		var t task
		rows.Scan(&t.Key, &t.Link, &t.Title)
		tasks = append(tasks, t)
	}
	rows.Close()

	for _, t := range tasks {
		// 1. 检查是否已处理 Content
		var exists int
		err := checkStmt.QueryRow(t.Key).Scan(&exists)
		if err == nil {
			// 内容已存在，跳过抓取，但检查 LLM
			goto LLM_CHECK
		}

		{
			log.Printf("正在抓取正文: %s", t.Title)
			htmlBytes, err := doRequest("GET", t.Link, nil, nil)
			if err != nil {
				log.Printf("抓取失败: %v", err)
				continue
			}

			// 转 Markdown
			markdown, err := converter.ConvertString(string(htmlBytes))
			if err != nil {
				log.Printf("Markdown 转换失败: %v", err)
				markdown = "Conversion Failed" // 占位
			}

			// 存入 Content DB
			_, err = mgr.ContentDB.Exec("INSERT OR REPLACE INTO content_store (key, value) VALUES (?, ?)", t.Key, markdown)
			if err != nil {
				log.Printf("Content 存储失败: %v", err)
			}
		}

	LLM_CHECK:
		// 2. 检查是否已处理 LLM
		if llmConf == nil { continue }
		
		err = mgr.LLMDB.QueryRow("SELECT 1 FROM llm_store WHERE key = ?", t.Key).Scan(&exists)
		if err == nil { continue } // 已有摘要

		// 获取刚才存入或已存在的 Markdown
		var content string
		err = mgr.ContentDB.QueryRow("SELECT value FROM content_store WHERE key = ?", t.Key).Scan(&content)
		if err != nil || len(content) < 50 {
			log.Println("内容太短或未找到，跳过 LLM")
			continue
		}

		// 调用 LLM
		log.Printf("正在生成摘要: %s", t.Title)
		summary, err := callLLM(content)
		if err != nil {
			log.Printf("LLM 调用失败: %v", err)
			continue
		}

		// 存入 LLM DB
		_, err = mgr.LLMDB.Exec("INSERT OR REPLACE INTO llm_store (key, value) VALUES (?, ?)", t.Key, summary)
		if err != nil {
			log.Printf("LLM 结果存储失败: %v", err)
		}
	}
}

// 调用 LLM API
func callLLM(articleContent string) (string, error) {
	// 截断内容以防止 token 溢出 (假设简单截断，实际应按 token 计算)
	if len(articleContent) > 12000 {
		articleContent = articleContent[:12000]
	}

	payload := map[string]interface{}{
		"model": llmConf.Model,
		"messages": []map[string]string{
			{"role": "system", "content": llmConf.Prompt},
			{"role": "user", "content": articleContent},
		},
		"stream": false,
	}
	
	jsonPayload, _ := json.Marshal(payload)
	
	// 轮询 Key
	apiKey := ""
	if len(llmConf.ApiKeys) > 0 {
		apiKey = llmConf.ApiKeys[rand.Intn(len(llmConf.ApiKeys))]
	}

	headers := map[string]string{
		"Content-Type": "application/json",
		"Authorization": apiKey, // 注意：如果 apiKey 没带 Bearer 前缀，需在此处处理。配置里已带。
	}

	respBytes, err := doRequest("POST", llmConf.Server, bytes.NewBuffer(jsonPayload), headers)
	if err != nil {
		return "", err
	}

	// 解析响应 (适配 OpenAI 格式)
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return "", err
	}

	if resp.Error.Message != "" {
		return "", fmt.Errorf("api error: %s", resp.Error.Message)
	}

	if len(resp.Choices) > 0 {
		return resp.Choices[0].Message.Content, nil
	}

	return "", fmt.Errorf("no response content")
}

// --- 主程序入口 ---

func main() {
	// 第一步：解析参数
	opmlUrl := flag.String("url", "", "OPML 文件的 URL 地址")
	dbName := flag.String("db", "default", "数据库文件名前缀")
	flag.Parse()

	if *opmlUrl == "" {
		fmt.Println("请提供 -url 参数")
		return
	}

	rand.Seed(time.Now().UnixNano())
	initNetwork()

	// 第二步：创建数据库
	log.Printf("初始化数据库: %s_*.db", *dbName)
	mgr, err := initDB(*dbName)
	if err != nil {
		log.Fatalf("数据库初始化失败: %v", err)
	}
	defer mgr.Close()

	// 第三步：执行
	// 1. 获取 OPML
	if err := processOPML(*opmlUrl, mgr); err != nil {
		log.Fatalf("处理 OPML 失败: %v", err)
	}

	// 2. 遍历 RSS
	log.Println("开始采集 RSS Feed...")
	processFeeds(mgr)

	// 3. 抓取正文与生成摘要
	log.Println("开始处理正文与 AI 摘要...")
	processContentAndLLM(mgr)

	log.Println("所有任务执行完毕。")
}