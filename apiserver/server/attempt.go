package server

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"

	"github.com/mini-drop/apiserver/model"
)

// startTaskAttempt creates immutable evidence for each delivery attempt. The
// sequence number is calculated inside a transaction so retries never replace
// earlier diagnostics.
func (s *APIServer) startTaskAttempt(task *model.HotmethodTask, trigger string) (*model.TaskAttempt, error) {
	var attempt model.TaskAttempt
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&model.TaskAttempt{}).Where("task_tid = ?", task.TID).Count(&count).Error; err != nil {
			return err
		}
		now := time.Now()
		attempt = model.TaskAttempt{
			TaskTID: task.TID, AttemptSeq: int(count) + 1, Trigger: trigger,
			AgentIP: task.TargetIP, BeginTime: &now, CreatedAt: now,
		}
		var agent model.AgentInfo
		if tx.Where("ip_addr = ?", task.TargetIP).First(&agent).Error == nil {
			attempt.RunnerVersion = agent.Version
		}
		return tx.Create(&attempt).Error
	})
	return &attempt, err
}

func (s *APIServer) recordTaskAttemptFailure(task *model.HotmethodTask, trigger, code, message string) {
	attempt, err := s.startTaskAttempt(task, trigger)
	if err == nil {
		s.finishTaskAttempt(attempt.ID, code, message)
	}
}

func (s *APIServer) finishTaskAttempt(id uint, errorCode, message string) {
	if id == 0 {
		return
	}
	now := time.Now()
	updates := map[string]interface{}{"end_time": &now, "error_code": errorCode, "error_message": message}
	if errorCode == "" {
		updates["exit_code"] = 0
	} else {
		updates["exit_code"] = 1
	}
	_ = s.DB.Model(&model.TaskAttempt{}).Where("id = ?", id).Updates(updates).Error
}

func (s *APIServer) finishLatestTaskAttempt(tid, errorCode, message string, keys []string) {
	_ = s.DB.Transaction(func(tx *gorm.DB) error {
		return s.finishLatestTaskAttemptTx(tx, tid, errorCode, message, keys, 0)
	})
}

func (s *APIServer) finishLatestTaskAttemptTx(tx *gorm.DB, tid, errorCode, message string, keys []string, artifactID uint) error {
	var attempt model.TaskAttempt
	if err := tx.Where("task_tid = ? AND end_time IS NULL", tid).Order("attempt_seq DESC").First(&attempt).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil
		}
		return err
	}
	now := time.Now()
	updates := map[string]interface{}{"end_time": &now, "error_code": errorCode, "error_message": message}
	if errorCode == "" {
		updates["exit_code"] = 0
	} else {
		updates["exit_code"] = 1
	}
	if len(keys) > 0 {
		encoded, err := json.Marshal(keys)
		if err != nil {
			return err
		}
		updates["artifact_keys"] = encoded
		if artifactID != 0 {
			if err := tx.Model(&model.Artifact{}).Where("id = ?", artifactID).Update("attempt_id", attempt.ID).Error; err != nil {
				return err
			}
		}
	}
	return tx.Model(&attempt).Updates(updates).Error
}
