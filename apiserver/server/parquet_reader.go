// ============================================================
// server/parquet_reader.go — 阶段五：Parquet 范围读取与 row group 选择
// ============================================================
// 对象存储读取接口增加带 size 的 ReaderAt/range-read 能力（Storage
// 接口新增 GetObjectRange）。查询只读取 footer、匹配 row group 和需要
// 列，不允许全量下载多个 Parquet 后再过滤。
// ============================================================

package server

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/parquet-go/parquet-go"
)

// objectReaderAt 把 Storage 的 range-read 包装成 io.ReaderAt（parquet-go
// OpenFile 要求）。每次 ReadAt 发一个 Range 请求。
type objectReaderAt struct {
	store interface {
		GetObjectRange(ctx context.Context, bucket, key string, offset, length int64) (io.ReadCloser, error)
	}
	bucket string
	key    string
	size   int64
	ctx    context.Context
}

func (o *objectReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("objectReaderAt: 负 offset")
	}
	if off >= o.size {
		return 0, io.EOF
	}
	length := int64(len(p))
	if off+length > o.size {
		length = o.size - off
	}
	if length <= 0 {
		return 0, io.EOF
	}
	ctx := o.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	rc, err := o.store.GetObjectRange(ctx, o.bucket, o.key, off, length)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	n, err := io.ReadFull(rc, p[:length])
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			// 源对象已到末尾：ReadAt 语义返回 io.EOF。
			return n, io.EOF
		}
		return n, err
	}
	if off+length >= o.size {
		return n, io.EOF
	}
	return n, nil
}

// openParquetPart 打开一个 v2 part（footer + 元数据通过 range-read 读取）。
func (s *APIServer) openParquetPart(ctx context.Context, objectKey string) (*parquet.File, error) {
	if !s.StorageConnected() {
		return nil, errProfileUnavailable
	}
	size, err := s.Storage.StatObject(ctx, s.Config.Storage.Bucket, objectKey)
	if err != nil {
		return nil, fmt.Errorf("stat parquet part %s: %w", objectKey, err)
	}
	if size <= 0 {
		return nil, fmt.Errorf("parquet part %s 大小非法: %d", objectKey, size)
	}
	ra := &objectReaderAt{
		store:  s.Storage,
		bucket: s.Config.Storage.Bucket,
		key:    objectKey,
		size:   size,
		ctx:    ctx,
	}
	f, err := parquet.OpenFile(ra, size, parquet.SkipBloomFilters(true))
	if err != nil {
		return nil, fmt.Errorf("打开 parquet part %s: %w", objectKey, err)
	}
	return f, nil
}

// pqTimestampColumnIndex 返回 timestamp 叶列在 RowGroup.ColumnChunks 中的
// 下标（按 leaf 顺序）。找不到返回错误。
func pqTimestampColumnIndex(f *parquet.File) (int, error) {
	paths := f.Schema().Columns()
	for i, path := range paths {
		if len(path) == 1 && path[0] == "timestamp" {
			return i, nil
		}
	}
	// 兜底：任何以 timestamp 结尾的路径
	for i, path := range paths {
		if len(path) > 0 && path[len(path)-1] == "timestamp" {
			return i, nil
		}
	}
	return -1, fmt.Errorf("schema 缺少 timestamp 叶列")
}

// pqRowGroupRange 单个 row group 的时间范围（MinTS/MaxTS 为 0 表示未知）。
type pqRowGroupRange struct {
	Index int
	MinTS int64
	MaxTS int64
}

// pqSelectRowGroups 按时间范围选择匹配的 row group。
//   - 全部组时间未知时返回全部组（保守）。
//   - 已知的组按 [minTS, maxTS] 与查询区间 [fromMS, toMS] 相交判定。
func pqSelectRowGroups(f *parquet.File, fromMS, toMS int64) []pqRowGroupRange {
	tsIdx, err := pqTimestampColumnIndex(f)
	if err != nil {
		// 无 timestamp 列（异常文件）：读全部
		out := make([]pqRowGroupRange, len(f.RowGroups()))
		for i := range f.RowGroups() {
			out[i] = pqRowGroupRange{Index: i}
		}
		return out
	}
	anyKnown := false
	ranges := make([]pqRowGroupRange, 0, len(f.RowGroups()))
	for i, rg := range f.RowGroups() {
		chunks := rg.ColumnChunks()
		item := pqRowGroupRange{Index: i}
		if tsIdx < len(chunks) {
			if ci, cerr := chunks[tsIdx].ColumnIndex(); cerr == nil {
				first := true
				for page := 0; page < ci.NumPages(); page++ {
					lo, hi := ci.MinValue(page).Int64(), ci.MaxValue(page).Int64()
					if first {
						item.MinTS, item.MaxTS = lo, hi
						first = false
					} else {
						if lo < item.MinTS {
							item.MinTS = lo
						}
						if hi > item.MaxTS {
							item.MaxTS = hi
						}
					}
				}
				if !first {
					anyKnown = true
				}
			}
		}
		ranges = append(ranges, item)
	}
	if !anyKnown {
		return ranges // 全部未知 → 全部候选
	}
	out := make([]pqRowGroupRange, 0, len(ranges))
	for _, item := range ranges {
		if item.MinTS == 0 && item.MaxTS == 0 {
			out = append(out, item) // 单组未知 → 保守读取
			continue
		}
		if item.MaxTS < fromMS || item.MinTS > toMS {
			continue
		}
		out = append(out, item)
	}
	return out
}

// readParquetRows 读取 part 中与 [fromMS, toMS] 相交的 row group 的全部行
// （按需列投影：T 的结构决定读取哪些列）。fromMS/toMS 传 0,0 表示不筛选。
func readParquetRows[T any](s *APIServer, ctx context.Context, objectKey string, fromMS, toMS int64) ([]T, error) {
	f, err := s.openParquetPart(ctx, objectKey)
	if err != nil {
		return nil, err
	}

	groups := f.RowGroups()
	var selected []parquet.RowGroup
	if fromMS == 0 && toMS == 0 {
		selected = groups
	} else {
		for _, item := range pqSelectRowGroups(f, fromMS, toMS) {
			selected = append(selected, groups[item.Index])
		}
	}

	out := make([]T, 0, 1024)
	for _, rg := range selected {
		reader := parquet.NewRowGroupReader(rg)
		for {
			var row T
			err := reader.Read(&row)
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				_ = reader.Close()
				return nil, fmt.Errorf("读取 parquet part %s row group: %w", objectKey, err)
			}
			out = append(out, row)
		}
		_ = reader.Close()
	}
	return out, nil
}
