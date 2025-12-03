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
	"strings"
	"sync"
	"time"

	md "github.com/JohannesKaufmann/html-to-markdown"
	_ "github.com/go-sql-driver/mysql" // MySQL 驱动
	"github.com/mmcdole/gofeed"
)

// --- 配置结构体 ---

type ProxyConfig struct {
	Proxies []string `json:"proxies"`
}

type DBConfig struct {
	MySQL struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		User     string `json:"user"`
		Password string `json:"password"`
		Database string `json:"database"`
		Charset  string `json:"charset"`
	} `json:"mysql"`
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
			Text    string `xml:"text,attr"`
			Title   string `xml:"title,attr"`
			Type    string `xml:"type,attr"`
			XMLUrl  string `xml:"xmlUrl,attr"`
			HTMLUrl string `xml:"htmlUrl,attr"`
		} `xml:"outline"`
	} `xml:"body"`
}

// --- 全局变量与工具 ---

var (
	proxies []string
	llmConf *LLMConfig
	// 全局并发信号量
	concurrencySem chan struct{}
)

// 初始化网络客户端配置
func initNetwork() {
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
}

// 指数退避重试请求
func doRequest(method, reqUrl string, body io.Reader, headers map[string]string) ([]byte, error) {
	var lastErr error

	for i := 0; i < 3; i++ {
		// 创建基础 Client
		client := &http.Client{
			Timeout: 600 * time.Second,
		}

		// 动态设置代理
		if len(proxies) > 0 {
			proxyUrlStr := proxies[rand.Intn(len(proxies))]
			proxyUrl, err := url.Parse(proxyUrlStr)
			if err == nil {
				// 只有当解析成功时才设置 Transport
				client.Transport = &http.Transport{
					Proxy: http.ProxyURL(proxyUrl),
				}
			}
		}
		// 如果没有代理，client.Transport 保持为 nil，Go 会自动使用 DefaultTransport

		req, err := http.NewRequest(method, reqUrl, body)
		if err != nil {
			return nil, err
		}

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

// 生成大写 MD5
func md5Upper(s string) string {
	hash := md5.Sum([]byte(s))
	return strings.ToUpper(hex.EncodeToString(hash[:]))
}

// --- 数据库管理 ---

type DBManager struct {
	DB *sql.DB
}

func initDB() (*DBManager, error) {
	// 读取 db.json
	file, err := os.ReadFile("./db.json")
	if err != nil {
		return nil, fmt.Errorf("读取 db.json 失败: %v", err)
	}
	var cfg DBConfig
	if err := json.Unmarshal(file, &cfg); err != nil {
		return nil, fmt.Errorf("解析 db.json 失败: %v", err)
	}

	// 构建 DSN (Data Source Name)
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=%s&parseTime=True&loc=Local",
		cfg.MySQL.User,
		cfg.MySQL.Password,
		cfg.MySQL.Host,
		cfg.MySQL.Port,
		cfg.MySQL.Database,
		cfg.MySQL.Charset,
	)

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}

	// 测试连接
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("连接数据库失败: %v", err)
	}
	log.Println("成功连接到 MySQL 数据库")

	mgr := &DBManager{DB: db}

	// 创建 RSS 表 (适配 MySQL: 使用 VARCHAR 作为主键)
	_, err = mgr.DB.Exec(`CREATE TABLE IF NOT EXISTS rss (
		rss_host VARCHAR(255) PRIMARY KEY,
		rss_title VARCHAR(255),
		rss_url TEXT,
		update_at DATETIME
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
	if err != nil {
		return nil, fmt.Errorf("创建 rss 表失败: %v", err)
	}

	// 创建 Article 表
	_, err = mgr.DB.Exec(`CREATE TABLE IF NOT EXISTS article (
		article_key CHAR(32) PRIMARY KEY,
		article_link TEXT,
		article_title VARCHAR(512),
		article_published BIGINT,
		article_author VARCHAR(255)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
	if err != nil {
		return nil, fmt.Errorf("创建 article 表失败: %v", err)
	}

	// 创建 Content 表
	_, err = mgr.DB.Exec(`CREATE TABLE IF NOT EXISTS content_store (
		article_key CHAR(32) PRIMARY KEY,
		value LONGTEXT
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
	if err != nil {
		return nil, fmt.Errorf("创建 content_store 表失败: %v", err)
	}

	// 创建 LLM 表
	_, err = mgr.DB.Exec(`CREATE TABLE IF NOT EXISTS llm_store (
		article_key CHAR(32) PRIMARY KEY,
		value TEXT
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
	if err != nil {
		return nil, fmt.Errorf("创建 llm_store 表失败: %v", err)
	}

	return mgr, nil
}

func (mgr *DBManager) Close() {
	if mgr.DB != nil {
		mgr.DB.Close()
	}
}

// --- 核心业务逻辑 ---

// 步骤 1: 获取 OPML 并入库 (串行)
func processOPML(opmlUrl string, mgr *DBManager) error {
	log.Println("[步骤1] 正在获取 OPML...", opmlUrl)
	data, err := doRequest("GET", opmlUrl, nil, nil)
	if err != nil {
		return err
	}

	var opml OPML
	if err := xml.Unmarshal(data, &opml); err != nil {
		return err
	}

	// 使用 REPLACE INTO 处理重复更新
	stmt, err := mgr.DB.Prepare("REPLACE INTO rss (rss_host, rss_title, rss_url, update_at) VALUES (?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	count := 0
	for _, outline := range opml.Body.Outlines {
		if outline.XMLUrl == "" {
			continue
		}
		u, err := url.Parse(outline.XMLUrl)
		host := ""
		if err == nil {
			host = u.Host
		}

		title := outline.Title
		if title == "" {
			title = outline.Text
		}

		_, err = stmt.Exec(host, title, outline.XMLUrl, time.Now())
		if err != nil {
			log.Printf("RSS入库失败: %v", err)
		} else {
			count++
		}
	}
	log.Printf("[步骤1] 完成，共处理 %d 个 RSS 源", count)
	return nil
}

// 步骤 2: 遍历 RSS 并采集文章元数据 (并发)
func processFeeds(mgr *DBManager) {
	log.Println("[步骤2] 开始解析 RSS 订阅源...")
	rows, err := mgr.DB.Query("SELECT rss_host, rss_url FROM rss")
	if err != nil {
		log.Println("无法查询 RSS 表:", err)
		return
	}
	defer rows.Close()

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

	var wg sync.WaitGroup
	for _, f := range feeds {
		wg.Add(1)
		concurrencySem <- struct{}{}

		go func(feedInfo rssItem) {
			defer wg.Done()
			defer func() { <-concurrencySem }()

			log.Printf("正在解析 RSS: %s", feedInfo.URL)
			parser := gofeed.NewParser()

			xmlData, err := doRequest("GET", feedInfo.URL, nil, nil)
			if err != nil {
				log.Printf("获取 RSS 失败 %s: %v", feedInfo.URL, err)
				return
			}

			feed, err := parser.ParseString(string(xmlData))
			if err != nil {
				log.Printf("解析 XML 失败 %s: %v", feedInfo.URL, err)
				return
			}

			for _, item := range feed.Items {
				link := item.Link
				if link == "" {
					continue
				}
				u, err := url.Parse(link)
				if err == nil {
					// 补全相对路径链接
					if u.Scheme == "" || u.Host == "" {
						if !strings.HasPrefix(link, "http") {
							scheme := "https"
							if strings.HasPrefix(feedInfo.URL, "http:") {
								scheme = "http"
							}
							link = fmt.Sprintf("%s://%s%s", scheme, feedInfo.Host, link)
						}
					}
				}

				key := md5Upper(link)
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

				_, err = mgr.DB.Exec(`INSERT IGNORE INTO article 
					(article_key, article_link, article_title, article_published, article_author) 
					VALUES (?, ?, ?, ?, ?)`,
					key, link, item.Title, pubTime, author)
				
				if err != nil {
					log.Printf("文章元数据插入错误: %v", err)
				}
			}
		}(f)
	}
	wg.Wait()
	log.Println("[步骤2] RSS 解析完成")
}

// 步骤 3: 获取正文 -> 转 Markdown (并发)
func processContent(mgr *DBManager) {
	log.Println("[步骤3] 开始抓取正文并清洗为 Markdown...")
	rows, err := mgr.DB.Query("SELECT article_key, article_link, article_title FROM article")
	if err != nil {
		log.Println(err)
		return
	}
	defer rows.Close()

	type task struct {
		Key   string
		Link  string
		Title string
	}
	var tasks []task
	for rows.Next() {
		var t task
		rows.Scan(&t.Key, &t.Link, &t.Title)
		tasks = append(tasks, t)
	}
	rows.Close()

	var wg sync.WaitGroup

	for _, t := range tasks {
		wg.Add(1)
		concurrencySem <- struct{}{}

		go func(taskItem task) {
			defer wg.Done()
			defer func() { <-concurrencySem }()

			// 检查是否已存在正文
			var exists int
			err := mgr.DB.QueryRow("SELECT 1 FROM content_store WHERE article_key = ?", taskItem.Key).Scan(&exists)
			
			// 如果存在则跳过
			if err == nil {
				return
			}

			log.Printf("正在抓取正文: %s", taskItem.Title)
			htmlBytes, err := doRequest("GET", taskItem.Link, nil, nil)
			if err != nil {
				log.Printf("抓取失败 [%s]: %v", taskItem.Title, err)
				return
			}

			// 转 Markdown
			converter := md.NewConverter("", true, nil)
			markdown, err := converter.ConvertString(string(htmlBytes))
			if err != nil {
				log.Printf("Markdown 转换失败: %v", err)
				markdown = "Conversion Failed"
			}

			// 存储
			_, err = mgr.DB.Exec("REPLACE INTO content_store (article_key, value) VALUES (?, ?)", taskItem.Key, markdown)
			if err != nil {
				log.Printf("Content 存储失败: %v", err)
			}
		}(t)
	}
	wg.Wait()
	log.Println("[步骤3] 正文抓取完成")
}

// 步骤 4: 读取正文 -> 生成 LLM 摘要 (并发)
func processLLM(mgr *DBManager) {
	log.Println("[步骤4] 开始生成 LLM 摘要...")
	// 加载 LLM 配置
	llmData, err := os.ReadFile("./llm.json")
	if err != nil {
		log.Printf("跳过 LLM 步骤: 无法读取 llm.json: %v", err)
		return
	}
	llmConf = &LLMConfig{}
	json.Unmarshal(llmData, llmConf)

	// 只查询有正文且未生成摘要的文章
	// 为了方便日志显示标题，我们联表查询，或者这里简单点：查询 article 表，然后查 content
	rows, err := mgr.DB.Query("SELECT article_key, article_link, article_title FROM article")
	if err != nil {
		log.Println(err)
		return
	}
	defer rows.Close()

	type task struct {
		Key   string
		Link  string
		Title string
	}
	var tasks []task
	for rows.Next() {
		var t task
		rows.Scan(&t.Key, &t.Link, &t.Title)
		tasks = append(tasks, t)
	}
	rows.Close()

	var wg sync.WaitGroup

	for _, t := range tasks {
		wg.Add(1)
		concurrencySem <- struct{}{}

		go func(taskItem task) {
			defer wg.Done()
			defer func() { <-concurrencySem }()

			// 1. 检查是否已有摘要
			var exists int
			err := mgr.DB.QueryRow("SELECT 1 FROM llm_store WHERE article_key = ?", taskItem.Key).Scan(&exists)
			if err == nil {
				return // 已有摘要，跳过
			}

			// 2. 获取正文 Markdown
			var content string
			err = mgr.DB.QueryRow("SELECT value FROM content_store WHERE article_key = ?", taskItem.Key).Scan(&content)
			
			// 如果没有正文或正文太短，无法生成摘要
			if err != nil || len(content) < 50 {
				return
			}

			// 调用 LLM
			log.Printf("正在生成摘要: %s", taskItem.Title)
			summary, err := callLLM(content)
			if err != nil {
				log.Printf("LLM 调用失败 [%s]: %v", taskItem.Title, err)
				return
			}

			_, err = mgr.DB.Exec("REPLACE INTO llm_store (article_key, value) VALUES (?, ?)", taskItem.Key, summary)
			if err != nil {
				log.Printf("LLM 结果存储失败: %v", err)
			}
		}(t)
	}
	wg.Wait()
	log.Println("[步骤4] 摘要生成完成")
}

// 调用 LLM API
func callLLM(articleContent string) (string, error) {
	// 简单截断防止 Token 溢出
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

	apiKey := ""
	if len(llmConf.ApiKeys) > 0 {
		apiKey = llmConf.ApiKeys[rand.Intn(len(llmConf.ApiKeys))]
	}

	headers := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": apiKey,
	}

	respBytes, err := doRequest("POST", llmConf.Server, bytes.NewBuffer(jsonPayload), headers)
	if err != nil {
		return "", err
	}

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
	opmlUrl := flag.String("url", "", "OPML 文件的 URL 地址 (仅步骤 0,1 需要)")
	concurrency := flag.Int("c", 1, "并发控制数 (默认为 1)")
	step := flag.Int("step", 0, "运行步骤: 0=全部, 1=获取OPML, 2=解析RSS, 3=抓取正文, 4=LLM摘要")
	flag.Parse()

	// 仅在步骤 0 或 1 且未提供 URL 时报错
	if (*step == 0 || *step == 1) && *opmlUrl == "" {
		fmt.Println("错误: 运行步骤 0 或 1 时必须提供 -url 参数")
		return
	}

	// 初始化并发信号量
	if *concurrency < 1 {
		*concurrency = 1
	}
	concurrencySem = make(chan struct{}, *concurrency)

	rand.Seed(time.Now().UnixNano())
	initNetwork()

	log.Println("正在连接 MySQL 数据库...")
	mgr, err := initDB()
	if err != nil {
		log.Fatalf("数据库初始化失败: %v", err)
	}
	defer mgr.Close()

	// 步骤分发逻辑
	// 如果是 0，则所有条件都满足；如果是具体数字，则只满足对应条件
	
	// Step 1: OPML 获取
	if *step == 0 || *step == 1 {
		if err := processOPML(*opmlUrl, mgr); err != nil {
			log.Fatalf("处理 OPML 失败: %v", err)
		}
	}

	// Step 2: RSS 解析
	if *step == 0 || *step == 2 {
		processFeeds(mgr)
	}

	// Step 3: 正文抓取
	if *step == 0 || *step == 3 {
		processContent(mgr)
	}

	// Step 4: LLM 摘要
	if *step == 0 || *step == 4 {
		processLLM(mgr)
	}

	log.Println("所有指定任务执行完毕。")
}