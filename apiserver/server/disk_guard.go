package server

import (
	"fmt"
	"syscall"
	"time"

	"github.com/mini-drop/apiserver/config"
)

// 存储压力等级。判定基于受监控文件系统的剩余可用字节数：
//
//	normal    >= warning_free_bytes
//	warning   < warning_free_bytes 且 >= critical_free_bytes
//	critical  < critical_free_bytes 且 >= min_free_bytes
//	emergency < min_free_bytes
//	unknown   statfs 读取失败（fail-closed：不允许新采集）
//
// 只有 emergency / unknown 才拒绝新采集；warning / critical 只告警。
type StoragePressureLevel string

const (
	StoragePressureNormal    StoragePressureLevel = "normal"
	StoragePressureWarning   StoragePressureLevel = "warning"
	StoragePressureCritical  StoragePressureLevel = "critical"
	StoragePressureEmergency StoragePressureLevel = "emergency"
	StoragePressureUnknown   StoragePressureLevel = "unknown"
)

// 采集入口来源（用于拒收计数 label）。
const (
	CollectionSourceOneShot    = "one_shot"
	CollectionSourceRetry      = "retry"
	CollectionSourceScheduled  = "scheduled"
	CollectionSourceContinuous = "continuous"
	CollectionSourceCompactor  = "compactor"
)

// StorageDiskSnapshot 一次磁盘状态快照。statfs 失败时 Level=unknown、
// CollectionAllowed=false，且不伪造 total/available/used（保持 0）。
type StorageDiskSnapshot struct {
	Path              string               `json:"path"`
	TotalBytes        uint64               `json:"total_bytes"`
	AvailableBytes    uint64               `json:"available_bytes"`
	UsedBytes         uint64               `json:"used_bytes"`
	Level             StoragePressureLevel `json:"level"`
	CollectionAllowed bool                 `json:"collection_allowed"`
	CheckedAt         time.Time            `json:"checked_at"`
}

const (
	storageMinFreeBytesDefault      uint64 = 512 << 20
	storageWarningFreeBytesDefault  uint64 = 2 << 30
	storageCriticalFreeBytesDefault uint64 = 1 << 30
)

// readStorageDiskSnapshot 是可被单元测试替换的 statfs 读取函数。
// 返回 (total, available, used) 字节数。
var readStorageDiskSnapshot = func(path string) (total uint64, available uint64, used uint64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, 0, err
	}
	bsize := uint64(stat.Bsize)
	total = uint64(stat.Blocks) * bsize
	available = uint64(stat.Bavail) * bsize
	if uint64(stat.Blocks) > uint64(stat.Bfree) {
		used = (uint64(stat.Blocks) - uint64(stat.Bfree)) * bsize
	}
	return total, available, used, nil
}

// diskGuardConfig 返回 (path, warning, critical, minimum)。
func diskGuardConfig(cfg *config.Config) (string, uint64, uint64, uint64) {
	path := "/tmp"
	warning, critical, minimum := storageWarningFreeBytesDefault, storageCriticalFreeBytesDefault, storageMinFreeBytesDefault
	if cfg != nil {
		if cfg.StorageDisk.Path != "" {
			path = cfg.StorageDisk.Path
		}
		if cfg.StorageDisk.WarningFreeBytes != 0 {
			warning = cfg.StorageDisk.WarningFreeBytes
		}
		if cfg.StorageDisk.CriticalFreeBytes != 0 {
			critical = cfg.StorageDisk.CriticalFreeBytes
		}
		if cfg.StorageDisk.MinFreeBytes != 0 {
			minimum = cfg.StorageDisk.MinFreeBytes
		}
	}
	return path, warning, critical, minimum
}

// currentStorageSnapshot 读取一次磁盘快照。statfs 失败时返回 level=unknown、
// collection_allowed=false，不伪造剩余空间。
func (s *APIServer) currentStorageSnapshot() StorageDiskSnapshot {
	path, warning, critical, minimum := diskGuardConfig(s.Config)
	snap := StorageDiskSnapshot{
		Path:              path,
		Level:             StoragePressureUnknown,
		CollectionAllowed: false,
		CheckedAt:         time.Now(),
	}
	total, avail, used, err := readStorageDiskSnapshot(path)
	if err != nil {
		return snap
	}
	snap.TotalBytes = total
	snap.AvailableBytes = avail
	snap.UsedBytes = used
	switch {
	case avail < minimum:
		snap.Level = StoragePressureEmergency
		snap.CollectionAllowed = false
	case avail < critical:
		snap.Level = StoragePressureCritical
		snap.CollectionAllowed = true
	case avail < warning:
		snap.Level = StoragePressureWarning
		snap.CollectionAllowed = true
	default:
		snap.Level = StoragePressureNormal
		snap.CollectionAllowed = true
	}
	return snap
}

// canStartCollection 保留采集入口的硬保护：只有 emergency/unknown 才拒收，
// warning/critical 放行。拒收时按来源计数（mini_drop_collection_rejected_low_disk_total）。
// 阶段五在 emergency/unknown 之上叠加 required_free 动态门槛：
// available < required_free 时同样拒收（new/retry/scheduled/continuous）。
func (s *APIServer) canStartCollection(source string) (bool, string, StorageDiskSnapshot) {
	snap := s.currentStorageSnapshot()
	if snap.Level == StoragePressureEmergency || snap.Level == StoragePressureUnknown {
		incCollectionRejectedLowDisk(source)
		var message string
		switch snap.Level {
		case StoragePressureEmergency:
			_, _, _, minimum := diskGuardConfig(s.Config)
			message = fmt.Sprintf("采集被拒绝：%s 可用空间 %d bytes，低于保护阈值 %d bytes；请清理服务器磁盘后重试", snap.Path, snap.AvailableBytes, minimum)
		default:
			message = fmt.Sprintf("采集被拒绝：无法检查采集磁盘 %s 的状态（statfs 失败），已暂停新采集", snap.Path)
		}
		return false, message, snap
	}
	// 阶段五：required_free 动态门槛（低于门槛拒绝，防止 compaction/新采集
	// 挤占保护线；恢复由 60s 检测状态机控制）。
	if ok, reason, _ := s.collectionCapacityOK(source); !ok {
		return false, reason, snap
	}
	return true, "", snap
}
