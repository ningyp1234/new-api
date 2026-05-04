package service

import (
	"time"
	"strconv"
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

func ReturnPreConsumedQuota(c *gin.Context, relayInfo *relaycommon.RelayInfo) {
	if relayInfo.FinalPreConsumedQuota != 0 {
		logger.LogInfo(c, fmt.Sprintf("用户 %d 请求失败, 返还预扣费额度 %s", relayInfo.UserId, logger.FormatQuota(relayInfo.FinalPreConsumedQuota)))
		gopool.Go(func() {
			relayInfoCopy := *relayInfo

			err := PostConsumeQuota(&relayInfoCopy, -relayInfoCopy.FinalPreConsumedQuota, 0, false)
			if err != nil {
				common.SysLog("error return pre-consumed quota: " + err.Error())
			}
		})
	}
}

// PreConsumeQuota checks if the user has enough quota to pre-consume.
// It returns the pre-consumed quota if successful, or an error if not.
func PreConsumeQuota(c *gin.Context, preConsumedQuota int, relayInfo *relaycommon.RelayInfo) *types.NewAPIError {
	userQuota, err := model.GetUserQuota(relayInfo.UserId, false)
	if err != nil {
		return types.NewError(err, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
	}
	if userQuota <= 0 {
		return types.NewErrorWithStatusCode(fmt.Errorf("用户额度不足, 剩余额度: %s", logger.FormatQuota(userQuota)), types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
	}
	if userQuota-preConsumedQuota < 0 {
		return types.NewErrorWithStatusCode(fmt.Errorf("预扣费额度失败, 用户剩余额度: %s, 需要预扣费额度: %s", logger.FormatQuota(userQuota), logger.FormatQuota(preConsumedQuota)), types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
	}

	trustQuota := common.GetTrustQuota()

	relayInfo.UserQuota = userQuota
	if userQuota > trustQuota {
		// 用户额度充足，判断令牌额度是否充足
		if !relayInfo.TokenUnlimited {
			// 非无限令牌，判断令牌额度是否充足
			tokenQuota := c.GetInt("token_quota")
			if tokenQuota > trustQuota {
				// 令牌额度充足，信任令牌
				preConsumedQuota = 0
				logger.LogInfo(c, fmt.Sprintf("用户 %d 剩余额度 %s 且令牌 %d 额度 %d 充足, 信任且不需要预扣费", relayInfo.UserId, logger.FormatQuota(userQuota), relayInfo.TokenId, tokenQuota))
			}
		} else {
			// SECURITY (H-4): trust path bypasses pre-consume; cap concurrent
			// in-flight calls per user to prevent overdraft via burst concurrency.
			// 用 redis INCR + TTL 实现 sliding window.
			if err := acquireTrustInflightSlot(c, relayInfo.UserId); err != nil {
				return types.NewErrorWithStatusCode(err, types.ErrorCodeInsufficientUserQuota, http.StatusTooManyRequests, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
			}
			preConsumedQuota = 0
			logger.LogInfo(c, fmt.Sprintf("用户 %d 额度充足且为无限额度令牌, 信任且不需要预扣费 (inflight slot acquired)", relayInfo.UserId))
		}
	}

	if preConsumedQuota > 0 {
		err := PreConsumeTokenQuota(relayInfo, preConsumedQuota)
		if err != nil {
			return types.NewErrorWithStatusCode(err, types.ErrorCodePreConsumeTokenQuotaFailed, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
		err = model.DecreaseUserQuota(relayInfo.UserId, preConsumedQuota, false)
		if err != nil {
			return types.NewError(err, types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
		}
		logger.LogInfo(c, fmt.Sprintf("用户 %d 预扣费 %s, 预扣费后剩余额度: %s", relayInfo.UserId, logger.FormatQuota(preConsumedQuota), logger.FormatQuota(userQuota-preConsumedQuota)))
	}
	relayInfo.FinalPreConsumedQuota = preConsumedQuota
	return nil
}


// SECURITY (H-4): cap concurrent in-flight requests for users on the trust path.
// Default 20 concurrent requests per user; configurable via TRUST_INFLIGHT_LIMIT env.
// We use a redis counter with a 60s TTL ceiling so a crashed handler eventually
// frees its slot. Falls back to allow-all if redis is unavailable (graceful
// degradation; the audit logging still records the usage).
func acquireTrustInflightSlot(c *gin.Context, userId int) error {
	if !common.RedisEnabled || common.RDB == nil {
		return nil
	}
	limit := 20
	if v := common.GetEnvOrDefault("TRUST_INFLIGHT_LIMIT", "20"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	key := fmt.Sprintf("trust_inflight:%d", userId)
	ctx := c.Request.Context()
	count, err := common.RDB.Incr(ctx, key).Result()
	if err != nil {
		// graceful degradation
		return nil
	}
	if count == 1 {
		// First INCR — set safety TTL
		_ = common.RDB.Expire(ctx, key, 60*time.Second).Err()
	}
	// Decrement on response — best-effort via gin "after" hook
	c.Set("trust_inflight_release", true)
	c.Set("trust_inflight_user", userId)
	if int(count) > limit {
		// release immediately and reject
		_ = common.RDB.Decr(ctx, key).Err()
		return fmt.Errorf("超出并发限制 (max %d in-flight requests for trusted user)", limit)
	}
	return nil
}

// ReleaseTrustInflight should be called from the response cleanup path.
func ReleaseTrustInflight(c *gin.Context) {
	if !common.GetContextKeyBool(c, "trust_inflight_release") {
		return
	}
	uid := c.GetInt("trust_inflight_user")
	if uid <= 0 || common.RDB == nil {
		return
	}
	key := fmt.Sprintf("trust_inflight:%d", uid)
	_ = common.RDB.Decr(c.Request.Context(), key).Err()
}
