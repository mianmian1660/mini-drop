// ============================================================
// server/parquet_writer.go — 阶段五：Parquet 流式写入 MinIO
// ============================================================
// 固定写入参数：ZSTD、低基数/dict 字符串、row group 目标 16 MiB、
// 单文件 ≤128 MiB（超出拆 part）、按 (timestamp, session, process) 排序、
// 流式写入（io.Pipe 直连 MinIO multipart）、同步计算字节数与 SHA-256、
// 上传后回读 footer/schema/统计校验，通过后才允许登记 active。
// ============================================================

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress/zstd"
	"go.uber.org/zap"
)

const (
	parquetCreatedByApp = "mini-drop-apiserver"
	parquetZstdLevel    = 3
)

// parquetWriteResult 一次物理 part 写入结果。
type parquetWriteResult struct {
	ObjectKey        string               `json:"object_key"`
	SizeBytes        int64                `json:"size_bytes"`
	SHA256           string               `json:"sha256"`
	RowCount         int64                `json:"row_count"`
	RowGroupCount    int                  `json:"row_group_count"`
	RowGroupBoundary []pqRowGroupBoundary `json:"row_group_boundaries"`
}

// pqRow 四种信号行共有的排序能力（timestamp, session, pid, process_start_ms）。
type pqRow interface {
	pqSortKey() pqSortKey
}

// pqSortKey 排序键。
type pqSortKey struct {
	Timestamp      int64
	SessionSID     string
	PID            int32
	ProcessStartMs int64
}

func pqLess(a, b pqSortKey) bool {
	if a.Timestamp != b.Timestamp {
		return a.Timestamp < b.Timestamp
	}
	if a.SessionSID != b.SessionSID {
		return a.SessionSID < b.SessionSID
	}
	if a.PID != b.PID {
		return a.PID < b.PID
	}
	return a.ProcessStartMs < b.ProcessStartMs
}

// countingReader 统计流经的字节数。
type countingReader struct {
	reader io.Reader
	count  int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	c.count += int64(n)
	return n, err
}

// pqRowsPerRowGroup 估算每 row group 行数：目标字节 / 平均行字节。
// 平均行字节由 builder 在写入前估算（CPU 行含 frames 较大）。
func pqRowsPerRowGroup(avgRowBytes, rowGroupTargetBytes int64) int64 {
	if avgRowBytes <= 0 {
		avgRowBytes = 1024
	}
	if rowGroupTargetBytes <= 0 {
		rowGroupTargetBytes = 16 << 20
	}
	perGroup := rowGroupTargetBytes / avgRowBytes
	if perGroup < 1000 {
		perGroup = 1000
	}
	if perGroup > 5_000_000 {
		perGroup = 5_000_000
	}
	return perGroup
}

// writeParquetPartGeneric 把一批同构行流式写入 MinIO 并回读校验。
// T 必须是四个行类型之一（CPU/Metrics/Histogram/DB），实现 pqRow。
func writeParquetPartGeneric[T pqRow](s *APIServer, ctx context.Context, objectKey string, rows []T) (parquetWriteResult, error) {
	if len(rows) == 0 {
		return parquetWriteResult{}, fmt.Errorf("拒绝写入空 part")
	}
	sort.Slice(rows, func(i, j int) bool {
		return pqLess(rows[i].pqSortKey(), rows[j].pqSortKey())
	})

	cfg := s.Config.ContinuousParquet
	rowGroupTarget := cfg.RowGroupTargetBytes
	if rowGroupTarget <= 0 {
		rowGroupTarget = 16 << 20
	}
	avgRowBytes := pqAvgRowBytesEstimate.Load()
	rowsPerGroup := pqRowsPerRowGroup(avgRowBytes, rowGroupTarget)

	pr, pw := io.Pipe()
	type writeOutcome struct {
		rowCount int64
		err      error
	}
	outcome := make(chan writeOutcome, 1)
	go func() {
		writer := parquet.NewGenericWriter[T](pw,
			parquet.Compression(&zstd.Codec{Level: parquetZstdLevel}),
			parquet.MaxRowsPerRowGroup(rowsPerGroup),
			parquet.DataPageVersion(2),
			parquet.DataPageStatistics(true),
			parquet.CreatedBy(parquetCreatedByApp, "v2", "continuous-parquet"),
		)
		var err error
		written := int64(0)
		batch := 20_000
		for start := 0; start < len(rows); start += batch {
			end := start + batch
			if end > len(rows) {
				end = len(rows)
			}
			n, werr := writer.Write(rows[start:end])
			written += int64(n)
			if werr != nil {
				err = werr
				break
			}
		}
		if err == nil {
			err = writer.Close()
		}
		outcome <- writeOutcome{rowCount: written, err: err}
		_ = pw.CloseWithError(err)
	}()

	hasher := sha256.New()
	counted := &countingReader{reader: io.TeeReader(pr, hasher)}
	putErr := s.Storage.PutObject(ctx, s.Config.Storage.Bucket, objectKey, counted, -1, "application/octet-stream")

	res := <-outcome
	if putErr != nil {
		return parquetWriteResult{}, fmt.Errorf("上传 parquet part %s 失败: %w", objectKey, putErr)
	}
	if res.err != nil {
		return parquetWriteResult{}, fmt.Errorf("写入 parquet part %s 失败: %w", objectKey, res.err)
	}

	result := parquetWriteResult{
		ObjectKey: objectKey,
		SizeBytes: counted.count,
		SHA256:    hex.EncodeToString(hasher.Sum(nil)),
	}

	// 回读校验：footer/schema/row group 统计。
	if err := s.verifyParquetPart(ctx, &result); err != nil {
		_ = s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, objectKey)
		return parquetWriteResult{}, fmt.Errorf("parquet part %s 校验失败，已清理: %w", objectKey, err)
	}
	if result.RowCount != res.rowCount {
		_ = s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, objectKey)
		return parquetWriteResult{}, fmt.Errorf("parquet part %s 行数不一致（footer=%d, 写入=%d）", objectKey, result.RowCount, res.rowCount)
	}
	return result, nil
}

// verifyParquetPart 回读 footer/schema/统计值并回填 result：
//   - 能打开文件（footer 完整）
//   - RowCount 与写入行数一致
//   - 每 row group 从 timestamp 列 ColumnIndex 计算 min/max 时间
func (s *APIServer) verifyParquetPart(ctx context.Context, result *parquetWriteResult) error {
	f, err := s.openParquetPart(ctx, result.ObjectKey)
	if err != nil {
		return err
	}
	result.RowGroupCount = len(f.RowGroups())
	tsIdx, err := pqTimestampColumnIndex(f)
	if err != nil {
		return err
	}
	var totalRows int64
	var boundaries []pqRowGroupBoundary
	for rgIndex, rg := range f.RowGroups() {
		totalRows += rg.NumRows()
		chunks := rg.ColumnChunks()
		if tsIdx >= len(chunks) {
			return fmt.Errorf("row group %d 缺少 timestamp 列", rgIndex)
		}
		ci, err := chunks[tsIdx].ColumnIndex()
		if err != nil {
			// 无列索引时该组范围未知（查询退化为读全组）
			boundaries = append(boundaries, pqRowGroupBoundary{RowIndex: rg.NumRows(), MinTS: 0, MaxTS: 0})
			continue
		}
		var minTS, maxTS int64
		first := true
		for page := 0; page < ci.NumPages(); page++ {
			lo := ci.MinValue(page).Int64()
			hi := ci.MaxValue(page).Int64()
			if first {
				minTS, maxTS = lo, hi
				first = false
			} else {
				if lo < minTS {
					minTS = lo
				}
				if hi > maxTS {
					maxTS = hi
				}
			}
		}
		boundaries = append(boundaries, pqRowGroupBoundary{RowIndex: rg.NumRows(), MinTS: minTS, MaxTS: maxTS})
	}
	result.RowCount = totalRows
	if result.RowGroupBoundary == nil {
		result.RowGroupBoundary = boundaries
	}
	if totalRows == 0 {
		return errors.New("parquet 文件无行")
	}
	return nil
}

// parquetObjectKeyV2 生成 v2 对象 key：
//
//	continuous/v2/{tenant}/date=YYYY-MM-DD/hour=HH/
//	  signal={cpu|metrics|histogram|db}/resolution={raw|5m|1h}/{block-id}-{part}.parquet
func parquetObjectKeyV2(tenant string, bucketStart time.Time, signalType, resolution, blockID string, partIndex int) string {
	utc := bucketStart.UTC()
	return fmt.Sprintf("continuous/v2/%s/date=%04d-%02d-%02d/hour=%02d/signal=%s/resolution=%s/%s-%02d.parquet",
		tenant, utc.Year(), int(utc.Month()), utc.Day(), utc.Hour(), signalType, resolution, blockID, partIndex)
}

// marshalRowGroupBoundaries JSON 序列化（ledger 存 JSONB）。
func marshalRowGroupBoundaries(boundaries []pqRowGroupBoundary) ([]byte, error) {
	if len(boundaries) == 0 {
		return []byte(`[]`), nil
	}
	return json.Marshal(boundaries)
}

// pqLogPartWritten 记录 part 写入日志。
func pqLogPartWritten(s *APIServer, result parquetWriteResult, signalType, resolution string) {
	s.Logger.Info("parquet v2 part 已写入",
		zap.String("signal", signalType), zap.String("resolution", resolution),
		zap.String("object_key", result.ObjectKey),
		zap.Int64("size_bytes", result.SizeBytes),
		zap.Int64("row_count", result.RowCount),
		zap.Int("row_groups", result.RowGroupCount),
		zap.String("sha256", result.SHA256[:16]))
}
