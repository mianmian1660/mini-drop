package server

import (
	"context"
	"sort"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/mini-drop/apiserver/model"
)

const (
	continuousMigrationReceiptPassed  = "passed"
	continuousMigrationReceiptRevoked = "revoked"
)

// pqPersistMigrationReceiptsTx 把 raw member 固化为独立于物理块生命周期的迁移凭证。
func (s *APIServer) pqPersistMigrationReceiptsTx(tx *gorm.DB, key pqBlockKey, blockID string,
	bucketEnd time.Time, members []model.ContinuousParquetBlockMember) error {
	if key.Resolution != model.ContinuousParquetResolutionRaw {
		return nil
	}
	now := time.Now()
	for i := range members {
		member := &members[i]
		if member.SourceKind != "batch" || member.SourceRef == "" {
			continue
		}
		receipt := model.ContinuousMigrationReceipt{
			Tenant: key.Tenant, SourceKind: member.SourceKind, SourceRef: member.SourceRef,
			SessionSID: member.SessionSID, SignalType: key.SignalType, BlockID: blockID,
			BucketStart: key.BucketStart, BucketEnd: bucketEnd,
			StartTime: member.StartTime, EndTime: member.EndTime,
			SampleCount: member.SampleCount, ValueTotal: member.ValueTotal, RowCount: member.RowCount,
			Status: continuousMigrationReceiptPassed, CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "source_kind"}, {Name: "source_ref"}, {Name: "block_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"tenant", "session_sid", "signal_type", "bucket_start", "bucket_end",
				"start_time", "end_time", "sample_count", "value_total", "row_count",
				"status", "revoke_reason", "updated_at",
			}),
		}).Create(&receipt).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *APIServer) pqRevokeMigrationReceipts(ctx context.Context, blockID, reason string) {
	if blockID == "" {
		return
	}
	_ = s.DB.WithContext(ctx).Model(&model.ContinuousMigrationReceipt{}).
		Where("block_id = ? AND status = ?", blockID, continuousMigrationReceiptPassed).
		Updates(map[string]interface{}{
			"status": continuousMigrationReceiptRevoked, "revoke_reason": reason, "updated_at": time.Now(),
		}).Error
}

func (s *APIServer) pqPassedMigrationReceipts(ctx context.Context, batchID, signal string) ([]model.ContinuousMigrationReceipt, error) {
	var receipts []model.ContinuousMigrationReceipt
	err := s.DB.WithContext(ctx).
		Where("source_kind = ? AND source_ref = ? AND signal_type = ? AND status = ?",
			"batch", batchID, signal, continuousMigrationReceiptPassed).
		Order("start_time ASC, end_time ASC").Find(&receipts).Error
	return receipts, err
}

func pqReceiptsCoverRange(receipts []model.ContinuousMigrationReceipt, start, end time.Time) bool {
	if !start.Before(end) {
		return true
	}
	sort.Slice(receipts, func(i, j int) bool {
		if receipts[i].StartTime.Equal(receipts[j].StartTime) {
			return receipts[i].EndTime.Before(receipts[j].EndTime)
		}
		return receipts[i].StartTime.Before(receipts[j].StartTime)
	})
	cursor := start
	for i := range receipts {
		r := &receipts[i]
		if !r.EndTime.After(cursor) {
			continue
		}
		if r.StartTime.After(cursor) {
			return false
		}
		if r.EndTime.After(cursor) {
			cursor = r.EndTime
		}
		if !cursor.Before(end) {
			return true
		}
	}
	return false
}
