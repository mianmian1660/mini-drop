// ============================================================
// server/storage_status.go — 存储压力检测、鉴权接口与指标
// ============================================================
// 后台每 60 秒读取一次磁盘快照并更新 Prometheus 指标；状态变化时立即
// 写结构化日志，warning/critical/emergency 持续存在时每 15 分钟重复一次，
// normal 状态不重复刷日志。
//
// GET /api/v1/storage/status（需登录）返回最新磁盘快照；statfs 失败时
// 返回 level=unknown、collection_allowed=false，不伪造剩余空间。
// ============================================================

package server

import (
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const (
	storageMonitorInterval   = 60 * time.Second
	storageRepeatLogInterval = 15 * time.Minute
)

// 后台检测状态（单实例内存态，无需持久化）。
type storageMonitorState struct {
	mu            sync.Mutex
	lastLevel     StoragePressureLevel
	lastRepeatLog time.Time
}

// startStorageMonitor 启动后台磁盘压力检测（每 60 秒一次）。
func (s *APIServer) startStorageMonitor() {
	s.checkStoragePressure()
	ticker := time.NewTicker(storageMonitorInterval)
	defer ticker.Stop()
	for range ticker.C {
		s.checkStoragePressure()
	}
}

// checkStoragePressure 执行一次检测：更新指标 + 按规则写日志。
func (s *APIServer) checkStoragePressure() {
	snap := s.currentStorageSnapshot()
	updateStorageMetrics(snap)

	s.storageState.mu.Lock()
	defer s.storageState.mu.Unlock()

	now := time.Now()
	first := s.storageState.lastLevel == ""
	changed := !first && s.storageState.lastLevel != snap.Level

	if first || changed {
		s.logStorageLevel(snap, changed)
		s.storageState.lastLevel = snap.Level
		s.storageState.lastRepeatLog = now
		return
	}
	// 未变化：warning/critical/emergency 每 15 分钟重复一次；normal/unknown 不重复。
	if snap.Level != StoragePressureNormal && snap.Level != StoragePressureUnknown &&
		now.Sub(s.storageState.lastRepeatLog) >= storageRepeatLogInterval {
		s.logStorageLevel(snap, false)
		s.storageState.lastRepeatLog = now
	}
}

func (s *APIServer) logStorageLevel(snap StorageDiskSnapshot, changed bool) {
	fields := []zap.Field{
		zap.String("path", snap.Path),
		zap.Uint64("total_bytes", snap.TotalBytes),
		zap.Uint64("available_bytes", snap.AvailableBytes),
		zap.Uint64("used_bytes", snap.UsedBytes),
		zap.String("level", string(snap.Level)),
		zap.Bool("collection_allowed", snap.CollectionAllowed),
		zap.Bool("state_changed", changed),
	}
	switch snap.Level {
	case StoragePressureNormal:
		s.Logger.Info("存储压力恢复正常", fields...)
	case StoragePressureWarning:
		s.Logger.Warn("存储压力警告：服务端存储空间偏低", fields...)
	case StoragePressureCritical:
		s.Logger.Error("存储压力严重：服务端存储空间不足，请尽快清理", fields...)
	case StoragePressureEmergency:
		s.Logger.Error("存储压力紧急：服务端存储空间低于保护阈值，新采集已暂停", fields...)
	case StoragePressureUnknown:
		s.Logger.Warn("存储压力未知：无法读取磁盘状态，新采集已暂停", fields...)
	}
}

// StorageStatus 返回最新磁盘快照（每次请求实时读取一次 statfs）。
// GET /api/v1/storage/status（鉴权分组内）
func (s *APIServer) StorageStatus(c *gin.Context) {
	s.RespondOK(c, s.currentStorageSnapshot())
}
