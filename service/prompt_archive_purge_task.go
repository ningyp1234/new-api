// Package service — prompt_archive_purge_task: P0 D4
//
// 后台 ticker，每天凌晨 3 点跑一次 model.PurgeRawFieldsBeyondRetention，
// 把超过 PROMPT_ARCHIVE_RETENTION_DAYS（默认 90 天）的 prompt_raw_enc /
// completion_raw_enc 字段清空。脱敏版（prompt_redacted / completion_redacted）
// 永久保留，长期统计分析仍可用。
//
// 设计要点：
//   - 只在 master node 启动，避免多节点重复清理（与 audit/codex 任务一致）
//   - sync.Once 保证全进程只起一个 worker
//   - atomic.Bool guard 防止上一轮还在跑、新一轮就触发的并发竞争
//   - 凌晨 3 点是经验值：晚高峰已过，备份窗口空闲，清理 IO 不影响业务
//   - PROMPT_ARCHIVE_ENABLED=false 时 task 仍启动（清理幂等无副作用），
//     但实际工作量为 0，因为表里就没有新数据
package service

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"

	"github.com/bytedance/gopkg/util/gopool"
)

const (
	// 凌晨几点跑（24 小时制）
	promptArchivePurgeHour = 3
	// 兜底定期检查间隔，万一夜间机器睡过头能尽早赶上
	promptArchivePurgeFallbackInterval = 1 * time.Hour
)

var (
	promptArchivePurgeOnce    sync.Once
	promptArchivePurgeRunning atomic.Bool
)

// StartPromptArchivePurgeTask 由 main.go 在初始化阶段调用一次。
// 内部 sync.Once 保证幂等。
func StartPromptArchivePurgeTask() {
	promptArchivePurgeOnce.Do(func() {
		if !common.IsMasterNode {
			return
		}
		gopool.Go(func() {
			logger.LogInfo(context.Background(),
				fmt.Sprintf("prompt_archive purge task started: schedule=daily@%02d:00 retention=%d days",
					promptArchivePurgeHour, common.PromptArchiveRetentionDays))

			// 程序启动时立即跑一次（如果机器昨晚没起、错过 3am，启动时补跑）
			runPromptArchivePurgeOnce()

			for {
				next := nextPurgeTime()
				wait := time.Until(next)
				logger.LogInfo(context.Background(),
					fmt.Sprintf("prompt_archive next purge at %s (in %s)",
						next.Format("2006-01-02 15:04:05"), wait.Round(time.Second)))
				timer := time.NewTimer(wait)
				<-timer.C
				runPromptArchivePurgeOnce()
			}
		})
	})
}

// nextPurgeTime 计算下一个 03:00（本地时区）。如果当前时间已过 3am，
// 取明天 3am；否则取今天 3am。
func nextPurgeTime() time.Time {
	now := time.Now()
	target := time.Date(now.Year(), now.Month(), now.Day(),
		promptArchivePurgeHour, 0, 0, 0, now.Location())
	if !target.After(now) {
		target = target.Add(24 * time.Hour)
	}
	return target
}

// runPromptArchivePurgeOnce 真正执行清理，带 atomic guard + panic 兜底。
func runPromptArchivePurgeOnce() {
	if !promptArchivePurgeRunning.CompareAndSwap(false, true) {
		logger.LogInfo(context.Background(),
			"prompt_archive purge skipped: previous run still in progress")
		return
	}
	defer promptArchivePurgeRunning.Store(false)
	defer func() {
		if r := recover(); r != nil {
			common.SysError(fmt.Sprintf("prompt_archive purge panic: %v", r))
		}
	}()

	retention := common.PromptArchiveRetentionDays
	if retention <= 0 {
		// 0 或负数视为禁用清理（保留所有 raw 字段）
		logger.LogInfo(context.Background(),
			"prompt_archive retention <= 0, skip purge (raw fields will be kept indefinitely)")
		return
	}

	start := time.Now()
	rows, err := model.PurgeRawFieldsBeyondRetention(retention)
	elapsed := time.Since(start)
	if err != nil {
		common.SysError(fmt.Sprintf("prompt_archive purge failed: %v (elapsed=%s)", err, elapsed))
		return
	}
	logger.LogInfo(context.Background(),
		fmt.Sprintf("prompt_archive purge done: cleared %d records older than %d days, elapsed=%s",
			rows, retention, elapsed))
}

// PurgeRawNow 给 admin API 用的同步触发入口。返回清理行数 + 耗时。
// 不走 sync.Once，但有 atomic guard 防止和定时任务同时跑。
func PurgeRawNow() (int64, time.Duration, error) {
	if !common.IsMasterNode {
		return 0, 0, fmt.Errorf("purge can only be triggered on master node")
	}
	if !promptArchivePurgeRunning.CompareAndSwap(false, true) {
		return 0, 0, fmt.Errorf("purge already in progress, please retry later")
	}
	defer promptArchivePurgeRunning.Store(false)

	start := time.Now()
	rows, err := model.PurgeRawFieldsBeyondRetention(common.PromptArchiveRetentionDays)
	elapsed := time.Since(start)
	return rows, elapsed, err
}
