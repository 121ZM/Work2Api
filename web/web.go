// Package web 内嵌控制台静态资源。
//
// 单文件 HTML（无外部依赖、无构建步骤）—— 反代工具的面板没必要引入
// npm 工具链，一个文件更便于审计与分发。
package web

import (
	"bytes"
	_ "embed"
	"net"
	"net/http"
	"strconv"
	"strings"
)

//go:embed index.html
var indexHTML []byte

// keyPlaceholder 是 index.html 里的占位符，服务时被替换成一段 **JS 表达式**
// （带引号的字符串字面量，空 Key 时替换成 ""）。
//
// 注意它替换的是整个表达式、不是引号内的内容 —— 这样占位符在页面里只出现一次。
// 若写成 `var K = "__PH__"` 且 JS 里再拿 "__PH__" 做比较，ReplaceAll 会把
// 比较用的那处也换掉，导致比较恒真、面板永远拿不到 Key。
const keyPlaceholder = "__W2A_KEY_EXPR__"

// IndexHTML 返回面板 HTML 原始字节（占位符未替换）。
func IndexHTML() []byte { return indexHTML }

// Handler 返回面板处理器。
//
// apiKey 会被注入面板 HTML，浏览器因此无需手填 Key 就能调用管理 API。
//
// 安全边界：**只对回环请求注入**。若把 listen.host 改成 0.0.0.0，
// 外部访问者拿到的页面里 Key 是空的（面板会提示仅本机可用）；
// 否则等于把 Key 直接送给公网扫描器。
func Handler(apiKey string) http.HandlerFunc {
	localPage := renderKey(apiKey)
	remotePage := renderKey("")
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if isLoopback(r.RemoteAddr) {
			_, _ = w.Write(localPage)
			return
		}
		_, _ = w.Write(remotePage)
	}
}

// renderKey 把占位符整体替换成一个带引号的 JS 字符串字面量。
//
// 用 strconv.Quote 借道 Go 的转义：它输出的 \" \\ \uXXXX 在 JS 里语义一致。
// 必须转义 —— 用户可以把 W2A_API_KEY 设成含引号的值，裸拼会直接破坏面板脚本。
func renderKey(apiKey string) []byte {
	return bytes.ReplaceAll(indexHTML, []byte(keyPlaceholder), []byte(strconv.Quote(apiKey)))
}

// isLoopback 判断 RemoteAddr 是否为回环地址。
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
