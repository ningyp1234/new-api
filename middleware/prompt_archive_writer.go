// Package middleware — prompt_archive_writer: 装上一个 gin.ResponseWriter 包装器，
// 把发往客户端的响应字节同步 tee 一份到内存 buffer，给 service.PromptArchiveCommit
// 在 PostTextConsumeQuota 收尾时取出来解析 completion 文本。
//
// 设计要点:
//
//  1. 只在 PROMPT_ARCHIVE_ENABLED=true 时启用，否则零开销 c.Next()
//
//  2. 只对会产生 completion 的 chat/completions、messages、responses 路径装 wrapper，
//     其他路径（图像、嵌入、音频、模型列表等）走默认 writer 不消耗内存
//
//  3. 4 MB 硬上限：超过这个体积的响应只 tee 前 4 MB，不影响客户端正常接收完整数据，
//     避免被超长 stream 拖爆内存
//
//  4. teeBuffer 通过 c.Set(ContextKeyPromptArchiveBuf, ...) 暴露给下游，
//     downstream 读取无需知道 wrapper 细节
package middleware

import (
	"bytes"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

// ContextKeyPromptArchiveBuf —— gin.Context 里挂存响应 buffer 的 key
// !! 必须与 service/prompt_archiver.go 里的 promptArchiveBufKey 常量保持一致 !!
// 两边 hardcode 是为了避免 import cycle (middleware 已经依赖 service)
const ContextKeyPromptArchiveBuf = "prompt_archive_response_buf"

// PromptArchiveResponseCapLimit 4 MB
const PromptArchiveResponseCapLimit = 4 * 1024 * 1024

// teeWriter 包装 gin.ResponseWriter，每次 Write 同时写到底层 writer 和 buffer
type teeWriter struct {
	gin.ResponseWriter
	buf      *bytes.Buffer
	capLimit int
	exceeded bool // 一旦超限就只 forward 不再 buffer
}

// Write 是热路径：先写客户端 (即便 buffer 失败也不能影响客户端响应)，再 tee 到 buffer
func (w *teeWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if !w.exceeded && w.buf.Len()+len(p) <= w.capLimit {
		w.buf.Write(p)
	} else {
		w.exceeded = true
	}
	return n, err
}

// WriteString 同 Write，gin 内部和某些 framework 会优先用 WriteString 走零拷贝
func (w *teeWriter) WriteString(s string) (int, error) {
	n, err := w.ResponseWriter.WriteString(s)
	if !w.exceeded && w.buf.Len()+len(s) <= w.capLimit {
		w.buf.WriteString(s)
	} else {
		w.exceeded = true
	}
	return n, err
}

// shouldCaptureForPath 仅对会产生 completion 文本的接口启用
// (避免给图像 / 嵌入 / 文件等接口分配无谓的 buffer)
func shouldCaptureForPath(path string) bool {
	switch {
	case strings.HasSuffix(path, "/v1/chat/completions"):
		return true
	case strings.HasSuffix(path, "/v1/completions"):
		return true
	case strings.HasSuffix(path, "/v1/messages"):
		return true
	case strings.HasSuffix(path, "/v1/responses"):
		return true
	case strings.HasSuffix(path, "/v1/responses/compact"):
		return true
	}
	return false
}

// PromptArchiveResponseCapture 是 gin middleware，必须挂在 TokenAuth() 之后、
// Distribute() 之前；这样我们既知道 UserId，又能在请求处理前装 wrapper。
//
// 调用流：
//   - 命中条件 → 用 teeWriter 替换 c.Writer，往 c 里塞 buffer 引用
//   - c.Next() 走完处理链（adaptor.DoResponse 写响应到 teeWriter）
//   - service.PostTextConsumeQuota → service.PromptArchiveCommit 从 c 取 buffer 解析文本
//
// 关键不变量：响应已 flush 给客户端后才能解析 buffer，所以 commit 必须在 c.Next()
// 之后（即在 PostTextConsumeQuota 路径上）触发。
func PromptArchiveResponseCapture() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !common.PromptArchiveEnabled {
			c.Next()
			return
		}
		if !shouldCaptureForPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		buf := bytes.NewBuffer(make([]byte, 0, 8192))
		c.Writer = &teeWriter{
			ResponseWriter: c.Writer,
			buf:            buf,
			capLimit:       PromptArchiveResponseCapLimit,
		}
		c.Set(ContextKeyPromptArchiveBuf, buf)
		c.Next()
	}
}
