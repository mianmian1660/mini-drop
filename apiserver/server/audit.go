package server

import (
	"encoding/json"
	"errors"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/mini-drop/apiserver/model"
)

const (
	TaskStatusCreated   = 0
	TaskStatusRunning   = 1
	TaskStatusDone      = 2
	TaskStatusFailed    = 3
	TaskStatusUploading = 4
	TaskStatusCanceled  = 5
)

// ErrTaskStatusStale 表示调用方拿到的 task.Status 在写入前已经被别的路径
// 并发改掉了（notify 回调 / outbox 分发器 / 状态巡检器三者都可能改状态）。
// 这不是一次真正的失败，而是"这次迁移被别人抢先做过了，无需重复处理"。
var ErrTaskStatusStale = errors.New("任务状态已被并发修改，跳过本次迁移")

func (s *APIServer) transitionTaskStatus(task *model.HotmethodTask, toStatus int, reason string, source string, extra map[string]interface{}) error {
	if task == nil {
		return nil
	}
	if reason == "" {
		reason = "状态迁移"
	}
	if source == "" {
		source = "apiserver"
	}

	fromStatus := task.Status
	updates := map[string]interface{}{
		"status":      toStatus,
		"status_info": reason,
	}
	for k, v := range extra {
		updates[k] = v
	}

	stale := false
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(task).Where("status = ?", fromStatus).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			stale = true
			return nil
		}
		sequence := nextTaskEventSequenceTx(tx, task.TID)
		payload, _ := json.Marshal(extra)
		return tx.Create(&model.TaskStatusEvent{
			TID:          task.TID,
			FromStatus:   fromStatus,
			ToStatus:     toStatus,
			Reason:       reason,
			Source:       source,
			Sequence:     sequence,
			SourceModule: source,
			Payload:      payload,
			CreatedAt:    time.Now(),
		}).Error
	})
	if stale {
		s.Logger.Warn("任务状态迁移跳过：目标状态已被并发修改",
			zap.String("tid", task.TID),
			zap.Int("expected_from_status", fromStatus),
			zap.Int("to_status", toStatus),
			zap.String("reason", reason),
			zap.String("source", source),
		)
		return ErrTaskStatusStale
	}
	if err != nil {
		s.Logger.Error("记录任务状态迁移失败",
			zap.String("tid", task.TID),
			zap.Int("from_status", fromStatus),
			zap.Int("to_status", toStatus),
			zap.String("reason", reason),
			zap.Error(err),
		)
		return err
	}

	task.Status = toStatus
	task.StatusInfo = reason
	s.Logger.Info("任务状态迁移",
		zap.String("tid", task.TID),
		zap.Int("from_status", fromStatus),
		zap.Int("to_status", toStatus),
		zap.String("reason", reason),
		zap.String("source", source),
	)
	return nil
}

func (s *APIServer) recordTaskStatusEvent(tid string, fromStatus int, toStatus int, reason string, source string) {
	if tid == "" {
		return
	}
	if reason == "" {
		reason = "状态记录"
	}
	if source == "" {
		source = "apiserver"
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		return tx.Create(&model.TaskStatusEvent{
			TID:          tid,
			FromStatus:   fromStatus,
			ToStatus:     toStatus,
			Reason:       reason,
			Source:       source,
			Sequence:     nextTaskEventSequenceTx(tx, tid),
			SourceModule: source,
			CreatedAt:    time.Now(),
		}).Error
	})
	if err != nil {
		s.Logger.Warn("记录任务状态事件失败", zap.String("tid", tid), zap.Error(err))
	}
}

func nextTaskEventSequenceTx(tx *gorm.DB, tid string) int64 {
	var maxSeq int64
	_ = tx.Model(&model.TaskStatusEvent{}).Where("tid = ?", tid).Select("COALESCE(MAX(sequence), 0)").Scan(&maxSeq).Error
	return maxSeq + 1
}

func (s *APIServer) recordAgentAudit(ip string, hostname string, event string, reason string) {
	if ip == "" {
		return
	}
	if event == "" {
		event = "status_change"
	}
	if reason == "" {
		reason = "Agent 状态变化"
	}
	if err := s.DB.Create(&model.AgentAuditLog{
		IPAddr:    ip,
		Hostname:  hostname,
		Event:     event,
		Reason:    reason,
		CreatedAt: time.Now(),
	}).Error; err != nil {
		s.Logger.Warn("记录 Agent 审计失败", zap.String("ip", ip), zap.Error(err))
		return
	}
	s.Logger.Info("Agent 审计事件",
		zap.String("ip", ip),
		zap.String("hostname", hostname),
		zap.String("event", event),
		zap.String("reason", reason),
	)
}
