你作为一个编程专家，

帮我信达雅的设计一个 golang 程序，要求如下：

涉及到网络请求时，如遇错误需要指数级错误延迟重试三次，需要优先读取程序当前目录下的代理配置文件获取代理来随机选择代理进行请求，若无代理文件，使用直连。./proxys.json={
  "proxies": [
    "http://5.45.195.112:18888",
    "http://5.82.202.216:18888"
  ]
}



程序的执行步骤如下：

第一步：解析传入的命令行参数。-url opml_url -db db_name 

第二步：创建数据库。1. 以 db_name 为前缀，创建 db_name  + "_meta.db" 作为元数据数据库。元数据数据库含两个表，分别为 rss 表和 article 表，rss 表存放 rss_host (主机名)作为主键，rss_title，rss_url(xmlUrl), update_at  。article 表存放请求 xmlUrl 后得到的文章元数据，需要使用第三方 golang 库来自动识别 atom 或 feed 或其它格式的xml，处理为含 article_key(文章链接的 md5 大写字符串), article_link(如果缺失域名或非http开头，需要使用 rss 的主机名来补全地址),article_title,article_published（处理为Unix 秒级整数间戳）,article_author 几个字段的数据入库。

2. 以 db_name 为前缀，创建 db_name  + "_content.db" 作为文章html缓存数据库。含 key,value 两个字段。key需要约束为 article 表的 article_key，content 为 字符串，需要使用golang的html转 md5 库，去除无用标签，仅保留主要正文。

3. 以 db_name 为前缀，创建 db_name  + "_llm.db" 作为文章llm摘要缓存数据库。含 key,value 两个字段。key需要约束为 article 表的 article_key，content 为 字符串，需要使用 ./llm.json={
  "server": "https://api.siliconflow.cn/v1/chat/completions",
  "api_keys": [
    "Bearer sk-bbbb",
    "Bearer sk-aaaa"
  ],
  "model": "THUDM/glm-4-9b-chat",
  "prompt": "你是一个聪明的人工智能助手，你的最终输出语言是简体中文。\n请阅读以下Markdown文章内容（你需要自行判断原始语言），并生成一篇简洁、清晰的简体中文总结，总结为一个自然段。总结应梳理文章的脉络，突出核心观点和关键信息，语言风格需朴实优雅，避免使用过于复杂的术语或冗长的句子。总结字数控制在300字以内，确保逻辑连贯、易于理解。请忽略所有违法违规、无价值或与主题无关的言论，专注于文章的主旨和重要细节。\n\n**要求：**\n避免使用\"这篇文章\",\"本文\"之类的开头，使用文章中的主要事物：例如核心事物、概念或场景等作为主语开头。  \n提取文章的核心主题和主要论点。\n概括文章的结构，包括引言、主体和结论（如有）。\n突出文章中的关键事实、数据或观点。\n语言简洁明了，避免冗余或重复。\n确保总结内容忠实于原文，不添加主观臆断。\n请根据以上要求生成总结。"

} 来请求 大语言模型的 API 获取总结摘要入库。


第三步：执行。1. 获取 opml 链接的内容，解析出所有 rss 并入库。2. 遍历 rss 数据库，依次请求 rss_url 获取rss订阅内容，使用第三方解析库解析出所有文章的 article_key(文章链接的 md5 大写字符串), article_link(如果缺失域名或非http开头，需要使用 rss 的主机名来补全地址),article_title,article_published（处理为Unix 秒级整数间戳）,article_author 5个字段的数据，按article_key唯一入库。 3. 遍历元数据库的 article 表，依次遍历所有 article_link,尝试 获取 article_link对应的网页正文，使用第三方库转为 markdown 写入 文章html缓存数据库。尝试基于文章html缓存，获取 llm 总结摘要并入库。