// Package service — prompt_archiver: P0 D2 的核心抓取逻辑
//
// 职责：
//  1. 从 gin.Context 提取 prompt 文本（来自 info.Request）
//  2. 从 middleware 装的 buffer 提取 completion 文本（解析 SSE 或 JSON 响应）
//  3. 用 M-1 DLP 引擎做脱敏 + 分类
//  4. 计算 prompt_hash（归一化后 SHA-256）+ 语言检测
//  5. 异步落盘到 prompt_archives 表
//
// 设计原则：
//   - 失败永远不影响主链路（所有错误 SysLog 但 swallow）
//   - 解析逻辑兼容 OpenAI / Claude / Gemini 三种主流格式（P0 优先 OpenAI）
//   - DLP 脱敏在落盘前完成（raw 也加密存储，redacted 永久存）
package service

import (
	"bytes"
	"strings"
	"unicode"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
)

// promptArchiveBufKey 必须与 middleware/prompt_archive_writer.go 里的
// ContextKeyPromptArchiveBuf 字面量保持一致。两边都 hardcode 是为了避免
// import cycle (middleware 已经依赖 service)。
const promptArchiveBufKey = "prompt_archive_response_buf"

// PromptArchiveCommit 是 PostTextConsumeQuota 末尾调用的入口。
// 单一职责：组装 ArchivePromptPayload 并触发异步落盘。
//
// 入参：
//   - ctx: 当前请求上下文
//   - relayInfo: 已计费的 relay 元数据
//   - promptTokens / completionTokens: 已经计算好的 token 数
//   - useTimeSec: 请求耗时（秒）
//
// 不返回 error —— 任何失败 SysLog 但不传染主链路
func PromptArchiveCommit(
	ctx *gin.Context,
	relayInfo *relaycommon.RelayInfo,
	modelName string,
	promptTokens int,
	completionTokens int,
	useTimeSec int,
) {
	if !common.PromptArchiveEnabled {
		return
	}

	// ── 1. 提取 prompt 原文 ───────────────────────────────────────
	promptRaw := extractPromptText(relayInfo)
	if promptRaw == "" {
		// 没拿到 prompt 文本（可能是图像/嵌入接口走串了，或非 OpenAI 格式）
		// 不报错，跳过归档即可
		return
	}

	// ── 2. 提取 completion 原文（从 middleware 缓存的响应 buffer 里）──
	completionRaw := extractCompletionText(ctx, relayInfo)

	// ── 3. DLP 脱敏 ───────────────────────────────────────────────
	promptHit, promptCats, promptRedacted := SensitiveWordReplace(promptRaw, false)
	completionHit, completionCats, completionRedacted := SensitiveWordReplace(completionRaw, false)
	dlpHit := promptHit || completionHit
	allCats := mergeUniqueStrings(promptCats, completionCats)

	// ── 4. 元数据 ─────────────────────────────────────────────────
	username := ctx.GetString("username")
	requestId := ctx.GetString(common.RequestIdKey)

	payload := &model.ArchivePromptPayload{
		RequestId: requestId,
		UserId:    relayInfo.UserId,
		Username:  username,
		UserGroup: relayInfo.UsingGroup,
		TokenId:   relayInfo.TokenId,
		ChannelId: relayInfo.ChannelId,
		ModelName: modelName,

		PromptRaw:      promptRaw,
		PromptRedacted: promptRedacted,
		PromptHash:     model.ComputePromptHash(promptRedacted),
		PromptTokens:   promptTokens,
		PromptLang:     detectLang(promptRaw),

		CompletionRaw:      completionRaw,
		CompletionRedacted: completionRedacted,
		CompletionTokens:   completionTokens,

		DlpHit:        dlpHit,
		DlpCategories: allCats,

		UseTimeSec: useTimeSec,
		IsStream:   relayInfo.IsStream,
	}

	model.ArchivePrompt(payload)
}

// ============================================================
// extractPromptText: 把 info.Request 里的 messages 拼成可读文本
// ============================================================

// extractPromptText 从 RelayInfo.Request 里提取 prompt 文本
// 支持 OpenAI Chat/Completions 与 Claude Messages。这里只归档可读文本，
// 图片/音频/文件等多模态内容使用占位符，避免把大块二进制或 URL 污染到审计文本。
func extractPromptText(relayInfo *relaycommon.RelayInfo) string {
	if relayInfo == nil || relayInfo.Request == nil {
		return ""
	}
	switch req := relayInfo.Request.(type) {
	case *dto.GeneralOpenAIRequest:
		return formatOpenAIMessages(req.Messages)
	case *dto.ClaudeRequest:
		return formatClaudePrompt(req)
	}
	return ""
}

// formatOpenAIMessages 把 messages 数组拼成可读文本：
//
//	[user] hello
//	[assistant] hi there
//	[user] another question
//
// 多模态内容（image/audio）只放占位符，避免污染 prompt 文本
func formatOpenAIMessages(messages []dto.Message) string {
	if len(messages) == 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(512)
	for i, msg := range messages {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteByte('[')
		b.WriteString(msg.Role)
		b.WriteString("] ")
		if msg.IsStringContent() {
			b.WriteString(msg.StringContent())
			continue
		}
		// 多模态：把每个 part 按类型展开
		for _, part := range msg.ParseContent() {
			switch part.Type {
			case dto.ContentTypeText:
				b.WriteString(part.Text)
			case "image_url":
				b.WriteString("[image]")
			case "input_audio":
				b.WriteString("[audio]")
			case "file":
				b.WriteString("[file]")
			default:
				b.WriteString("[")
				b.WriteString(part.Type)
				b.WriteString("]")
			}
		}
	}
	return b.String()
}

func formatClaudePrompt(req *dto.ClaudeRequest) string {
	if req == nil {
		return ""
	}
	var b strings.Builder
	b.Grow(512)
	if system := formatClaudeSystem(req); system != "" {
		b.WriteString("[system] ")
		b.WriteString(system)
	}
	if req.Prompt != "" {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("[user] ")
		b.WriteString(req.Prompt)
	}
	for _, msg := range req.Messages {
		part := formatClaudeMessage(msg)
		if part == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(part)
	}
	return b.String()
}

func formatClaudeSystem(req *dto.ClaudeRequest) string {
	if req.System == nil {
		return ""
	}
	if req.IsStringSystem() {
		return req.GetStringSystem()
	}
	return formatClaudeMediaMessages(req.ParseSystem())
}

func formatClaudeMessage(msg dto.ClaudeMessage) string {
	var b strings.Builder
	b.WriteByte('[')
	b.WriteString(msg.Role)
	b.WriteString("] ")
	if msg.IsStringContent() {
		b.WriteString(msg.GetStringContent())
		return b.String()
	}
	media, err := msg.ParseContent()
	if err != nil || len(media) == 0 {
		if content := msg.GetStringContent(); content != "" {
			b.WriteString(content)
			return b.String()
		}
		return ""
	}
	b.WriteString(formatClaudeMediaMessages(media))
	return b.String()
}

func formatClaudeMediaMessages(media []dto.ClaudeMediaMessage) string {
	var b strings.Builder
	for _, part := range media {
		switch part.Type {
		case dto.ContentTypeText:
			b.WriteString(part.GetText())
		case "image":
			b.WriteString("[image]")
		case "document":
			b.WriteString("[document]")
		case "tool_use":
			b.WriteString("[tool_use")
			if part.Name != "" {
				b.WriteByte(':')
				b.WriteString(part.Name)
			}
			b.WriteByte(']')
			if part.Input != nil {
				if raw, err := common.Marshal(part.Input); err == nil {
					b.WriteByte(' ')
					b.Write(raw)
				}
			}
		case "tool_result":
			b.WriteString("[tool_result")
			if part.ToolUseId != "" {
				b.WriteByte(':')
				b.WriteString(part.ToolUseId)
			}
			b.WriteByte(']')
			if part.Content != nil {
				if raw, err := common.Marshal(part.Content); err == nil {
					b.WriteByte(' ')
					b.Write(raw)
				}
			}
		case "thinking":
			b.WriteString("[thinking]")
		case "":
			if text := part.GetText(); text != "" {
				b.WriteString(text)
			}
		default:
			b.WriteByte('[')
			b.WriteString(part.Type)
			b.WriteByte(']')
		}
	}
	return b.String()
}

// ============================================================
// extractCompletionText: 从 middleware 缓存的响应 buffer 里解析 completion 文本
// ============================================================

// extractCompletionText 区分 stream 和 non-stream 两种场景：
//   - stream: SSE 格式，逐行 data: {...} JSON，累加 delta.content
//   - non-stream: 单条 JSON 响应，提取 choices[0].message.content
func extractCompletionText(ctx *gin.Context, relayInfo *relaycommon.RelayInfo) string {
	v, exists := ctx.Get(promptArchiveBufKey)
	if !exists {
		return ""
	}
	buf, ok := v.(*bytes.Buffer)
	if !ok || buf == nil || buf.Len() == 0 {
		return ""
	}
	body := buf.Bytes()

	if relayInfo.IsStream {
		return parseSSECompletionText(body)
	}
	return parseJSONCompletionText(body)
}

// parseSSECompletionText 从 SSE 流字节里抠出 completion 文本
// 格式: 每条 event 形如:
//
//	data: {"choices":[{"delta":{"content":"Hello"}}]}\n\n
//	data: {"choices":[{"delta":{"content":" world"}}]}\n\n
//	data: [DONE]\n\n
func parseSSECompletionText(body []byte) string {
	var b strings.Builder
	b.Grow(len(body) / 2)
	lines := bytes.Split(body, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[5:])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		// 兼容 OpenAI / Claude SSE 两种 JSON 结构
		// OpenAI: choices[0].delta.content (string)
		// Claude: type=content_block_delta, delta.text (string)
		var chunk map[string]any
		if err := common.Unmarshal(payload, &chunk); err != nil {
			continue
		}
		if text := pickTextFromOpenAIChunk(chunk); text != "" {
			b.WriteString(text)
		} else if text := pickTextFromClaudeChunk(chunk); text != "" {
			b.WriteString(text)
		}
	}
	return b.String()
}

// pickTextFromOpenAIChunk: choices[0].delta.content
func pickTextFromOpenAIChunk(chunk map[string]any) string {
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		return ""
	}
	first, ok := choices[0].(map[string]any)
	if !ok {
		return ""
	}
	delta, ok := first["delta"].(map[string]any)
	if !ok {
		// 非 stream 形态可能是 message 而非 delta
		if msg, mok := first["message"].(map[string]any); mok {
			delta = msg
		} else {
			return ""
		}
	}
	if c, ok := delta["content"].(string); ok {
		return c
	}
	return ""
}

// pickTextFromClaudeChunk: type=content_block_delta, delta.text
func pickTextFromClaudeChunk(chunk map[string]any) string {
	t, _ := chunk["type"].(string)
	if t != "content_block_delta" {
		return ""
	}
	delta, ok := chunk["delta"].(map[string]any)
	if !ok {
		return ""
	}
	if txt, ok := delta["text"].(string); ok {
		return txt
	}
	return ""
}

// parseJSONCompletionText: 非 stream 单条 JSON
func parseJSONCompletionText(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var resp map[string]any
	if err := common.Unmarshal(body, &resp); err != nil {
		return ""
	}
	// OpenAI: choices[0].message.content
	if text := pickTextFromOpenAIChunk(resp); text != "" {
		return text
	}
	// Claude: content[0].text
	if cs, ok := resp["content"].([]any); ok && len(cs) > 0 {
		if first, ok := cs[0].(map[string]any); ok {
			if t, ok := first["text"].(string); ok {
				return t
			}
		}
	}
	return ""
}

// ============================================================
// 工具函数
// ============================================================

func mergeUniqueStrings(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	for _, s := range b {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

// detectLang 启发式语言检测：
//   - CJK 占比 ≥ 30% → "zh"
//   - 否则 → "en"
//
// 这里故意做得简单：精度由 P2 阶段引入 lingua-go 之类替代
func detectLang(s string) string {
	if s == "" {
		return ""
	}
	cjk := 0
	total := 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			total++
			if unicode.Is(unicode.Han, r) {
				cjk++
			}
		}
	}
	if total == 0 {
		return ""
	}
	if cjk*10 >= total*3 {
		return "zh"
	}
	return "en"
}
