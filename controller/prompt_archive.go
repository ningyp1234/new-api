// Package controller — prompt_archive: P0 D5 + D6 + D4-admin
//
// 三个用户层 API:
//
//	GET  /api/prompts/me                  → GetMyPrompts        分页拉取个人 prompt 历史
//	GET  /api/prompts/me/:id              → GetMyPromptDetail   看自己某条详情（含解密后 raw）
//	GET  /api/prompts/hot                 → GetHotPromptsForGroup 按用户所属 group 看本组热门
//	GET  /api/prompts/analytics           → GetMyPromptAnalytics 个人 prompt 多维统计
//	GET  /api/prompts/insights            → GetMyPromptInsights 输入质量 / 输出反馈 / skill 候选
//
// 一个管理员 API:
//
//	POST /api/prompts/admin/purge-raw     → AdminPurgeRawNow    手动触发 90 天清理（运维 / 紧急合规）
package controller

import (
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

// ============================================================
// D5: 个人 prompt 历史
// ============================================================

// GetMyPrompts 分页拉取自己的 prompt + completion 历史
//
// Query:
//
//	p / page_size: 分页，标准 newapi PageInfo
//	model_name:    可选，按模型过滤
//	start_timestamp / end_timestamp:  可选，时间窗
//	keyword:       可选，prompt_redacted / completion_redacted 模糊匹配
//	include_raw:   "1" / "true" → 列表里也带回解密的 raw（默认不带，省流量 + 默认更安全）
//
// 强制 user_id = c.GetInt("id")，不允许跨用户查询。
func GetMyPrompts(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	userId := c.GetInt("id")
	if userId <= 0 {
		common.ApiError(c, errUnauthorized())
		return
	}

	startTs, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTs, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	includeRaw := c.Query("include_raw") == "1" || c.Query("include_raw") == "true"

	rows, total, err := model.ListUserPromptsPaginated(model.PromptArchiveQuery{
		UserId:     userId,
		ModelName:  c.Query("model_name"),
		StartTime:  startTs,
		EndTime:    endTs,
		Keyword:    c.Query("keyword"),
		StartIdx:   pageInfo.GetStartIdx(),
		PageSize:   pageInfo.GetPageSize(),
		IncludeRaw: includeRaw,
	})
	if err != nil {
		common.ApiError(c, err)
		return
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(rows)
	common.ApiSuccess(c, pageInfo)
}

// GetMyPromptDetail 单条详情（含解密的 raw 字段）
// URL 参数 :id 是 prompt_archives.id
// 强制 ownership 校验：record.user_id 必须等于 c.GetInt("id")
func GetMyPromptDetail(c *gin.Context) {
	userId := c.GetInt("id")
	if userId <= 0 {
		common.ApiError(c, errUnauthorized())
		return
	}
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		common.ApiError(c, errBadRequest("invalid id"))
		return
	}
	rec, err := model.GetPromptArchiveByIdForUser(id, userId)
	if err != nil {
		// gorm.ErrRecordNotFound 也走 ApiError（前端按 success=false 处理 404 等价）
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, rec)
}

// GetMyPromptAnalytics 返回当前用户自己的 prompt 多维统计。
//
// Query:
//
//	since_days: 最近 N 天（默认 30，最大 365）
//	start_timestamp / end_timestamp: 可选，显式时间窗，优先级高于 since_days
//
// 统计只使用 prompt_archives 中的 redacted 文本、hash 和元数据，
// 不返回也不读取 raw 字段，适合放在个人审计页面顶部。
func GetMyPromptAnalytics(c *gin.Context) {
	userId := c.GetInt("id")
	if userId <= 0 {
		common.ApiError(c, errUnauthorized())
		return
	}

	startTs, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTs, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	if startTs <= 0 {
		sinceDaysText := c.Query("since_days")
		sinceDays, _ := strconv.Atoi(sinceDaysText)
		if sinceDaysText == "" {
			sinceDays = 30
		}
		if sinceDays > 365 {
			sinceDays = 365
		}
		if sinceDays > 0 {
			startTs = nowUnix() - int64(sinceDays)*86400
		}
	}

	result, err := model.GetUserPromptAnalytics(model.PromptAnalyticsQuery{
		UserId:    userId,
		StartTime: startTs,
		EndTime:   endTs,
	})
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, result)
}

// GetMyPromptInsights 返回当前用户的 prompt 能力提升洞察。
//
// Query:
//
//	since_days: 最近 N 天（默认 30，最大 365）
//	start_timestamp / end_timestamp: 可选，显式时间窗，优先级高于 since_days
//
// 该接口只读取脱敏文本，不返回 raw 原文，用于质量评价、输出反馈考核、
// 高频输入汇总和 skill 候选提炼。
func GetMyPromptInsights(c *gin.Context) {
	userId := c.GetInt("id")
	if userId <= 0 {
		common.ApiError(c, errUnauthorized())
		return
	}

	startTs, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTs, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	if startTs <= 0 {
		sinceDaysText := c.Query("since_days")
		sinceDays, _ := strconv.Atoi(sinceDaysText)
		if sinceDaysText == "" {
			sinceDays = 30
		}
		if sinceDays > 365 {
			sinceDays = 365
		}
		if sinceDays > 0 {
			startTs = nowUnix() - int64(sinceDays)*86400
		}
	}

	result, err := model.GetUserPromptQualityInsights(model.PromptAnalyticsQuery{
		UserId:    userId,
		StartTime: startTs,
		EndTime:   endTs,
	})
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, result)
}

// ============================================================
// D6: 部门 / 组维度热门 prompt
// ============================================================

// GetHotPromptsForGroup 返回当前用户所在 group 的 N 条热门 prompt（按 prompt_hash 出现次数）
//
// Query:
//
//	since_days: 整数，最近 N 天（默认 7，最大 30）
//	top:        整数，返回多少条（默认 10，最大 50）
//
// 鉴权：UserAuth — 任何登录用户都可以看自己 group 的热度榜，
// 但只能看 redacted（脱敏版）+ hash + count，看不到具体某条 raw。
func GetHotPromptsForGroup(c *gin.Context) {
	userId := c.GetInt("id")
	if userId <= 0 {
		common.ApiError(c, errUnauthorized())
		return
	}
	// 拿用户所在 group。c.GetString("group") 会被 UserAuth 中间件填进去。
	userGroup := c.GetString("group")
	if userGroup == "" {
		// 兜底从 DB 拉一次
		u, err := model.GetUserById(userId, false)
		if err == nil && u != nil {
			userGroup = u.Group
		}
	}
	if userGroup == "" {
		common.ApiSuccess(c, map[string]any{
			"group": "",
			"items": []any{},
			"hint":  "user has no group set, hot list is empty",
		})
		return
	}

	sinceDays, _ := strconv.Atoi(c.Query("since_days"))
	if sinceDays <= 0 {
		sinceDays = 7
	}
	if sinceDays > 30 {
		sinceDays = 30
	}
	topN, _ := strconv.Atoi(c.Query("top"))
	if topN <= 0 {
		topN = 10
	}
	if topN > 50 {
		topN = 50
	}

	sinceUnix := nowUnix() - int64(sinceDays)*86400
	rows, err := model.TopHotPromptsByGroup(userGroup, sinceUnix, topN)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, map[string]any{
		"group":      userGroup,
		"since_days": sinceDays,
		"top":        topN,
		"items":      rows,
	})
}

// ============================================================
// D4-admin: 手动触发 raw 字段清理
// ============================================================

// AdminPurgeRawNow 手动触发一次 PurgeRawFieldsBeyondRetention，给 root admin 用。
// 业务场景：紧急合规 / 用户主张被遗忘权 / 测试时验证清理逻辑。
//
// 受 RootAuth + SecureVerificationRequired 保护（同 H-5 删 logs 的等级）。
func AdminPurgeRawNow(c *gin.Context) {
	rows, elapsed, err := service.PurgeRawNow()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, map[string]any{
		"purged_rows":    rows,
		"elapsed_ms":     elapsed.Milliseconds(),
		"retention_days": common.PromptArchiveRetentionDays,
	})
}

// ============================================================
// 内部小工具
// ============================================================

func errUnauthorized() error       { return &simpleError{code: http.StatusUnauthorized, msg: "unauthorized"} }
func errBadRequest(m string) error { return &simpleError{code: http.StatusBadRequest, msg: m} }

type simpleError struct {
	code int
	msg  string
}

func (e *simpleError) Error() string { return e.msg }

func nowUnix() int64 {
	return common.GetTimestamp()
}
