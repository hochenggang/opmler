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

// LLMResult 定义 LLM 返回的期望 JSON 结构
type LLMResult struct {
	Keywords []string `json:"keywords"`
	Summary  string   `json:"summary"`
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

// 指数退避重试请求 (网络层面的重试)
func doRequest(method, reqUrl string, body io.Reader, headers map[string]string) ([]byte, error) {
	var lastErr error

	// 为了防止 body 在重试时被耗尽，如果 body 不为空，需要读取并在每次请求时重建 Reader
	var bodyBytes []byte
	if body != nil {
		bodyBytes, _ = io.ReadAll(body)
	}

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
				client.Transport = &http.Transport{
					Proxy: http.ProxyURL(proxyUrl),
				}
			}
		}

		// 重建 Body Reader
		var reqBody io.Reader
		if bodyBytes != nil {
			reqBody = bytes.NewBuffer(bodyBytes)
		}

		req, err := http.NewRequest(method, reqUrl, reqBody)
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
		// 只有不是最后一次尝试时才 log，避免刷屏，或者保留 log
		// log.Printf("网络请求失败 [%s]: %v. %v 后重试...", reqUrl, err, sleepDuration)
		time.Sleep(sleepDuration)
	}

	return nil, fmt.Errorf("network request failed after 3 retries: %v", lastErr)
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

	// 构建 DSN
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

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("连接数据库失败: %v", err)
	}
	log.Println("成功连接到 MySQL 数据库")

	mgr := &DBManager{DB: db}

	// 1. RSS 表
	_, err = mgr.DB.Exec(`CREATE TABLE IF NOT EXISTS rss (
		rss_host VARCHAR(255) PRIMARY KEY,
		rss_title VARCHAR(255),
		rss_url TEXT,
		update_at DATETIME
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
	if err != nil {
		return nil, fmt.Errorf("创建 rss 表失败: %v", err)
	}

	// 2. Article 表
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

	// 3. Content 表
	_, err = mgr.DB.Exec(`CREATE TABLE IF NOT EXISTS content_store (
		article_key CHAR(32) PRIMARY KEY,
		value LONGTEXT
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
	if err != nil {
		return nil, fmt.Errorf("创建 content_store 表失败: %v", err)
	}

	// 4. LLM 表 (存储完整 JSON 响应)
	_, err = mgr.DB.Exec(`CREATE TABLE IF NOT EXISTS llm_store (
		article_key CHAR(32) PRIMARY KEY,
		value TEXT
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
	if err != nil {
		return nil, fmt.Errorf("创建 llm_store 表失败: %v", err)
	}

	// 5. Keyword 表 (用于关键词反查)
	_, err = mgr.DB.Exec(`CREATE TABLE IF NOT EXISTS keyword_store (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		keyword VARCHAR(100) NOT NULL,
		article_key CHAR(32) NOT NULL,
		INDEX idx_keyword (keyword),
		UNIQUE KEY unique_key_article (keyword, article_key)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
	if err != nil {
		return nil, fmt.Errorf("创建 keyword_store 表失败: %v", err)
	}

	return mgr, nil
}

func (mgr *DBManager) Close() {
	if mgr.DB != nil {
		mgr.DB.Close()
	}
}

// --- 核心业务逻辑 ---

// 步骤 1: 获取 OPML 并入库
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

// 步骤 2: 遍历 RSS 并采集文章元数据
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

// 步骤 3: 获取正文 -> 转 Markdown
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

			var exists int
			err := mgr.DB.QueryRow("SELECT 1 FROM content_store WHERE article_key = ?", taskItem.Key).Scan(&exists)
			if err == nil {
				return
			}

			log.Printf("正在抓取正文: %s", taskItem.Title)
			htmlBytes, err := doRequest("GET", taskItem.Link, nil, nil)
			if err != nil {
				log.Printf("抓取失败 [%s]: %v", taskItem.Title, err)
				return
			}

			converter := md.NewConverter("", true, nil)
			markdown, err := converter.ConvertString(string(htmlBytes))
			if err != nil {
				log.Printf("Markdown 转换失败: %v", err)
				markdown = "Conversion Failed"
			}

			_, err = mgr.DB.Exec("REPLACE INTO content_store (article_key, value) VALUES (?, ?)", taskItem.Key, markdown)
			if err != nil {
				log.Printf("Content 存储失败: %v", err)
			}
		}(t)
	}
	wg.Wait()
	log.Println("[步骤3] 正文抓取完成")
}

// 步骤 4: 读取正文 -> 生成 LLM 摘要并提取关键词
func processLLM(mgr *DBManager) {
	log.Println("[步骤4] 开始生成 LLM 摘要...")
	llmData, err := os.ReadFile("./llm.json")
	if err != nil {
		log.Printf("跳过 LLM 步骤: 无法读取 llm.json: %v", err)
		return
	}
	llmConf = &LLMConfig{}
	json.Unmarshal(llmData, llmConf)

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
			if err != nil || len(content) < 50 {
				return
			}

			// 3. 调用 LLM (带结构化重试机制)
			log.Printf("正在分析文章: %s", taskItem.Title)
			result, err := callLLM(content)
			if err != nil {
				log.Printf("LLM 处理失败 (已跳过) [%s]: %v", taskItem.Title, err)
				return
			}

			// 4. 存储完整的 JSON 结果到 llm_store
			jsonBytes, _ := json.Marshal(result)
			_, err = mgr.DB.Exec("REPLACE INTO llm_store (article_key, value) VALUES (?, ?)", taskItem.Key, string(jsonBytes))
			if err != nil {
				log.Printf("LLM 结果存储失败: %v", err)
			}

			// 5. 存储关键词到 keyword_store (便于反查)
			for _, kw := range result.Keywords {
				// 简单的清洗：去除首尾空格
				kw = strings.TrimSpace(kw)
				if kw != "" {
					// 插入关键词映射，忽略重复
					_, err := mgr.DB.Exec("INSERT IGNORE INTO keyword_store (keyword, article_key) VALUES (?, ?)", kw, taskItem.Key)
					if err != nil {
						log.Printf("关键词存储失败 [%s]: %v", kw, err)
					}
				}
			}

		}(t)
	}
	wg.Wait()
	log.Println("[步骤4] 摘要生成与关键词提取完成")
}

// 辅助函数：清洗 LLM 返回的字符串
func cleanLLMContent(raw string) string {
	lines := strings.Split(raw, "\n")
	var cleanLines []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// 移除 Markdown 代码块标记，如 ``` 或 ```json
		if strings.HasPrefix(trimmed, "```") {
			continue
		}
		cleanLines = append(cleanLines, line)
	}
	result := strings.Join(cleanLines, "\n")
	
	// 为了更安全，只截取第一个 { 和最后一个 } 之间的内容
	start := strings.Index(result, "{")
	end := strings.LastIndex(result, "}")
	if start != -1 && end != -1 && end > start {
		return result[start : end+1]
	}

	return result
}

// 调用 LLM API (带 Key 轮询和格式验证的重试逻辑)
func callLLM(articleContent string) (*LLMResult, error) {
	// 简单截断防止 Token 溢出
	if len(articleContent) > 12000 {
		articleContent = articleContent[:12000]
	}

	// 构造 Payload，强制 JSON 模式
	payload := map[string]interface{}{
		"model": llmConf.Model,
		"messages": []map[string]string{
			{"role": "system", "content": llmConf.Prompt},
			{"role": "user", "content": articleContent},
		},
		"stream": false,
		"response_format": map[string]string{
			"type": "json_object",
		},
	}

	jsonPayload, _ := json.Marshal(payload)

	// 获取所有 Key 并打乱顺序（简单的负载均衡）
	apiKeys := make([]string, len(llmConf.ApiKeys))
	copy(apiKeys, llmConf.ApiKeys)
	rand.Shuffle(len(apiKeys), func(i, j int) { apiKeys[i], apiKeys[j] = apiKeys[j], apiKeys[i] })

	var lastErr error

	// --- 核心改动：外层循环遍历 Key ---
	for keyIdx, apiKey := range apiKeys {
		// 掩码用于日志，避免泄露完整 Key
		maskedKey := "unknown"
		if len(apiKey) > 8 {
			maskedKey = apiKey[:8] + "..."
		}

		headers := map[string]string{
			"Content-Type":  "application/json",
			"Authorization": apiKey,
		}

		// 内层循环：针对当前 Key 进行重试 (主要解决 JSON 格式错误)
		// 注意：doRequest 内部已经包含了针对网络错误 (429/5xx) 的 3 次重试。
		// 如果 doRequest 返回错误，意味着这个 Key 在网络层面已经失败了 3 次，应该切换 Key。
		// 如果 doRequest 返回成功，但 JSON 格式错误，我们在内层循环重试最多 3 次。
		
		for attempt := 0; attempt < 3; attempt++ {
			
			// 调用网络请求 (doRequest 内部有 3 次网络重试)
			respBytes, err := doRequest("POST", llmConf.Server, bytes.NewBuffer(jsonPayload), headers)
			
			if err != nil {
				// 如果 doRequest 失败，说明网络层面不可用 (例如 429 超过重试次数)
				// 此时不应在内层循环重试，而应直接跳出内层循环，进入下一个 Key
				lastErr = fmt.Errorf("Key [%s] 网络/额度耗尽: %v", maskedKey, err)
				log.Printf("切换 Key 警告: %v", lastErr)
				break // Break inner loop -> Next Key
			}

			// 解析 OpenAI 格式响应
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
				// API 返回了非 JSON 格式 (极少见，可能是服务崩溃)
				lastErr = fmt.Errorf("Key [%s] 响应非 JSON: %v", maskedKey, err)
				// 这种情况可以重试 (attempt++)
				continue 
			}

			if resp.Error.Message != "" {
				// API 返回了业务错误 (如 key 无效)
				lastErr = fmt.Errorf("Key [%s] API 报错: %s", maskedKey, resp.Error.Message)
				log.Printf("切换 Key 警告: %v", lastErr)
				// 业务错误通常重试无效，直接换 Key
				break 
			}

			if len(resp.Choices) == 0 {
				lastErr = fmt.Errorf("Key [%s] 返回空 Choices", maskedKey)
				continue
			}

			rawContent := resp.Choices[0].Message.Content

			// 清洗 Markdown 标记并提取 JSON
			cleanContent := cleanLLMContent(rawContent)

			var result LLMResult
			if err := json.Unmarshal([]byte(cleanContent), &result); err != nil {
				lastErr = fmt.Errorf("Key [%s] 内容格式错误: %v", maskedKey, err)
				// JSON 格式不对，可以在当前 Key 重试生成 (attempt++)
				continue
			}

			// 字段完整性验证
			if len(result.Keywords) == 0 || result.Summary == "" {
				lastErr = fmt.Errorf("Key [%s] 缺少 keywords 或 summary", maskedKey)
				continue
			}

			// 成功！直接返回
			return &result, nil
		}
		
		// 如果内层循环结束仍未返回，说明当前 Key 失败了，外层循环会自动尝试下一个 Key
		log.Printf("Key [%s] 尝试 %d 次后失败，切换下一个 Key (剩余 %d 个)", maskedKey, 3, len(apiKeys)-1-keyIdx)
	}

	return nil, fmt.Errorf("所有 API Key 均已尝试并失败: %v", lastErr)
}

// --- 主程序入口 ---

func main() {
	opmlUrl := flag.String("url", "", "OPML 文件的 URL 地址 (仅步骤 0,1 需要)")
	concurrency := flag.Int("c", 1, "并发控制数 (默认为 1)")
	step := flag.Int("step", 0, "运行步骤: 0=全部, 1=获取OPML, 2=解析RSS, 3=抓取正文, 4=LLM摘要")
	flag.Parse()

	// --- 新增: 配置日志输出到文件和控制台 ---
	logFile, err := os.OpenFile("./log.txt", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		fmt.Printf("无法打开日志文件: %v\n", err)
		return
	}
	defer logFile.Close()

	// 使用 MultiWriter 同时输出到标准输出和文件
	multiWriter := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(multiWriter)
	// -------------------------------------

	if (*step == 0 || *step == 1) && *opmlUrl == "" {
		fmt.Println("错误: 运行步骤 0 或 1 时必须提供 -url 参数")
		return
	}

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

	if *step == 0 || *step == 1 {
		if err := processOPML(*opmlUrl, mgr); err != nil {
			log.Fatalf("处理 OPML 失败: %v", err)
		}
	}

	if *step == 0 || *step == 2 {
		processFeeds(mgr)
	}

	if *step == 0 || *step == 3 {
		processContent(mgr)
	}

	if *step == 0 || *step == 4 {
		processLLM(mgr)
	}

	log.Println("所有指定任务执行完毕。")
}