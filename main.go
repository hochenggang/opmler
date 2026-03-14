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
	_ "github.com/go-sql-driver/mysql"
	"github.com/mmcdole/gofeed"
)

const (
	maxRetries      = 3
	retryDelayBase  = 2
	httpTimeout     = 30 * time.Second
	llmContentLimit = 12000
)

type Config struct {
	Proxies []string `json:"proxies"`
	MySQL   struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		User     string `json:"user"`
		Password string `json:"password"`
		Database string `json:"database"`
		Charset  string `json:"charset"`
	} `json:"mysql"`
	LLM struct {
		Server  string   `json:"server"`
		ApiKeys []string `json:"api_keys"`
		Model   string   `json:"model"`
		Prompt  string   `json:"prompt"`
	} `json:"llm"`
}

type OPML struct {
	Body struct {
		Outlines []struct {
			Text    string `xml:"text,attr"`
			Title   string `xml:"title,attr"`
			XMLUrl  string `xml:"xmlUrl,attr"`
			HTMLUrl string `xml:"htmlUrl,attr"`
		} `xml:"outline"`
	} `xml:"body"`
}

type LLMResult struct {
	Keywords []string `json:"keywords"`
	Summary  string   `json:"summary"`
}

type App struct {
	db        *sql.DB
	config    *Config
	proxies   []string
	baseDir   string
	converter *md.Converter
}

func NewApp() *App {
	execPath, _ := os.Executable()
	baseDir := filepath.Dir(execPath)
	if baseDir == "" {
		baseDir, _ = os.Getwd()
	}
	return &App{
		config:    &Config{},
		baseDir:   baseDir,
		converter: md.NewConverter("", true, nil),
	}
}

func (a *App) configPath(filename string) string {
	return filepath.Join(a.baseDir, filename)
}

func (a *App) init() error {
	if err := a.loadConfig(); err != nil {
		return err
	}
	if err := a.initDB(); err != nil {
		return err
	}
	return nil
}

func (a *App) loadConfig() error {
	dbPath := a.configPath("db.json")
	data, err := os.ReadFile(dbPath)
	if err != nil {
		return fmt.Errorf("读取 %s 失败: %v", dbPath, err)
	}
	if err := json.Unmarshal(data, &a.config); err != nil {
		return fmt.Errorf("解析 db.json 失败: %v", err)
	}

	proxyPath := a.configPath("proxys.json")
	if data, err = os.ReadFile(proxyPath); err == nil {
		var pc struct {
			Proxies []string `json:"proxies"`
		}
		if json.Unmarshal(data, &pc) == nil && len(pc.Proxies) > 0 {
			a.proxies = pc.Proxies
			log.Printf("加载 %d 个代理服务器", len(a.proxies))
		}
	}

	llmPath := a.configPath("llm.json")
	if data, err = os.ReadFile(llmPath); err == nil {
		json.Unmarshal(data, &a.config)
	}

	return nil
}

func (a *App) initDB() error {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=%s&parseTime=True&loc=Local",
		a.config.MySQL.User,
		a.config.MySQL.Password,
		a.config.MySQL.Host,
		a.config.MySQL.Port,
		a.config.MySQL.Database,
		a.config.MySQL.Charset,
	)

	var db *sql.DB
	var err error
	for i := 0; i < maxRetries; i++ {
		db, err = sql.Open("mysql", dsn)
		if err != nil {
			time.Sleep(time.Duration(retryDelayBase<<(i+1)) * time.Second)
			continue
		}
		if err = db.Ping(); err != nil {
			db.Close()
			time.Sleep(time.Duration(retryDelayBase<<(i+1)) * time.Second)
			continue
		}
		break
	}
	if err != nil {
		return fmt.Errorf("连接数据库失败: %v", err)
	}

	a.db = db
	log.Println("成功连接 MySQL")

	tables := []string{
		`CREATE TABLE IF NOT EXISTS rss (
			rss_host VARCHAR(255) PRIMARY KEY,
			rss_title VARCHAR(255),
			rss_url TEXT,
			update_at DATETIME
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS article (
			article_key CHAR(32) PRIMARY KEY,
			article_link TEXT,
			article_title VARCHAR(512),
			article_published BIGINT,
			article_author VARCHAR(255)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS content_store (
			article_key CHAR(32) PRIMARY KEY,
			value LONGTEXT
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS llm_store (
			article_key CHAR(32) PRIMARY KEY,
			value TEXT
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS keyword_store (
			id BIGINT AUTO_INCREMENT PRIMARY KEY,
			keyword VARCHAR(100) NOT NULL,
			article_key CHAR(32) NOT NULL,
			INDEX idx_keyword (keyword),
			UNIQUE KEY unique_key_article (keyword, article_key)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}

	for _, sql := range tables {
		if _, err := db.Exec(sql); err != nil {
			return fmt.Errorf("创建表失败: %v", err)
		}
	}
	return nil
}

func (a *App) close() {
	if a.db != nil {
		a.db.Close()
	}
}

func (a *App) httpRequest(method, urlStr string, body []byte, headers map[string]string) ([]byte, error) {
	var lastErr error

	for retry := 0; retry < maxRetries; retry++ {
		client := &http.Client{Timeout: httpTimeout}

		if len(a.proxies) > 0 {
			proxyURL, err := url.Parse(a.proxies[rand.Intn(len(a.proxies))])
			if err == nil {
				client.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
			}
		}

		var reqBody io.Reader
		if body != nil {
			reqBody = bytes.NewBuffer(body)
		}

		req, err := http.NewRequest(method, urlStr, reqBody)
		if err != nil {
			return nil, err
		}

		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; RSSCollector/1.0)")
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			delay := time.Duration(math.Pow(float64(retryDelayBase), float64(retry))) * time.Second
			log.Printf("  请求失败 [%s], %v 后重试 (%d/%d): %v", urlStr, delay, retry+1, maxRetries, err)
			time.Sleep(delay)
			continue
		}

		defer resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return io.ReadAll(resp.Body)
		}

		lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			delay := time.Duration(math.Pow(float64(retryDelayBase), float64(retry))) * time.Second
			log.Printf("  HTTP %d [%s], %v 后重试 (%d/%d)", resp.StatusCode, urlStr, delay, retry+1, maxRetries)
			time.Sleep(delay)
			continue
		}
		break
	}

	return nil, fmt.Errorf("请求失败: %v", lastErr)
}

func md5Hash(s string) string {
	hash := md5.Sum([]byte(s))
	return strings.ToUpper(hex.EncodeToString(hash[:]))
}

func (a *App) step1_FetchOPML(opmlURL string) error {
	log.Println("[步骤1] 获取 OPML...")

	data, err := a.httpRequest("GET", opmlURL, nil, nil)
	if err != nil {
		return fmt.Errorf("获取 OPML 失败: %v", err)
	}

	var opml OPML
	if err := xml.Unmarshal(data, &opml); err != nil {
		return fmt.Errorf("解析 OPML 失败: %v", err)
	}

	stmt, err := a.db.Prepare("REPLACE INTO rss (rss_host, rss_title, rss_url, update_at) VALUES (?, ?, ?, ?)")
	if err != nil {
		return fmt.Errorf("准备语句失败: %v", err)
	}
	defer stmt.Close()

	count := 0
	for _, o := range opml.Body.Outlines {
		if o.XMLUrl == "" {
			continue
		}
		u, _ := url.Parse(o.XMLUrl)
		host := ""
		if u != nil {
			host = u.Host
		}
		title := o.Title
		if title == "" {
			title = o.Text
		}
		if _, err := stmt.Exec(host, title, o.XMLUrl, time.Now()); err == nil {
			count++
		} else {
			log.Printf("  RSS 入库失败 [%s]: %v", o.XMLUrl, err)
		}
	}

	log.Printf("[步骤1] 完成，入库 %d 个 RSS 源", count)
	return nil
}

func (a *App) step2_FetchFeeds() {
	log.Println("[步骤2] 解析 RSS 订阅源...")

	rows, err := a.db.Query("SELECT rss_host, rss_url FROM rss")
	if err != nil {
		log.Printf("查询 RSS 失败: %v", err)
		return
	}

	type feedItem struct{ host, url string }
	var feeds []feedItem
	for rows.Next() {
		var f feedItem
		if err := rows.Scan(&f.host, &f.url); err == nil {
			feeds = append(feeds, f)
		}
	}
	rows.Close()

	if len(feeds) == 0 {
		log.Println("[步骤2] 无 RSS 源，跳过")
		return
	}

	log.Printf("共 %d 个 RSS 源", len(feeds))

	successCount := 0
	failCount := 0
	totalArticles := 0

	for i, f := range feeds {
		log.Printf("[%d/%d] %s", i+1, len(feeds), f.url)

		data, err := a.httpRequest("GET", f.url, nil, nil)
		if err != nil {
			log.Printf("  -> 获取失败: %v", err)
			failCount++
			continue
		}

		parser := gofeed.NewParser()
		feed, err := parser.ParseString(string(data))
		if err != nil {
			log.Printf("  -> 解析失败: %v", err)
			failCount++
			continue
		}

		count := 0
		for _, item := range feed.Items {
			if item.Link == "" {
				continue
			}

			link := item.Link
			u, err := url.Parse(link)
			if err == nil && (u.Scheme == "" || u.Host == "") && !strings.HasPrefix(link, "http") {
				scheme := "https"
				if strings.HasPrefix(f.url, "http:") {
					scheme = "http"
				}
				if !strings.HasPrefix(link, "/") {
					link = "/" + link
				}
				link = fmt.Sprintf("%s://%s%s", scheme, f.host, link)
			}

			key := md5Hash(link)
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
			} else if len(item.Authors) > 0 && item.Authors[0] != nil {
				author = item.Authors[0].Name
			}

			_, err = a.db.Exec(`INSERT IGNORE INTO article (article_key, article_link, article_title, article_published, article_author) VALUES (?, ?, ?, ?, ?)`,
				key, link, item.Title, pubTime, author)
			if err == nil {
				count++
			}
		}
		log.Printf("  -> %d 篇文章", count)
		successCount++
		totalArticles += count
	}

	log.Printf("[步骤2] 完成: 成功 %d, 失败 %d, 共 %d 篇文章", successCount, failCount, totalArticles)
}

func (a *App) step3_FetchContent() {
	log.Println("[步骤3] 抓取正文...")

	rows, err := a.db.Query("SELECT article_key, article_link, article_title FROM article")
	if err != nil {
		log.Printf("查询文章失败: %v", err)
		return
	}

	type article struct{ key, link, title string }
	var articles []article
	for rows.Next() {
		var a article
		if err := rows.Scan(&a.key, &a.link, &a.title); err == nil {
			articles = append(articles, a)
		}
	}
	rows.Close()

	if len(articles) == 0 {
		log.Println("[步骤3] 无文章，跳过")
		return
	}

	log.Printf("共 %d 篇文章", len(articles))

	successCount := 0
	skipCount := 0
	failCount := 0

	for i, art := range articles {
		var exists int
		if err := a.db.QueryRow("SELECT 1 FROM content_store WHERE article_key = ?", art.key).Scan(&exists); err == nil {
			skipCount++
			continue
		}

		log.Printf("[%d/%d] %s", i+1, len(articles), art.title)

		html, err := a.httpRequest("GET", art.link, nil, nil)
		if err != nil {
			log.Printf("  -> 抓取失败，删除文章: %v", err)
			a.db.Exec("DELETE FROM article WHERE article_key = ?", art.key)
			failCount++
			continue
		}

		markdown := "Conversion Failed"
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("  -> Markdown 转换 panic: %v", r)
				}
			}()
			if m, err := a.converter.ConvertString(string(html)); err == nil {
				markdown = m
			}
		}()

		if _, err := a.db.Exec("REPLACE INTO content_store (article_key, value) VALUES (?, ?)", art.key, markdown); err != nil {
			log.Printf("  -> 存储失败: %v", err)
			failCount++
			continue
		}

		successCount++
	}

	log.Printf("[步骤3] 完成: 成功 %d, 跳过 %d, 失败 %d", successCount, skipCount, failCount)
}

func (a *App) step4_GenerateSummary() {
	log.Println("[步骤4] 生成 LLM 摘要...")

	if a.config.LLM.Server == "" {
		log.Println("未配置 LLM，跳过")
		return
	}

	if len(a.config.LLM.ApiKeys) == 0 {
		log.Println("未配置 API Key，跳过")
		return
	}

	rows, err := a.db.Query("SELECT article_key, article_title FROM article")
	if err != nil {
		log.Printf("查询文章失败: %v", err)
		return
	}

	type article struct{ key, title string }
	var articles []article
	for rows.Next() {
		var a article
		if err := rows.Scan(&a.key, &a.title); err == nil {
			articles = append(articles, a)
		}
	}
	rows.Close()

	if len(articles) == 0 {
		log.Println("[步骤4] 无文章，跳过")
		return
	}

	log.Printf("共 %d 篇文章", len(articles))

	successCount := 0
	skipCount := 0
	failCount := 0

	for i, art := range articles {
		var exists int
		if err := a.db.QueryRow("SELECT 1 FROM llm_store WHERE article_key = ?", art.key).Scan(&exists); err == nil {
			skipCount++
			continue
		}

		var content string
		if err := a.db.QueryRow("SELECT value FROM content_store WHERE article_key = ?", art.key).Scan(&content); err != nil || len(content) < 50 {
			skipCount++
			continue
		}

		log.Printf("[%d/%d] %s", i+1, len(articles), art.title)

		result, err := a.callLLM(content)
		if err != nil {
			log.Printf("  -> LLM 失败: %v", err)
			failCount++
			continue
		}

		jsonData, _ := json.Marshal(result)
		if _, err := a.db.Exec("REPLACE INTO llm_store (article_key, value) VALUES (?, ?)", art.key, string(jsonData)); err != nil {
			log.Printf("  -> 存储失败: %v", err)
			failCount++
			continue
		}

		for _, kw := range result.Keywords {
			kw = strings.TrimSpace(kw)
			if kw != "" {
				a.db.Exec("INSERT IGNORE INTO keyword_store (keyword, article_key) VALUES (?, ?)", kw, art.key)
			}
		}

		successCount++
	}

	log.Printf("[步骤4] 完成: 成功 %d, 跳过 %d, 失败 %d", successCount, skipCount, failCount)
}

func (a *App) callLLM(content string) (*LLMResult, error) {
	if len(content) > llmContentLimit {
		content = content[:llmContentLimit]
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"model": a.config.LLM.Model,
		"messages": []map[string]string{
			{"role": "system", "content": a.config.LLM.Prompt},
			{"role": "user", "content": content},
		},
		"stream":          false,
		"response_format": map[string]string{"type": "json_object"},
	})

	keys := make([]string, len(a.config.LLM.ApiKeys))
	copy(keys, a.config.LLM.ApiKeys)
	rand.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })

	var lastErr error
	for keyIdx, key := range keys {
		for attempt := 0; attempt < maxRetries; attempt++ {
			headers := map[string]string{
				"Content-Type":  "application/json",
				"Authorization": key,
			}

			resp, err := a.httpRequest("POST", a.config.LLM.Server, payload, headers)
			if err != nil {
				lastErr = err
				break
			}

			var result struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}

			if err := json.Unmarshal(resp, &result); err != nil {
				lastErr = err
				continue
			}

			if result.Error.Message != "" {
				lastErr = fmt.Errorf("%s", result.Error.Message)
				log.Printf("  Key[%d] 错误: %s", keyIdx+1, result.Error.Message)
				break
			}

			if len(result.Choices) == 0 {
				lastErr = fmt.Errorf("空响应")
				continue
			}

			raw := result.Choices[0].Message.Content
			raw = cleanJSON(raw)

			var llmResult LLMResult
			if err := json.Unmarshal([]byte(raw), &llmResult); err != nil {
				lastErr = err
				continue
			}

			if len(llmResult.Keywords) == 0 || llmResult.Summary == "" {
				lastErr = fmt.Errorf("字段缺失")
				continue
			}

			return &llmResult, nil
		}
	}

	return nil, fmt.Errorf("LLM 调用失败: %v", lastErr)
}

func cleanJSON(raw string) string {
	lines := strings.Split(raw, "\n")
	var cleaned []string
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "```") {
			cleaned = append(cleaned, line)
		}
	}
	result := strings.Join(cleaned, "\n")
	start := strings.Index(result, "{")
	end := strings.LastIndex(result, "}")
	if start != -1 && end > start {
		return result[start : end+1]
	}
	return result
}

func setupLog(baseDir string) error {
	logPath := filepath.Join(baseDir, "log.txt")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return fmt.Errorf("无法打开日志文件 %s: %v", logPath, err)
	}
	log.SetOutput(io.MultiWriter(os.Stdout, logFile))
	return nil
}

func main() {
	opmlURL := flag.String("url", "", "OPML URL (步骤1需要)")
	step := flag.Int("step", 1, "起始步骤: 1=OPML, 2=RSS, 3=正文, 4=LLM")
	flag.Parse()

	rand.Seed(time.Now().UnixNano())

	execPath, _ := os.Executable()
	baseDir := filepath.Dir(execPath)
	if baseDir == "" {
		baseDir, _ = os.Getwd()
	}

	if err := setupLog(baseDir); err != nil {
		fmt.Printf("日志初始化失败: %v\n", err)
	}

	log.Printf("=== 程序启动 ===")
	log.Printf("工作目录: %s", baseDir)
	log.Printf("起始步骤: %d", *step)

	app := NewApp()
	if err := app.init(); err != nil {
		log.Fatalf("初始化失败: %v", err)
	}
	defer app.close()

	switch *step {
	case 1:
		if *opmlURL == "" {
			log.Fatal("步骤1需要 -url 参数")
		}
		if err := app.step1_FetchOPML(*opmlURL); err != nil {
			log.Printf("步骤1失败: %v", err)
		}
		fallthrough
	case 2:
		app.step2_FetchFeeds()
		fallthrough
	case 3:
		app.step3_FetchContent()
		fallthrough
	case 4:
		app.step4_GenerateSummary()
	default:
		log.Fatalf("无效步骤: %d (有效值: 1-4)", *step)
	}

	log.Println("=== 执行完毕 ===")
}
