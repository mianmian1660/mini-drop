package server

import (
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
)

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

	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(task).Updates(updates).Error; err != nil {
			return err
		}
		return tx.Create(&model.TaskStatusEvent{
			TID:        task.TID,
			FromStatus: fromStatus,
			ToStatus:   toStatus,
			Reason:     reason,
			Source:     source,
			CreatedAt:  time.Now(),
		}).Error
	})
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
	if err := s.DB.Create(&model.TaskStatusEvent{
		TID:        tid,
		FromStatus: fromStatus,
		ToStatus:   toStatus,
		Reason:     reason,
		Source:     source,
		CreatedAt:  time.Now(),
	}).Error; err != nil {
		s.Logger.Warn("记录任务状态事件失败", zap.String("tid", tid), zap.Error(err))
	}
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
