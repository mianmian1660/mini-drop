package server

import (
	"fmt"
	"syscall"

	"github.com/mini-drop/apiserver/config"
)

const storageMinFreeBytesDefault uint64 = 1 << 30

// storageFreeBytes is replaceable by unit tests. Statfs measures the filesystem
// visible to apiserver, which is the relevant root-volume pressure signal.
var storageFreeBytes = func(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}

func diskGuardConfig(cfg *config.Config) (string, uint64) {
	path, minimum := "/tmp", storageMinFreeBytesDefault
	if cfg != nil {
		if cfg.StorageDisk.Path != "" {
			path = cfg.StorageDisk.Path
		}
		if cfg.StorageDisk.MinFreeBytes != 0 {
			minimum = cfg.StorageDisk.MinFreeBytes
		}
	}
	return path, minimum
}

func (s *APIServer) canStartCollection() (bool, string, uint64, uint64) {
	path, minimum := diskGuardConfig(s.Config)
	free, err := storageFreeBytes(path)
	if err != nil {
		// Failing closed is deliberate: an unknown filesystem must not accept a
		// capture that can make a nearly-full host unrecoverable.
		return false, fmt.Sprintf("无法检查采集磁盘 %s: %v", path, err), 0, minimum
	}
	if free < minimum {
		return false, fmt.Sprintf("采集被拒绝：%s 可用空间 %d bytes，低于保护阈值 %d bytes；可删除历史原始数据后重试", path, free, minimum), free, minimum
	}
	return true, "", free, minimum
}
