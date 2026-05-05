// SECURITY: Structured audit events table.
//
// This table is the single source of truth for security-relevant events:
//   - DLP hits (rule_id, action, tier, hash of match — not the raw match)
//   - Login failures and lockouts (user-level, not just redis counters)
//   - Lockout cleared events
//   - SSRF base_url block events
//   - Audit log delete events
//   - Channel key access events (root reading vendor keys)
//
// Design principles:
//   1. Never store raw sensitive content — only SHA-256 hashes of matches
//   2. Always async-write: never block user request on audit write failure
//   3. Schema is stable (event_type + detail JSON for evolution)
//   4. Indexed on (event_type, created_at, user_id) for fast Grafana queries
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/bytedance/gopkg/util/gopool"
)

// AuditEvent is a single structured security event.
type AuditEvent struct {
	Id         int64  `gorm:"primaryKey;autoIncrement"`
	CreatedAt  int64  `gorm:"bigint;index:idx_audit_created;index:idx_audit_event_created,priority:2"`
	EventType  string `gorm:"type:varchar(32);index:idx_audit_event_created,priority:1"`
	Severity   string `gorm:"type:varchar(16)"` // info / warn / high / critical
	UserId     int    `gorm:"index"`
	Username   string `gorm:"type:varchar(64);default:''"`
	TokenId    int    `gorm:"default:0"`
	ChannelId  int    `gorm:"default:0"`
	IP         string `gorm:"type:varchar(64);default:''"`
	UserAgent  string `gorm:"type:varchar(512);default:''"`
	RequestId  string `gorm:"type:varchar(64);index:idx_audit_request;default:''"`
	// DLP-specific fields (NULL for other event types)
	RuleId   string `gorm:"type:varchar(64);index;default:''"`
	Action   string `gorm:"type:varchar(16);default:''"` // block/mask/monitor
	Tier     int    `gorm:"default:0"`
	MatchHash string `gorm:"type:varchar(64);default:''"` // SHA-256 of match — never the raw value
	Position int    `gorm:"default:0"`
	Length   int    `gorm:"default:0"`
	// Free-form metadata (JSON string)
	Detail string `gorm:"type:text"`
}

// Event types — stable string constants (don't reuse, only add)
const (
	AuditEventDLPHit          = "dlp_hit"
	AuditEventLoginFail       = "login_fail"
	AuditEventLoginLockout    = "login_lockout"   // account got locked
	AuditEventLoginBlocked    = "login_blocked"   // request rejected because already locked
	AuditEventLoginSuccess    = "login_success"   // successful login (low-volume, useful for forensics)
	AuditEventLockoutCleared  = "lockout_cleared" // failure counter reset on success
	AuditEventSSRFBlock       = "ssrf_block"      // base_url SSRF protection rejected a config
	AuditEventLogDelete       = "log_delete"      // root deleted historical logs (mirrors H-5 audit trail)
	AuditEventChannelKeyRead  = "channel_key_read" // root viewed a vendor key
	AuditEventQuotaChange     = "quota_change"    // admin modified user quota
	AuditEventUserStatusChange = "user_status_change" // admin enabled/disabled/promoted user
)

const (
	SeverityInfo     = "info"
	SeverityWarn     = "warn"
	SeverityHigh     = "high"
	SeverityCritical = "critical"
)

// HashSensitive returns the SHA-256 hex of input — used to record DLP matches
// without storing the raw sensitive value.
func HashSensitive(s string) string {
	if s == "" {
		return ""
	}
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// RecordAuditEvent persists an event asynchronously. Failures are logged
// to SysLog but do not block the caller.
func RecordAuditEvent(evt *AuditEvent) {
	if evt == nil {
		return
	}
	if evt.CreatedAt == 0 {
		evt.CreatedAt = time.Now().Unix()
	}
	if evt.Severity == "" {
		evt.Severity = SeverityInfo
	}
	gopool.Go(func() {
		if LOG_DB == nil {
			return
		}
		if err := LOG_DB.Create(evt).Error; err != nil {
			common.SysLog("WARN: RecordAuditEvent failed: " + err.Error() +
				" evt_type=" + evt.EventType)
		}
	})
}

// CountAuditEventsByType returns counts for a given event type within a time window.
// Useful for unit tests / smoke tests.
func CountAuditEventsByType(eventType string, sinceUnix int64) (int64, error) {
	var count int64
	err := LOG_DB.Model(&AuditEvent{}).
		Where("event_type = ? AND created_at >= ?", eventType, sinceUnix).
		Count(&count).Error
	return count, err
}
