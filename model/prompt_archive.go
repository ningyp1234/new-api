// Package model — prompt_archive: 用户 prompt + 模型 completion 全文归档
//
// P0 D1 设计要点：
//
//  1. 双层存储（隐私）
//     PromptRawEnc / CompletionRawEnc       AES-GCM 加密的原文，90 天后由定时任务清空
//     PromptRedacted / CompletionRedacted   M-1 DLP 脱敏后的可见版本，永久保留用于聚合分析
//     透明加解密通过 GORM BeforeSave / AfterFind hook 完成，业务代码无感知
//
//  2. 跨数据库兼容（Rule 2: SQLite + MySQL >= 5.7.8 + PostgreSQL >= 9.6）
//     - 不使用 PG 原生分区表（其他两个 DB 不支持）
//     - 90 天 raw 字段过期清理用 GORM Updates ... WHERE created_at < ?
//     - JSON 字段统一用 TEXT 存储
//     - 所有索引通过 GORM tag 创建，避免裸 SQL
//
//  3. 异步写入（不阻塞主调用链路）
//     ArchivePrompt(ctx, payload) 内部用 RelayCtxGo 投递到 relayGoPool
//     失败只 SysLog 不 panic、不重试，避免雪崩传染主链路
//
//  4. 与现有体系的衔接点
//     - 复用 H-3 的 common.EncryptField / common.DecryptField
//     - 复用 M-1 的 service.SensitiveWordReplace 做脱敏（在 archiver middleware 调用，不在 model 层）
//     - 与 logs 表通过 RequestId 关联（不创建外键，避免跨库 / 性能问题）
//     - 保留 UserGroup 字段做轻量分组（暂不上独立 departments 表）
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/bytedance/gopkg/util/gopool"
	"gorm.io/gorm"
)

// PromptArchive 是单次 LLM 调用的 prompt + completion 全文归档记录。
//
// 一条记录对应 logs 表中的一条 type=2 (LogTypeConsume) 记录，通过 request_id 关联。
// 一次请求可能因 stream / 重试 / fallback 产生多次模型调用，本表只在 PostTextConsumeQuota
// 最终结算成功时写入一次（与 logs 表语义一致）。
type PromptArchive struct {
	Id        int64 `json:"id" gorm:"primaryKey;autoIncrement"`
	CreatedAt int64 `json:"created_at" gorm:"bigint;not null;index:idx_pa_created;index:idx_pa_user_created,priority:2;index:idx_pa_group_created,priority:2"`

	// 关联键
	RequestId string `json:"request_id" gorm:"type:varchar(64);index;not null"`
	UserId    int    `json:"user_id" gorm:"not null;index:idx_pa_user_created,priority:1"`
	Username  string `json:"username" gorm:"type:varchar(64);default:''"`
	UserGroup string `json:"user_group" gorm:"type:varchar(64);default:'';index:idx_pa_group_created,priority:1"`
	TokenId   int    `json:"token_id" gorm:"default:0"`
	ChannelId int    `json:"channel_id" gorm:"default:0;index"`
	ModelName string `json:"model_name" gorm:"type:varchar(64);default:'';index"`

	// ─── Prompt 双层存储 ────────────────────────────────────────
	// PromptRawEnc       AES-GCM 加密原文（90 天后由 PurgeRawFieldsBeyondRetention 清空）
	// PromptRedacted     DLP 脱敏后可见版本（永久保留）
	// PromptHash         SHA-256(normalized redacted)，用于复用次数统计 + 去重
	PromptRawEnc   string `json:"-" gorm:"type:text"`
	PromptRedacted string `json:"prompt_redacted" gorm:"type:text;not null"`
	PromptHash     string `json:"prompt_hash" gorm:"type:varchar(64);default:'';index"`
	PromptTokens   int    `json:"prompt_tokens" gorm:"default:0"`
	PromptLang     string `json:"prompt_lang" gorm:"type:varchar(8);default:''"`

	// ─── Completion 双层存储 ────────────────────────────────────
	CompletionRawEnc   string `json:"-" gorm:"type:text"`
	CompletionRedacted string `json:"completion_redacted" gorm:"type:text"`
	CompletionTokens   int    `json:"completion_tokens" gorm:"default:0"`

	// ─── DLP 标签（M-1 联动）─────────────────────────────────
	DlpHit        bool   `json:"dlp_hit" gorm:"default:false;index"`
	DlpCategories string `json:"dlp_categories" gorm:"type:varchar(255);default:''"` // CSV: "phone,idcard"

	// ─── 质量评分（P2 异步填充，P0 全部为空）────────────────────
	QualityScore      *int16 `json:"quality_score,omitempty" gorm:"index"`
	QualityJudgeModel string `json:"quality_judge_model,omitempty" gorm:"type:varchar(64);default:''"`
	JudgedAt          int64  `json:"judged_at,omitempty" gorm:"default:0"`
	ReuseCount        int    `json:"reuse_count" gorm:"default:0"`

	// ─── 调用元数据 ────────────────────────────────────────────
	UseTimeSec int  `json:"use_time_sec" gorm:"default:0"`
	IsStream   bool `json:"is_stream" gorm:"default:false"`
}

// TableName 显式表名（保持 prompt_archives 复数风格，与 audit_events 一致）
func (PromptArchive) TableName() string {
	return "prompt_archives"
}

// ============================================================
// GORM Hook：透明 AES-GCM 加密 / 解密
// ============================================================

// BeforeSave 在 INSERT / UPDATE 前对 raw 字段做 AES-GCM 加密。
// EncryptField 已实现幂等（已加密的值不会重复加密），可以安全在 Update 时重复触发。
func (p *PromptArchive) BeforeSave(tx *gorm.DB) error {
	if p.PromptRawEnc != "" {
		enc, err := common.EncryptField(p.PromptRawEnc)
		if err != nil {
			return err
		}
		p.PromptRawEnc = enc
	}
	if p.CompletionRawEnc != "" {
		enc, err := common.EncryptField(p.CompletionRawEnc)
		if err != nil {
			return err
		}
		p.CompletionRawEnc = enc
	}
	return nil
}

// AfterFind 在查询后对 raw 字段透明解密。
// DecryptField 对未加密的值直接 passthrough（迁移期 / 历史脏数据兼容）。
func (p *PromptArchive) AfterFind(tx *gorm.DB) error {
	if p.PromptRawEnc != "" {
		plain, err := common.DecryptField(p.PromptRawEnc)
		if err != nil {
			return err
		}
		p.PromptRawEnc = plain
	}
	if p.CompletionRawEnc != "" {
		plain, err := common.DecryptField(p.CompletionRawEnc)
		if err != nil {
			return err
		}
		p.CompletionRawEnc = plain
	}
	return nil
}

// ============================================================
// 公共写入 API（D2 抓取中间件调用）
// ============================================================

// ArchivePromptPayload 是 archiver middleware 给 ArchivePrompt 的入参。
// 所有字段都是已经在 middleware 层准备好的（含脱敏、hash 计算等），
// 本层只负责持久化。
type ArchivePromptPayload struct {
	RequestId string
	UserId    int
	Username  string
	UserGroup string
	TokenId   int
	ChannelId int
	ModelName string

	// 已在 middleware 层做好脱敏 + hash 计算
	PromptRaw      string
	PromptRedacted string
	PromptHash     string
	PromptTokens   int
	PromptLang     string

	CompletionRaw      string
	CompletionRedacted string
	CompletionTokens   int

	DlpHit        bool
	DlpCategories []string

	UseTimeSec int
	IsStream   bool
}

// ArchivePrompt 异步落盘。失败只 SysLog，不阻塞调用方。
// 内部用 gopool 投递到独立 worker，避免占用主请求 goroutine。
func ArchivePrompt(payload *ArchivePromptPayload) {
	if payload == nil {
		return
	}
	if !common.PromptArchiveEnabled {
		return
	}
	// 在投递前完成所有上下文相关的 snapshot，避免 worker 拿到 stale 数据
	rec := &PromptArchive{
		CreatedAt: time.Now().Unix(),
		RequestId: payload.RequestId,
		UserId:    payload.UserId,
		Username:  payload.Username,
		UserGroup: payload.UserGroup,
		TokenId:   payload.TokenId,
		ChannelId: payload.ChannelId,
		ModelName: payload.ModelName,

		PromptRawEnc:   payload.PromptRaw,
		PromptRedacted: payload.PromptRedacted,
		PromptHash:     payload.PromptHash,
		PromptTokens:   payload.PromptTokens,
		PromptLang:     payload.PromptLang,

		CompletionRawEnc:   payload.CompletionRaw,
		CompletionRedacted: payload.CompletionRedacted,
		CompletionTokens:   payload.CompletionTokens,

		DlpHit:        payload.DlpHit,
		DlpCategories: strings.Join(payload.DlpCategories, ","),

		UseTimeSec: payload.UseTimeSec,
		IsStream:   payload.IsStream,
	}

	gopool.Go(func() {
		if LOG_DB == nil {
			return
		}
		if err := LOG_DB.Create(rec).Error; err != nil {
			common.SysLog("WARN: ArchivePrompt failed: " + err.Error() +
				" request_id=" + payload.RequestId +
				" user_id=" + intToStr(payload.UserId))
		}
	})
}

// ComputePromptHash 把 redacted prompt 归一化（小写 + 折叠空白）后做 SHA-256，
// 用于复用次数统计：相同语义的 prompt 即便大小写 / 空白不同也能聚到一起。
//
// 这是 D2 archiver middleware 调用的辅助函数，放在 model 层是因为 hash
// 算法是表 schema 的一部分（一旦 P1 上 reuse_count 聚合就不能再改了）。
func ComputePromptHash(redacted string) string {
	if redacted == "" {
		return ""
	}
	normalized := strings.ToLower(strings.Join(strings.Fields(redacted), " "))
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// ============================================================
// 90 天 raw 字段过期清理（D1 实现，D6 接 cron）
// ============================================================

// PurgeRawFieldsBeyondRetention 把 created_at 早于 (now - retentionDays * 86400) 的记录
// 的 raw 加密字段清空。redacted + 元数据保留。
//
// 跨数据库兼容：用 GORM Updates，不依赖 PG 原生分区。
// 调用方应在 admin 后台或 cron 定时任务里调用，不在请求路径上调用。
func PurgeRawFieldsBeyondRetention(retentionDays int) (rowsAffected int64, err error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Unix() - int64(retentionDays)*86400
	res := LOG_DB.Model(&PromptArchive{}).
		Where("created_at < ? AND (prompt_raw_enc <> ? OR completion_raw_enc <> ?)",
			cutoff, "", "").
		Updates(map[string]interface{}{
			"prompt_raw_enc":     "",
			"completion_raw_enc": "",
		})
	if res.Error != nil {
		return 0, res.Error
	}
	if res.RowsAffected > 0 {
		common.SysLog("PromptArchive: purged raw fields on " +
			intToStr64(res.RowsAffected) + " records older than " +
			intToStr(retentionDays) + " days")
	}
	return res.RowsAffected, nil
}

// ============================================================
// 简单查询 helper（P0 看板用，P1 会扩展）
// ============================================================

// ListUserPromptsRecent 返回某用户最近 N 条记录（按时间倒序）。
// raw 字段会被 AfterFind 自动解密 — 调用方决定是否暴露给前端。
func ListUserPromptsRecent(userId int, limit int) ([]*PromptArchive, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var out []*PromptArchive
	err := LOG_DB.Where("user_id = ?", userId).
		Order("created_at DESC").
		Limit(limit).
		Find(&out).Error
	return out, err
}

// CountUserPromptsSince 给个人成长曲线 / 部门看板用。
func CountUserPromptsSince(userId int, sinceUnix int64) (int64, error) {
	var n int64
	err := LOG_DB.Model(&PromptArchive{}).
		Where("user_id = ? AND created_at >= ?", userId, sinceUnix).
		Count(&n).Error
	return n, err
}

// PromptArchiveQuery —— D5 个人历史 API 用的查询过滤条件。
// 所有字段都是可选，零值视为不过滤。
type PromptArchiveQuery struct {
	UserId     int    // 必填，user_id 维度强制收敛
	ModelName  string // 可选 model_name 精确匹配
	StartTime  int64  // 可选 created_at >= ?
	EndTime    int64  // 可选 created_at <= ?
	Keyword    string // 可选 prompt_redacted 模糊搜索
	StartIdx   int    // 分页 offset
	PageSize   int    // 分页 limit
	IncludeRaw bool   // 是否解密 raw 字段（true 时 AfterFind hook 自动解密）
}

// PromptAnalyticsQuery 描述 prompt 多维分析查询条件。
// 当前只开放个人维度，UserId 必填，避免统计接口成为跨用户数据出口。
type PromptAnalyticsQuery struct {
	UserId    int
	StartTime int64
	EndTime   int64
}

// PromptAnalyticsOverview 是个人 prompt 使用概览。
type PromptAnalyticsOverview struct {
	TotalCalls          int64   `json:"total_calls"`
	UniquePrompts       int64   `json:"unique_prompts"`
	ReusedPrompts       int64   `json:"reused_prompts"`
	DlpHits             int64   `json:"dlp_hits"`
	StreamCalls         int64   `json:"stream_calls"`
	PromptTokens        int64   `json:"prompt_tokens"`
	CompletionTokens    int64   `json:"completion_tokens"`
	AvgPromptTokens     float64 `json:"avg_prompt_tokens"`
	AvgCompletionTokens float64 `json:"avg_completion_tokens"`
	AvgUseTimeSec       float64 `json:"avg_use_time_sec"`
	AvgPromptChars      float64 `json:"avg_prompt_chars"`
	ReuseRate           float64 `json:"reuse_rate"`
	DlpRate             float64 `json:"dlp_rate"`
	StreamRate          float64 `json:"stream_rate"`
}

// PromptAnalyticsDimension 是模型、语言等维度的聚合行。
type PromptAnalyticsDimension struct {
	Name             string  `json:"name"`
	Count            int64   `json:"count"`
	DlpHits          int64   `json:"dlp_hits"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	AvgUseTimeSec    float64 `json:"avg_use_time_sec"`
}

// PromptAnalyticsBucket 是长度 / token 桶的统计行。
type PromptAnalyticsBucket struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

// PromptAnalyticsDaily 是每日趋势行。
type PromptAnalyticsDaily struct {
	Date             string `json:"date"`
	Count            int64  `json:"count"`
	UniquePrompts    int64  `json:"unique_prompts"`
	DlpHits          int64  `json:"dlp_hits"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
}

// PromptAnalyticsResult 是 /api/prompts/analytics 返回体。
type PromptAnalyticsResult struct {
	Overview      PromptAnalyticsOverview    `json:"overview"`
	ByModel       []PromptAnalyticsDimension `json:"by_model"`
	ByLanguage    []PromptAnalyticsDimension `json:"by_language"`
	ByDlpCategory []PromptAnalyticsBucket    `json:"by_dlp_category"`
	TokenBuckets  []PromptAnalyticsBucket    `json:"token_buckets"`
	LengthBuckets []PromptAnalyticsBucket    `json:"length_buckets"`
	DailyTrend    []PromptAnalyticsDaily     `json:"daily_trend"`
}

// PromptQualityInsightsResult 是“提示词能力提升”视角的聚合结果。
// 它不读取 raw 字段，只基于 prompt_redacted / completion_redacted 和元数据做本地可解释评分。
type PromptQualityInsightsResult struct {
	Overview         PromptQualityOverview   `json:"overview"`
	PromptIssues     []PromptInsightIssue    `json:"prompt_issues"`
	CompletionIssues []PromptInsightIssue    `json:"completion_issues"`
	HighFrequency    []PromptInsightHotInput `json:"high_frequency"`
	SkillCandidates  []PromptSkillCandidate  `json:"skill_candidates"`
	Recommendations  []string                `json:"recommendations"`
	ScoreBuckets     []PromptAnalyticsBucket `json:"score_buckets"`
}

type PromptQualityOverview struct {
	TotalCalls             int64   `json:"total_calls"`
	AvgPromptQualityScore  float64 `json:"avg_prompt_quality_score"`
	AvgCompletionScore     float64 `json:"avg_completion_score"`
	HighQualityPromptRate  float64 `json:"high_quality_prompt_rate"`
	WeakPromptRate         float64 `json:"weak_prompt_rate"`
	WeakCompletionRate     float64 `json:"weak_completion_rate"`
	ReusablePromptFamilies int64   `json:"reusable_prompt_families"`
	SkillCandidateCount    int64   `json:"skill_candidate_count"`
}

type PromptInsightIssue struct {
	Name   string `json:"name"`
	Count  int64  `json:"count"`
	Advice string `json:"advice"`
}

type PromptInsightHotInput struct {
	PromptHash         string  `json:"prompt_hash"`
	PromptRedacted     string  `json:"prompt_redacted"`
	HitCount           int64   `json:"hit_count"`
	AvgPromptScore     float64 `json:"avg_prompt_score"`
	AvgCompletionScore float64 `json:"avg_completion_score"`
	PromptTokens       int64   `json:"prompt_tokens"`
	CompletionTokens   int64   `json:"completion_tokens"`
	Suggestion         string  `json:"suggestion"`
	SkillReadiness     string  `json:"skill_readiness"`
}

type PromptSkillCandidate struct {
	Title              string   `json:"title"`
	RecommendedName    string   `json:"recommended_name"`
	PromptHash         string   `json:"prompt_hash"`
	HitCount           int64    `json:"hit_count"`
	AvgPromptScore     float64  `json:"avg_prompt_score"`
	AvgCompletionScore float64  `json:"avg_completion_score"`
	SamplePrompt       string   `json:"sample_prompt"`
	SkillGoal          string   `json:"skill_goal"`
	Inputs             []string `json:"inputs"`
	Workflow           []string `json:"workflow"`
	Guardrails         []string `json:"guardrails"`
	Evidence           string   `json:"evidence"`
}

// ListUserPromptsPaginated 给 D5 个人历史 API 用。
// 支持 model / time-range / keyword 多条件过滤 + 分页 + 总数。
func ListUserPromptsPaginated(q PromptArchiveQuery) ([]*PromptArchive, int64, error) {
	if q.UserId <= 0 {
		return nil, 0, nil
	}
	if q.PageSize <= 0 || q.PageSize > 100 {
		q.PageSize = 20
	}
	if q.StartIdx < 0 {
		q.StartIdx = 0
	}

	tx := LOG_DB.Model(&PromptArchive{}).Where("user_id = ?", q.UserId)
	if q.ModelName != "" {
		tx = tx.Where("model_name = ?", q.ModelName)
	}
	if q.StartTime > 0 {
		tx = tx.Where("created_at >= ?", q.StartTime)
	}
	if q.EndTime > 0 {
		tx = tx.Where("created_at <= ?", q.EndTime)
	}
	if q.Keyword != "" {
		// PG 用 ILIKE / MySQL 用 LOWER(...) LIKE / SQLite 默认 LIKE 大小写不敏感
		// 这里走简单跨库兼容：LIKE 配合 LOWER（PG 也支持）
		kw := "%" + q.Keyword + "%"
		tx = tx.Where("prompt_redacted LIKE ? OR completion_redacted LIKE ?", kw, kw)
	}

	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []*PromptArchive
	if err := tx.Order("created_at DESC").
		Offset(q.StartIdx).Limit(q.PageSize).
		Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	// 调用方决定是否对外暴露 raw 字段。Find 已经触发 AfterFind hook 自动解密。
	if !q.IncludeRaw {
		for _, r := range rows {
			r.PromptRawEnc = ""
			r.CompletionRawEnc = ""
		}
	}
	return rows, total, nil
}

// GetUserPromptAnalytics 汇总当前用户 prompt 归档的多维统计。
// 统计只使用 redacted 文本、hash 和元数据，不读取 raw 加密字段。
func GetUserPromptAnalytics(q PromptAnalyticsQuery) (*PromptAnalyticsResult, error) {
	if q.UserId <= 0 {
		return &PromptAnalyticsResult{}, nil
	}

	result := &PromptAnalyticsResult{}
	base := func() *gorm.DB {
		tx := LOG_DB.Model(&PromptArchive{}).Where("user_id = ?", q.UserId)
		if q.StartTime > 0 {
			tx = tx.Where("created_at >= ?", q.StartTime)
		}
		if q.EndTime > 0 {
			tx = tx.Where("created_at <= ?", q.EndTime)
		}
		return tx
	}

	type overviewRow struct {
		TotalCalls          int64
		DlpHits             int64
		StreamCalls         int64
		PromptTokens        int64
		CompletionTokens    int64
		AvgPromptTokens     float64
		AvgCompletionTokens float64
		AvgUseTimeSec       float64
		AvgPromptChars      float64
	}
	var o overviewRow
	if err := base().
		Select(`COUNT(*) AS total_calls,
			COALESCE(SUM(CASE WHEN dlp_hit THEN 1 ELSE 0 END), 0) AS dlp_hits,
			COALESCE(SUM(CASE WHEN is_stream THEN 1 ELSE 0 END), 0) AS stream_calls,
			COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
			COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
			COALESCE(AVG(prompt_tokens), 0) AS avg_prompt_tokens,
			COALESCE(AVG(completion_tokens), 0) AS avg_completion_tokens,
			COALESCE(AVG(use_time_sec), 0) AS avg_use_time_sec,
			COALESCE(AVG(LENGTH(prompt_redacted)), 0) AS avg_prompt_chars`).
		Scan(&o).Error; err != nil {
		return nil, err
	}
	result.Overview.TotalCalls = o.TotalCalls
	result.Overview.DlpHits = o.DlpHits
	result.Overview.StreamCalls = o.StreamCalls
	result.Overview.PromptTokens = o.PromptTokens
	result.Overview.CompletionTokens = o.CompletionTokens
	result.Overview.AvgPromptTokens = round2(o.AvgPromptTokens)
	result.Overview.AvgCompletionTokens = round2(o.AvgCompletionTokens)
	result.Overview.AvgUseTimeSec = round2(o.AvgUseTimeSec)
	result.Overview.AvgPromptChars = round2(o.AvgPromptChars)
	if o.TotalCalls > 0 {
		result.Overview.DlpRate = round4(float64(o.DlpHits) / float64(o.TotalCalls))
		result.Overview.StreamRate = round4(float64(o.StreamCalls) / float64(o.TotalCalls))
	}

	if err := base().
		Distinct("prompt_hash").
		Where("prompt_hash <> ?", "").
		Count(&result.Overview.UniquePrompts).Error; err != nil {
		return nil, err
	}
	result.Overview.ReusedPrompts = result.Overview.TotalCalls - result.Overview.UniquePrompts
	if result.Overview.ReusedPrompts < 0 {
		result.Overview.ReusedPrompts = 0
	}
	if result.Overview.TotalCalls > 0 {
		result.Overview.ReuseRate = round4(float64(result.Overview.ReusedPrompts) / float64(result.Overview.TotalCalls))
	}

	byModel, err := promptAnalyticsDimensions(base(), "model_name", 10)
	if err != nil {
		return nil, err
	}
	result.ByModel = byModel
	byLanguage, err := promptAnalyticsDimensions(base(), "prompt_lang", 10)
	if err != nil {
		return nil, err
	}
	result.ByLanguage = byLanguage

	daily, err := promptAnalyticsDaily(base())
	if err != nil {
		return nil, err
	}
	result.DailyTrend = daily

	buckets, err := promptAnalyticsBuckets(base())
	if err != nil {
		return nil, err
	}
	result.TokenBuckets = buckets.tokenBuckets
	result.LengthBuckets = buckets.lengthBuckets
	result.ByDlpCategory = buckets.dlpCategories

	return result, nil
}

// GetUserPromptQualityInsights 生成 prompt 输入质量、输出反馈质量和 skill 候选洞察。
func GetUserPromptQualityInsights(q PromptAnalyticsQuery) (*PromptQualityInsightsResult, error) {
	if q.UserId <= 0 {
		return &PromptQualityInsightsResult{}, nil
	}

	type row struct {
		PromptHash         string
		PromptRedacted     string
		CompletionRedacted string
		PromptTokens       int
		CompletionTokens   int
		DlpHit             bool
		UseTimeSec         int
		ModelName          string
	}
	tx := LOG_DB.Model(&PromptArchive{}).
		Select("prompt_hash, prompt_redacted, completion_redacted, prompt_tokens, completion_tokens, dlp_hit, use_time_sec, model_name").
		Where("user_id = ?", q.UserId)
	if q.StartTime > 0 {
		tx = tx.Where("created_at >= ?", q.StartTime)
	}
	if q.EndTime > 0 {
		tx = tx.Where("created_at <= ?", q.EndTime)
	}

	var rows []row
	if err := tx.Order("created_at DESC").Limit(5000).Find(&rows).Error; err != nil {
		return nil, err
	}

	result := &PromptQualityInsightsResult{
		PromptIssues:     []PromptInsightIssue{},
		CompletionIssues: []PromptInsightIssue{},
		HighFrequency:    []PromptInsightHotInput{},
		SkillCandidates:  []PromptSkillCandidate{},
		Recommendations:  []string{},
		ScoreBuckets: []PromptAnalyticsBucket{
			{Name: "优秀 80-100"},
			{Name: "可用 60-79"},
			{Name: "待改进 0-59"},
		},
	}
	if len(rows) == 0 {
		return result, nil
	}

	families := map[string]*promptInsightFamily{}
	promptIssueCounts := map[string]int64{}
	completionIssueCounts := map[string]int64{}

	var promptScoreSum, completionScoreSum int64
	var highQuality, weakPrompt, weakCompletion int64
	for _, r := range rows {
		promptEval := evaluatePromptQuality(r.PromptRedacted, r.PromptTokens, r.DlpHit)
		completionEval := evaluateCompletionQuality(r.PromptRedacted, r.CompletionRedacted, r.CompletionTokens, r.UseTimeSec)
		promptScoreSum += int64(promptEval.score)
		completionScoreSum += int64(completionEval.score)
		if promptEval.score >= 80 {
			highQuality++
			result.ScoreBuckets[0].Count++
		} else if promptEval.score >= 60 {
			result.ScoreBuckets[1].Count++
		} else {
			weakPrompt++
			result.ScoreBuckets[2].Count++
		}
		if completionEval.score < 60 {
			weakCompletion++
		}
		for _, issue := range promptEval.issues {
			promptIssueCounts[issue]++
		}
		for _, issue := range completionEval.issues {
			completionIssueCounts[issue]++
		}

		hash := r.PromptHash
		if hash == "" {
			hash = ComputePromptHash(r.PromptRedacted)
		}
		if hash == "" {
			continue
		}
		f := families[hash]
		if f == nil {
			f = &promptInsightFamily{hash: hash, samplePrompt: r.PromptRedacted}
			families[hash] = f
		}
		f.count++
		f.promptScoreSum += promptEval.score
		f.completionSum += completionEval.score
		f.promptTokens += int64(r.PromptTokens)
		f.completionTokens += int64(r.CompletionTokens)
		if len(r.PromptRedacted) > len(f.samplePrompt) && len(r.PromptRedacted) < 3000 {
			f.samplePrompt = r.PromptRedacted
		}
	}

	total := int64(len(rows))
	result.Overview.TotalCalls = total
	result.Overview.AvgPromptQualityScore = round2(float64(promptScoreSum) / float64(total))
	result.Overview.AvgCompletionScore = round2(float64(completionScoreSum) / float64(total))
	result.Overview.HighQualityPromptRate = round4(float64(highQuality) / float64(total))
	result.Overview.WeakPromptRate = round4(float64(weakPrompt) / float64(total))
	result.Overview.WeakCompletionRate = round4(float64(weakCompletion) / float64(total))

	result.PromptIssues = topPromptInsightIssues(promptIssueCounts, promptIssueAdvice, 8)
	result.CompletionIssues = topPromptInsightIssues(completionIssueCounts, completionIssueAdvice, 8)
	result.HighFrequency = buildPromptHotInputs(families, 10)
	result.SkillCandidates = buildPromptSkillCandidates(families, 6)
	result.Overview.SkillCandidateCount = int64(len(result.SkillCandidates))
	for _, f := range families {
		if f.count >= 2 {
			result.Overview.ReusablePromptFamilies++
		}
	}
	result.Recommendations = buildPromptRecommendations(result)
	return result, nil
}

// GetPromptArchiveByIdForUser 单条详情，强制 ownership 校验：
// 只有 record.UserId == userId 时才返回数据，否则返回 nil（防越权）。
// raw 字段经 AfterFind 自动解密。
func GetPromptArchiveByIdForUser(id int64, userId int) (*PromptArchive, error) {
	var rec PromptArchive
	err := LOG_DB.Where("id = ? AND user_id = ?", id, userId).First(&rec).Error
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// TopHotPromptsByGroup 用户组（"部门"）维度的 N 条最热 prompt（按 reuse_count 降序）。
// P0 时 reuse_count 全是 0，等 P1 增量更新逻辑上线后才有值。
// 暂以 prompt_hash 出现次数排序作为 P0 fallback。
func TopHotPromptsByGroup(userGroup string, sinceUnix int64, topN int) ([]struct {
	PromptHash     string
	PromptRedacted string
	HitCount       int64
}, error) {
	if topN <= 0 || topN > 50 {
		topN = 10
	}
	type row struct {
		PromptHash     string
		PromptRedacted string
		HitCount       int64
	}
	var rows []row
	// 用 GORM 通用 raw 查询；ANY_VALUE 在 PG 不存在 → 改用 MIN(prompt_redacted) 的方式跨库兼容
	err := LOG_DB.Model(&PromptArchive{}).
		Select("prompt_hash, MIN(prompt_redacted) AS prompt_redacted, COUNT(*) AS hit_count").
		Where("user_group = ? AND created_at >= ? AND prompt_hash <> ?", userGroup, sinceUnix, "").
		Group("prompt_hash").
		Order("hit_count DESC").
		Limit(topN).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]struct {
		PromptHash     string
		PromptRedacted string
		HitCount       int64
	}, len(rows))
	for i, r := range rows {
		out[i].PromptHash = r.PromptHash
		out[i].PromptRedacted = r.PromptRedacted
		out[i].HitCount = r.HitCount
	}
	return out, nil
}

func promptAnalyticsDimensions(tx *gorm.DB, column string, limit int) ([]PromptAnalyticsDimension, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	nameExpr := "COALESCE(NULLIF(" + column + ", ''), 'unknown')"
	var rows []PromptAnalyticsDimension
	err := tx.Select(nameExpr + ` AS name,
			COUNT(*) AS count,
			COALESCE(SUM(CASE WHEN dlp_hit THEN 1 ELSE 0 END), 0) AS dlp_hits,
			COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
			COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
			COALESCE(AVG(use_time_sec), 0) AS avg_use_time_sec`).
		Group(nameExpr).
		Order("count DESC").
		Limit(limit).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].AvgUseTimeSec = round2(rows[i].AvgUseTimeSec)
	}
	return rows, nil
}

func promptAnalyticsDaily(tx *gorm.DB) ([]PromptAnalyticsDaily, error) {
	dateExpr := promptArchiveDateExpr()
	var rows []PromptAnalyticsDaily
	err := tx.Select(dateExpr + ` AS date,
			COUNT(*) AS count,
			COUNT(DISTINCT CASE WHEN prompt_hash <> '' THEN prompt_hash ELSE NULL END) AS unique_prompts,
			COALESCE(SUM(CASE WHEN dlp_hit THEN 1 ELSE 0 END), 0) AS dlp_hits,
			COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
			COALESCE(SUM(completion_tokens), 0) AS completion_tokens`).
		Group(dateExpr).
		Order("date ASC").
		Scan(&rows).Error
	return rows, err
}

func promptArchiveDateExpr() string {
	switch common.LogSqlType {
	case common.DatabaseTypeMySQL:
		return "DATE_FORMAT(FROM_UNIXTIME(created_at), '%Y-%m-%d')"
	case common.DatabaseTypePostgreSQL:
		return "TO_CHAR(TO_TIMESTAMP(created_at), 'YYYY-MM-DD')"
	default:
		return "strftime('%Y-%m-%d', created_at, 'unixepoch')"
	}
}

type promptAnalyticsBucketResult struct {
	tokenBuckets  []PromptAnalyticsBucket
	lengthBuckets []PromptAnalyticsBucket
	dlpCategories []PromptAnalyticsBucket
}

func promptAnalyticsBuckets(tx *gorm.DB) (*promptAnalyticsBucketResult, error) {
	type row struct {
		PromptTokens   int
		PromptRedacted string
		DlpCategories  string
	}
	var rows []row
	if err := tx.Select("prompt_tokens, prompt_redacted, dlp_categories").Find(&rows).Error; err != nil {
		return nil, err
	}

	tokenBuckets := []PromptAnalyticsBucket{
		{Name: "0-500"},
		{Name: "501-2k"},
		{Name: "2k-8k"},
		{Name: "8k+"},
	}
	lengthBuckets := []PromptAnalyticsBucket{
		{Name: "0-200"},
		{Name: "201-800"},
		{Name: "801-2k"},
		{Name: "2k+"},
	}
	categoryCounts := map[string]int64{}
	for _, r := range rows {
		switch {
		case r.PromptTokens <= 500:
			tokenBuckets[0].Count++
		case r.PromptTokens <= 2000:
			tokenBuckets[1].Count++
		case r.PromptTokens <= 8000:
			tokenBuckets[2].Count++
		default:
			tokenBuckets[3].Count++
		}

		chars := len([]rune(r.PromptRedacted))
		switch {
		case chars <= 200:
			lengthBuckets[0].Count++
		case chars <= 800:
			lengthBuckets[1].Count++
		case chars <= 2000:
			lengthBuckets[2].Count++
		default:
			lengthBuckets[3].Count++
		}

		for _, c := range strings.Split(r.DlpCategories, ",") {
			c = strings.TrimSpace(c)
			if c != "" {
				categoryCounts[c]++
			}
		}
	}

	categories := make([]PromptAnalyticsBucket, 0, len(categoryCounts))
	for name, count := range categoryCounts {
		categories = append(categories, PromptAnalyticsBucket{Name: name, Count: count})
	}
	sortPromptAnalyticsBuckets(categories)

	return &promptAnalyticsBucketResult{
		tokenBuckets:  tokenBuckets,
		lengthBuckets: lengthBuckets,
		dlpCategories: categories,
	}, nil
}

func sortPromptAnalyticsBuckets(rows []PromptAnalyticsBucket) {
	for i := 1; i < len(rows); i++ {
		v := rows[i]
		j := i - 1
		for j >= 0 && rows[j].Count < v.Count {
			rows[j+1] = rows[j]
			j--
		}
		rows[j+1] = v
	}
}

type promptTextEvaluation struct {
	score  int
	issues []string
}

func evaluatePromptQuality(prompt string, promptTokens int, dlpHit bool) promptTextEvaluation {
	issues := []string{}
	text := strings.TrimSpace(prompt)
	lower := strings.ToLower(text)
	chars := utf8.RuneCountInString(text)

	hasTask := containsAny(lower,
		"请", "帮我", "需要", "分析", "生成", "总结", "提取", "翻译", "解释", "回答", "提问", "继续", "写", "列出", "对比", "检查", "优化", "改写", "评审", "设计", "制定", "规划", "判断", "诊断", "计算", "如何", "怎么",
		"review", "analyze", "summarize", "extract", "translate", "write", "compare", "design", "plan", "evaluate", "diagnose", "improve")
	hasContext := containsAny(lower,
		"背景", "上下文", "场景", "角色", "你是", "作为", "已有", "当前", "目标用户", "业务", "项目", "系统", "客户", "团队",
		"context", "background", "role", "scenario", "business", "project", "customer", "team")
	hasInput := containsAny(lower,
		"以下", "如下", "内容", "文本", "代码", "日志", "数据", "材料", "需求", "问题", "文档", "表格", "链接", "报错", "```", "「", "《",
		"input", "data", "content", "code", "log", "requirement", "document", "error")
	hasOutput := containsAny(lower,
		"格式", "输出", "表格", "json", "markdown", "列表", "步骤", "字段", "结构", "模板", "标题", "摘要", "结论", "建议", "一句话", "简短", "直接回答",
		"format", "output", "table", "schema", "bullet", "template", "summary", "conclusion")
	hasConstraints := containsAny(lower,
		"必须", "不要", "限制", "约束", "优先", "只", "字数", "风格", "边界", "不能", "避免", "保留", "忽略", "用中文", "英文",
		"must", "avoid", "constraint", "style", "only", "limit", "do not")
	hasCriteria := containsAny(lower,
		"标准", "验收", "判断", "评分", "质量", "准确", "完整", "风险", "依据", "原因", "检查清单", "成功", "失败",
		"criteria", "rubric", "quality", "accurate", "complete", "risk", "evidence", "checklist")
	hasWorkflow := containsAny(lower,
		"步骤", "分步骤", "逐步", "先", "然后", "最后", "流程", "分阶段", "拆解", "计划", "自检", "复核",
		"step", "workflow", "first", "then", "finally", "check")
	hasExample := containsAny(lower, "例如", "示例", "样例", "参考", "例子", "demo", "example", "sample")
	hasAudience := containsAny(lower,
		"面向", "给", "用户", "客户", "老板", "管理层", "研发", "产品", "运营", "销售", "受众", "读者",
		"audience", "stakeholder", "reader", "manager", "developer")

	score := 0
	if hasTask {
		score += 22
	} else {
		issues = append(issues, "任务目标不够明确")
	}
	if hasContext {
		score += 12
	} else if chars > 60 {
		issues = append(issues, "缺少业务背景或角色设定")
	}
	if hasInput {
		score += 12
	} else if chars > 80 {
		issues = append(issues, "缺少明确输入材料")
	}
	if hasOutput {
		score += 14
	} else {
		issues = append(issues, "缺少输出格式要求")
	}
	if hasConstraints {
		score += 12
	} else if chars > 40 {
		issues = append(issues, "缺少约束条件")
	}
	if hasCriteria {
		score += 10
	} else if chars > 120 {
		issues = append(issues, "缺少验收标准")
	}
	if hasWorkflow {
		score += 8
	} else if chars > 500 || promptTokens > 1200 {
		issues = append(issues, "复杂任务缺少拆解步骤")
	}
	if hasExample {
		score += 5
	}
	if hasAudience {
		score += 5
	}

	switch {
	case chars < 12:
		score -= 20
		issues = append(issues, "输入过短，模型需要猜测")
	case chars < 30 && !(hasTask && hasInput):
		score -= 10
		issues = append(issues, "信息量偏少")
	case chars >= 30 && chars <= 1200:
		score += 4
	}
	if (chars > 4000 || promptTokens > 8000) && !hasWorkflow {
		score -= 12
		issues = append(issues, "输入很长但未拆分任务")
	}
	if dlpHit {
		score -= 18
		issues = append(issues, "包含敏感信息")
	}
	return promptTextEvaluation{score: clampScore(score), issues: uniqueStrings(issues)}
}

func evaluateCompletionQuality(prompt string, completion string, completionTokens int, useTimeSec int) promptTextEvaluation {
	issues := []string{}
	text := strings.TrimSpace(completion)
	lower := strings.ToLower(text)
	chars := utf8.RuneCountInString(text)
	promptChars := utf8.RuneCountInString(strings.TrimSpace(prompt))
	promptLower := strings.ToLower(prompt)
	wantsBrief := containsAny(promptLower, "一句话", "简短", "简洁", "只回答", "直接回答", "不要解释", "brief", "concise", "short")

	if chars == 0 {
		return promptTextEvaluation{score: 0, issues: []string{"无有效输出"}}
	}

	score := 0
	if wantsBrief && chars > 0 {
		score += 20
	} else if chars >= 40 && completionTokens > 8 {
		score += 18
	} else {
		issues = append(issues, "输出过短")
	}

	relevance := promptCompletionRelevance(prompt, completion)
	switch {
	case relevance >= 0.28:
		score += 22
	case relevance >= 0.14:
		score += 14
	case wantsBrief:
		score += 16
	case promptChars < 80:
		score += 10
	default:
		score += 4
		issues = append(issues, "输出与输入主题关联偏弱")
	}

	if wantsBrief {
		score += 10
	} else if hasStructuredOutput(text) {
		score += 16
	} else if chars > 220 {
		issues = append(issues, "输出结构化不足")
	}
	if containsAny(lower,
		"建议", "步骤", "原因", "依据", "风险", "注意", "下一步", "可以", "应该", "清单", "方案", "结论", "示例",
		"recommend", "step", "because", "risk", "next", "example", "checklist", "solution") {
		score += 14
	} else if !wantsBrief {
		issues = append(issues, "缺少可执行建议或依据")
	}

	if promptChars > 120 {
		if chars >= promptChars/4 {
			score += 12
		} else if chars >= promptChars/8 {
			score += 7
		} else {
			score += 2
			issues = append(issues, "输出深度可能不足")
		}
	} else if chars >= 80 {
		score += 10
	}
	if containsAny(lower, "假设", "不确定", "需要确认", "无法判断", "取决于", "如果", "前提", "assume", "uncertain", "depends") {
		score += 8
	}
	if containsAny(lower, "完成", "如下", "总结", "结论", "建议", "结果", "done", "summary", "result") {
		score += 6
	}
	if containsAny(lower, "error", "failed", "unauthorized", "invalid", "抱歉", "无法", "不能", "出错", "失败", "权限不足") {
		score -= 25
		issues = append(issues, "疑似拒答或错误输出")
	}
	if useTimeSec > 60 {
		score -= 5
		issues = append(issues, "响应耗时较长")
	}
	return promptTextEvaluation{score: clampScore(score), issues: uniqueStrings(issues)}
}

func containsAny(s string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(s, strings.ToLower(needle)) || strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func clampScore(score int) int {
	if score < 0 {
		return 0
	}
	if score > 100 {
		return 100
	}
	return score
}

func hasStructuredOutput(text string) bool {
	return containsAny(text, "\n", "1.", "2.", "一、", "二、", "：", ":", "-", "*", "|", "```", "{", "}")
}

func promptCompletionRelevance(prompt string, completion string) float64 {
	promptTerms := extractSignalTerms(prompt)
	if len(promptTerms) == 0 {
		return 0
	}
	completionTerms := extractSignalTerms(completion)
	if len(completionTerms) == 0 {
		return 0
	}
	var hit int
	for term := range promptTerms {
		if completionTerms[term] {
			hit++
		}
	}
	return float64(hit) / float64(len(promptTerms))
}

func extractSignalTerms(text string) map[string]bool {
	terms := map[string]bool{}
	var ascii strings.Builder
	var cjk []rune
	flushASCII := func() {
		if ascii.Len() == 0 {
			return
		}
		token := strings.ToLower(ascii.String())
		ascii.Reset()
		if len(token) >= 3 && !asciiStopTerms[token] {
			terms[token] = true
		}
	}
	flushCJK := func() {
		if len(cjk) == 0 {
			return
		}
		if len(cjk) >= 2 {
			for i := 0; i+1 < len(cjk); i++ {
				terms[string(cjk[i:i+2])] = true
			}
		}
		if len(cjk) >= 4 {
			for i := 0; i+3 < len(cjk); i += 2 {
				terms[string(cjk[i:i+4])] = true
			}
		}
		cjk = cjk[:0]
	}
	for _, r := range text {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			flushCJK()
			ascii.WriteRune(r)
		case r >= 0x4e00 && r <= 0x9fff:
			flushASCII()
			cjk = append(cjk, r)
		default:
			flushASCII()
			flushCJK()
		}
	}
	flushASCII()
	flushCJK()
	for stop := range cjkStopTerms {
		delete(terms, stop)
	}
	return terms
}

var asciiStopTerms = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "this": true, "that": true,
	"you": true, "are": true, "can": true, "please": true, "need": true, "from": true,
}

var cjkStopTerms = map[string]bool{
	"请帮": true, "帮我": true, "一下": true, "这个": true, "那个": true, "进行": true,
	"需要": true, "如何": true, "怎么": true, "以下": true, "输出": true, "格式": true,
}

func uniqueStrings(items []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func promptIssueAdvice(name string) string {
	switch name {
	case "输入过短，模型需要猜测", "信息量偏少":
		return "补充业务背景、输入材料、期望结果和使用对象。"
	case "任务目标不够明确":
		return "把“要做什么”和“成功标准是什么”写成一句明确指令。"
	case "缺少业务背景或角色设定":
		return "增加角色、场景、已有约束和上下文，减少模型猜测。"
	case "缺少明确输入材料":
		return "明确贴出待处理文本、代码、日志、数据或需求范围。"
	case "缺少输出格式要求":
		return "指定表格、Markdown、JSON 字段或步骤结构，便于复用。"
	case "缺少约束条件":
		return "加入风格、边界、禁止项、字数、优先级或验收标准。"
	case "缺少验收标准":
		return "说明什么样的输出算好，例如准确性、完整性、风险覆盖和交付标准。"
	case "复杂任务缺少拆解步骤", "输入很长但未拆分任务":
		return "把长任务拆成理解、分析、生成、校对等阶段。"
	case "包含敏感信息":
		return "先脱敏再提问，用占位符替换手机号、证件、密钥等内容。"
	default:
		return "把这个问题沉淀为团队模板中的检查项。"
	}
}

func completionIssueAdvice(name string) string {
	switch name {
	case "无有效输出", "输出过短":
		return "在 prompt 中要求最小输出深度、列出依据，并给出示例。"
	case "输出与输入主题关联偏弱":
		return "在 prompt 中指定必须覆盖的关键点，并要求逐项回应。"
	case "输出深度可能不足":
		return "要求模型分步骤展开，并覆盖背景、推理、结论和下一步。"
	case "疑似拒答或错误输出":
		return "检查渠道、权限、模型能力，并在 prompt 中说明允许的替代方案。"
	case "输出结构化不足":
		return "指定固定结构，便于审阅、复制和团队复用。"
	case "缺少可执行建议或依据":
		return "要求输出包含原因、建议、风险、下一步和可直接执行的清单。"
	case "响应耗时较长":
		return "拆分任务或减少一次性上下文，避免单次调用过重。"
	default:
		return "结合高频 prompt 优化输出约束。"
	}
}

func topPromptInsightIssues(counts map[string]int64, advice func(string) string, limit int) []PromptInsightIssue {
	rows := make([]PromptInsightIssue, 0, len(counts))
	for name, count := range counts {
		rows = append(rows, PromptInsightIssue{Name: name, Count: count, Advice: advice(name)})
	}
	sortPromptInsightIssues(rows)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

func sortPromptInsightIssues(rows []PromptInsightIssue) {
	for i := 1; i < len(rows); i++ {
		v := rows[i]
		j := i - 1
		for j >= 0 && rows[j].Count < v.Count {
			rows[j+1] = rows[j]
			j--
		}
		rows[j+1] = v
	}
}

type promptInsightFamily struct {
	hash             string
	samplePrompt     string
	count            int64
	promptScoreSum   int
	completionSum    int
	promptTokens     int64
	completionTokens int64
}

func buildPromptHotInputs(families map[string]*promptInsightFamily, topN int) []PromptInsightHotInput {
	rows := make([]PromptInsightHotInput, 0, len(families))
	for _, f := range families {
		if f.count < 2 {
			continue
		}
		avgPrompt := round2(float64(f.promptScoreSum) / float64(f.count))
		avgCompletion := round2(float64(f.completionSum) / float64(f.count))
		readiness := "观察中"
		suggestion := "继续观察复用效果，积累更多样例。"
		if f.count >= 3 && avgPrompt >= 75 && avgCompletion >= 65 {
			readiness = "适合沉淀"
			suggestion = "高频且效果稳定，建议抽象为团队 skill。"
		} else if f.count >= 3 {
			readiness = "先模板化"
			suggestion = "使用频率高但质量不稳，先统一模板和输出格式。"
		}
		rows = append(rows, PromptInsightHotInput{
			PromptHash:         f.hash,
			PromptRedacted:     f.samplePrompt,
			HitCount:           f.count,
			AvgPromptScore:     avgPrompt,
			AvgCompletionScore: avgCompletion,
			PromptTokens:       f.promptTokens,
			CompletionTokens:   f.completionTokens,
			Suggestion:         suggestion,
			SkillReadiness:     readiness,
		})
	}
	sortPromptHotInputs(rows)
	if len(rows) > topN {
		rows = rows[:topN]
	}
	return rows
}

func sortPromptHotInputs(rows []PromptInsightHotInput) {
	for i := 1; i < len(rows); i++ {
		v := rows[i]
		j := i - 1
		for j >= 0 && (rows[j].HitCount < v.HitCount ||
			(rows[j].HitCount == v.HitCount && rows[j].AvgPromptScore < v.AvgPromptScore)) {
			rows[j+1] = rows[j]
			j--
		}
		rows[j+1] = v
	}
}

func buildPromptSkillCandidates(families map[string]*promptInsightFamily, limit int) []PromptSkillCandidate {
	hot := buildPromptHotInputs(families, 50)
	out := make([]PromptSkillCandidate, 0, limit)
	for _, h := range hot {
		if h.HitCount < 2 {
			continue
		}
		title := derivePromptSkillTitle(h.PromptRedacted)
		out = append(out, PromptSkillCandidate{
			Title:              title,
			RecommendedName:    title + " Skill",
			PromptHash:         h.PromptHash,
			HitCount:           h.HitCount,
			AvgPromptScore:     h.AvgPromptScore,
			AvgCompletionScore: h.AvgCompletionScore,
			SamplePrompt:       h.PromptRedacted,
			SkillGoal:          "把团队反复使用的提示词整理成稳定流程，降低重复提问成本并提升输出一致性。",
			Inputs:             []string{"业务背景", "待处理材料或问题", "期望输出格式", "约束条件和验收标准"},
			Workflow: []string{
				"先确认任务目标、使用场景和交付对象。",
				"补齐必要上下文，并把敏感信息替换为占位符。",
				"按固定结构生成结果，同时列出假设、风险和下一步建议。",
				"对输出做一次自检，确认是否满足格式、边界和验收标准。",
			},
			Guardrails: []string{
				"不在 prompt 中直接粘贴密钥、证件号、手机号等敏感信息。",
				"当上下文不足时先要求补充信息，而不是直接编造。",
				"对外发布前必须由业务负责人复核事实和合规风险。",
			},
			Evidence: "近段时间复用 " + intToStr64(h.HitCount) + " 次，输入质量均分 " +
				floatToScoreText(h.AvgPromptScore) + "，输出反馈均分 " + floatToScoreText(h.AvgCompletionScore) + "。",
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

func derivePromptSkillTitle(prompt string) string {
	text := strings.TrimSpace(prompt)
	text = strings.TrimPrefix(text, "[user]")
	text = strings.TrimPrefix(text, "[system]")
	text = strings.TrimSpace(text)
	for _, sep := range []string{"\n", "。", "？", "?", "：", ":"} {
		if idx := strings.Index(text, sep); idx > 0 {
			text = text[:idx]
			break
		}
	}
	rs := []rune(text)
	if len(rs) > 18 {
		text = string(rs[:18])
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "高频 Prompt"
	}
	return text
}

func buildPromptRecommendations(result *PromptQualityInsightsResult) []string {
	out := []string{}
	if len(result.PromptIssues) > 0 {
		out = append(out, "优先改进输入问题："+result.PromptIssues[0].Name+"。"+result.PromptIssues[0].Advice)
	}
	if len(result.CompletionIssues) > 0 {
		out = append(out, "优先改进输出问题："+result.CompletionIssues[0].Name+"。"+result.CompletionIssues[0].Advice)
	}
	if result.Overview.SkillCandidateCount > 0 {
		out = append(out, "已有可沉淀的高频场景，建议从 skill 候选中挑选 1-2 个先做团队模板试点。")
	}
	if result.Overview.WeakPromptRate > 0.3 {
		out = append(out, "弱 prompt 占比较高，建议推广统一结构：背景、目标、输入、输出格式、约束、验收标准。")
	}
	if len(out) == 0 {
		out = append(out, "整体使用质量较稳定，可以开始沉淀团队最佳实践并建立示例库。")
	}
	return out
}

func floatToScoreText(v float64) string {
	n := int64(v + 0.5)
	return intToStr64(n)
}

func round2(v float64) float64 {
	if v <= 0 {
		return 0
	}
	return float64(int64(v*100+0.5)) / 100
}

func round4(v float64) float64 {
	if v <= 0 {
		return 0
	}
	return float64(int64(v*10000+0.5)) / 10000
}

// ============================================================
// 内部小工具（避免在多处依赖 strconv，集中在此）
// ============================================================

func intToStr(n int) string {
	return intToStr64(int64(n))
}

func intToStr64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
