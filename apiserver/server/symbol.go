// ============================================================
// server/symbol.go — 符号库账本接口（阶段三）
// ============================================================
// 只服务 drop_agent，不服务浏览器：Agent 采集完一个 perf CPU 任务后，
// 先问这里"这些 build-id 你有没有"，只上传服务端没有的那部分二进制，
// 实现跨任务、跨 Agent 的符号去重（docs/symbolization-design.md §7.2）。
//
// 响应体故意保持 {"missing":[...]} / {"build_id":...,"status":...} 这种
// 扁平形状，不套 RespondOK/RespondErrorCode 那套内部信封——这是要被
// C++ 手写 JSON 解析器读的外部契约，越简单越不容易解析错。
// ============================================================

package server

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/mini-drop/apiserver/model"
)

// maxSymbolUploadBytes 单个符号文件大小上限（§8 风险 8：需限制上传体量）。
const maxSymbolUploadBytes = 200 * 1024 * 1024

// maxKernelSymbolUploadBytes keeps kallsyms uploads bounded. Current Linux
// kallsyms snapshots are usually around 10MB on the target host.
const maxKernelSymbolUploadBytes = 64 * 1024 * 1024

// buildIDPattern 校验 build_id 只能是十六进制字符串，防止被用来拼出
// 越界的 MinIO 对象 key。
var buildIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8,64}$`)

var sha256Pattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

type symbolCheckEntry struct {
	BuildID string `json:"build_id" binding:"required"`
	DSOPath string `json:"dso_path"`
}

type symbolCheckReq struct {
	TID     string             `json:"tid" binding:"required"`
	Entries []symbolCheckEntry `json:"entries" binding:"required"`
}

type kernelSymbolCheckReq struct {
	TID           string `json:"tid" binding:"required"`
	SHA256        string `json:"sha256" binding:"required"`
	SizeBytes     int64  `json:"size_bytes"`
	KernelRelease string `json:"kernel_release"`
	Hostname      string `json:"hostname"`
	TargetIP      string `json:"target_ip"`
}

// CheckSymbols — POST /api/v1/symbols/check
// 请求  {"tid": "...", "entries": [{"build_id": "...", "dso_path": "..."}]}
// 响应  {"missing": ["build_id1", "build_id2"]}
//
// 顺带把这批 (tid, build_id, dso_path) 写进 task_build_ids——这是 Agent
// 唯一一次已经算好这份清单的时机，不为此单独开第三个接口。
func (s *APIServer) CheckSymbols(c *gin.Context) {
	var req symbolCheckReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数错误: " + err.Error()})
		return
	}
	if len(req.Entries) == 0 {
		c.JSON(http.StatusOK, gin.H{"missing": []string{}})
		return
	}

	// 按 build_id 去重：同一个 tid 里，同一个 build_id 可能因为共享库被
	// 挂载在不同路径而出现多条记录，保留第一次出现的 dso_path 即可。
	dedup := make(map[string]string, len(req.Entries))
	order := make([]string, 0, len(req.Entries))
	for _, e := range req.Entries {
		bid := strings.TrimSpace(e.BuildID)
		if bid == "" || !buildIDPattern.MatchString(bid) {
			continue // 单条格式不合法不应让整批请求失败
		}
		if _, ok := dedup[bid]; !ok {
			dedup[bid] = e.DSOPath
			order = append(order, bid)
		}
	}
	if len(order) == 0 {
		c.JSON(http.StatusOK, gin.H{"missing": []string{}})
		return
	}

	var missing []string
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		rows := make([]model.TaskBuildID, 0, len(order))
		for _, bid := range order {
			rows = append(rows, model.TaskBuildID{TID: req.TID, BuildID: bid, DSOPath: dedup[bid]})
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "tid"}, {Name: "build_id"}},
			DoNothing: true,
		}).Create(&rows).Error; err != nil {
			return err
		}

		var present []string
		if err := tx.Model(&model.SymbolFile{}).
			Where("build_id IN ? AND status = ?", order, model.SymbolFileStatusReady).
			Pluck("build_id", &present).Error; err != nil {
			return err
		}
		presentSet := make(map[string]bool, len(present))
		for _, p := range present {
			presentSet[p] = true
		}
		missing = make([]string, 0, len(order))
		for _, bid := range order {
			if !presentSet[bid] {
				missing = append(missing, bid)
			}
		}
		return nil
	})
	if err != nil {
		s.Logger.Error("symbols/check 失败", zap.String("tid", req.TID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "查询失败: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"missing": missing})
}

// UploadSymbol — PUT /api/v1/symbols/:build_id
// 请求体为 ELF 文件本体（raw bytes）
// 响应  {"build_id": "...", "status": "ready"}
func (s *APIServer) UploadSymbol(c *gin.Context) {
	buildID := c.Param("build_id")
	if !buildIDPattern.MatchString(buildID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "build_id 格式不合法"})
		return
	}
	if !s.StorageConnected() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "对象存储未连接"})
		return
	}
	if c.Request.ContentLength > maxSymbolUploadBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "文件超过大小上限"})
		return
	}

	// 限制读取上限，防止未声明 Content-Length 的请求把内存/存储撑爆。
	limited := http.MaxBytesReader(c.Writer, c.Request.Body, maxSymbolUploadBytes)
	br := bufio.NewReaderSize(limited, 4)
	magic, err := br.Peek(4)
	if err != nil || string(magic) != "\x7fELF" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "上传内容不是合法的 ELF 文件"})
		return
	}

	// 边读边算 sha256、边传 MinIO，不做二次缓冲（大文件也不会撑爆内存）。
	hasher := sha256.New()
	tee := io.TeeReader(br, hasher)

	objectKey := "symbols/" + buildID
	size := c.Request.ContentLength
	if size <= 0 {
		size = -1 // 未知大小交给 Storage 层处理
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := s.Storage.PutObject(ctx, s.Config.Storage.Bucket, objectKey, tee, size, "application/octet-stream"); err != nil {
		s.Logger.Error("符号文件上传失败", zap.String("build_id", buildID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "上传失败: " + err.Error()})
		return
	}

	row := model.SymbolFile{
		BuildID:   buildID,
		FileName:  buildID,
		ObjectKey: objectKey,
		SizeBytes: c.Request.ContentLength,
		SHA256:    hex.EncodeToString(hasher.Sum(nil)),
		Status:    model.SymbolFileStatusReady,
		CreatedAt: time.Now(),
	}
	// 先到先得：并发上传同一个 build_id 时，第一个成功落库的为准，
	// 后来者忽略而不是报错（§8 风险 6）。
	if err := s.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "build_id"}},
		DoNothing: true,
	}).Create(&row).Error; err != nil {
		s.Logger.Error("symbol_files 落库失败", zap.String("build_id", buildID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "落库失败: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"build_id": buildID, "status": "ready"})
}

// CheckKernelSymbol — POST /api/v1/kernel-symbols/check
// Agent 先发送 kallsyms 的 sha256 和元数据；服务端已有同内容快照时只登记
// 当前任务引用，否则返回 upload_required=true 让 Agent 再 PUT 文件本体。
func (s *APIServer) CheckKernelSymbol(c *gin.Context) {
	var req kernelSymbolCheckReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数错误: " + err.Error()})
		return
	}
	req.SHA256 = strings.ToLower(strings.TrimSpace(req.SHA256))
	req.TID = strings.TrimSpace(req.TID)
	if req.TID == "" || !sha256Pattern.MatchString(req.SHA256) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tid 或 sha256 格式不合法"})
		return
	}
	if !s.StorageConnected() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "对象存储未连接"})
		return
	}

	objectKey := kernelSymbolObjectKey(req.SHA256)
	now := time.Now()
	var row model.KernelSymbolFile
	err := s.DB.Where("sha256 = ? AND status = ?", req.SHA256, model.SymbolFileStatusReady).First(&row).Error
	if err == nil {
		if row.ObjectKey == "" {
			row.ObjectKey = objectKey
		}
		_ = s.DB.Model(&model.KernelSymbolFile{}).Where("sha256 = ?", req.SHA256).Updates(map[string]interface{}{
			"last_used_at": &now,
			"target_ip":    firstNonEmpty(req.TargetIP, row.TargetIP),
		}).Error
		if err := s.recordKernelSymbolArtifact(req.TID, row.ObjectKey, row.SizeBytes, req.SHA256, row.BlobID); err != nil {
			s.Logger.Warn("登记 kallsyms artifact 引用失败", zap.String("tid", req.TID), zap.String("sha256", req.SHA256), zap.Error(err))
		}
		c.JSON(http.StatusOK, gin.H{"upload_required": false, "object_key": row.ObjectKey})
		return
	}
	if err != gorm.ErrRecordNotFound {
		s.Logger.Error("查询 kallsyms 账本失败", zap.String("sha256", req.SHA256), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "查询失败: " + err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exists, existsErr := s.Storage.ObjectExists(ctx, s.Config.Storage.Bucket, objectKey)
	if existsErr == nil && !exists {
		legacyKey := "kernel-symbols/" + req.SHA256 + "/kallsyms"
		if legacyExists, legacyErr := s.Storage.ObjectExists(ctx, s.Config.Storage.Bucket, legacyKey); legacyErr == nil && legacyExists {
			objectKey, exists = legacyKey, true
		}
	}
	if existsErr == nil && exists {
		row = model.KernelSymbolFile{
			SHA256:        req.SHA256,
			ObjectKey:     objectKey,
			KernelRelease: req.KernelRelease,
			Hostname:      req.Hostname,
			TargetIP:      req.TargetIP,
			SizeBytes:     req.SizeBytes,
			Status:        model.SymbolFileStatusReady,
			CreatedAt:     now,
			LastUsedAt:    &now,
		}
		if err := s.DB.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "sha256"}},
			DoUpdates: clause.Assignments(map[string]interface{}{
				"object_key":     row.ObjectKey,
				"kernel_release": row.KernelRelease,
				"hostname":       row.Hostname,
				"target_ip":      row.TargetIP,
				"size_bytes":     row.SizeBytes,
				"status":         row.Status,
				"last_used_at":   row.LastUsedAt,
			}),
		}).Create(&row).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "落库失败: " + err.Error()})
			return
		}
		if err := s.recordKernelSymbolArtifact(req.TID, objectKey, req.SizeBytes, req.SHA256, nil); err != nil {
			s.Logger.Warn("登记 kallsyms artifact 引用失败", zap.String("tid", req.TID), zap.String("sha256", req.SHA256), zap.Error(err))
		}
		c.JSON(http.StatusOK, gin.H{"upload_required": false, "object_key": objectKey})
		return
	}

	c.JSON(http.StatusOK, gin.H{"upload_required": true, "object_key": objectKey})
}

// UploadKernelSymbol — PUT /api/v1/kernel-symbols/:sha256
func (s *APIServer) UploadKernelSymbol(c *gin.Context) {
	sum := strings.ToLower(strings.TrimSpace(c.Param("sha256")))
	tid := strings.TrimSpace(c.Query("tid"))
	if tid == "" || !sha256Pattern.MatchString(sum) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tid 或 sha256 格式不合法"})
		return
	}
	if !s.StorageConnected() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "对象存储未连接"})
		return
	}
	if c.Request.ContentLength > maxKernelSymbolUploadBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "文件超过大小上限"})
		return
	}

	limited := http.MaxBytesReader(c.Writer, c.Request.Body, maxKernelSymbolUploadBytes)
	var source io.Reader = limited
	if strings.EqualFold(strings.TrimSpace(c.GetHeader("Content-Encoding")), "gzip") {
		zr, zerr := gzip.NewReader(limited)
		if zerr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "gzip 内容无效"})
			return
		}
		defer zr.Close()
		source = zr
	}
	// Limit decompressed bytes too: the URL hash is always the raw payload hash.
	body, err := io.ReadAll(io.LimitReader(source, maxKernelSymbolUploadBytes+1))
	if err != nil {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "读取上传内容失败: " + err.Error()})
		return
	}
	if int64(len(body)) > maxKernelSymbolUploadBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "解压后文件超过大小上限"})
		return
	}
	if !looksLikeKallsyms(body) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "上传内容不是合法 kallsyms 快照"})
		return
	}
	actual := sha256.Sum256(body)
	actualHex := hex.EncodeToString(actual[:])
	if actualHex != sum {
		c.JSON(http.StatusBadRequest, gin.H{"error": "sha256 校验失败"})
		return
	}

	objectKey := kernelSymbolObjectKey(sum)
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(body); err != nil || zw.Close() != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "压缩上传内容失败"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := s.putObjectWithEncoding(ctx, objectKey, bytes.NewReader(compressed.Bytes()), int64(compressed.Len()), "application/octet-stream", "gzip"); err != nil {
		s.Logger.Error("kallsyms 上传失败", zap.String("sha256", sum), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "上传失败: " + err.Error()})
		return
	}

	now := time.Now()
	row := model.KernelSymbolFile{
		SHA256:        sum,
		ObjectKey:     objectKey,
		KernelRelease: strings.TrimSpace(c.Query("kernel_release")),
		Hostname:      strings.TrimSpace(c.Query("hostname")),
		TargetIP:      strings.TrimSpace(c.Query("target_ip")),
		SizeBytes:     int64(compressed.Len()),
		Status:        model.SymbolFileStatusReady,
		CreatedAt:     now,
		LastUsedAt:    &now,
	}
	if err := s.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "sha256"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"object_key":     row.ObjectKey,
			"kernel_release": row.KernelRelease,
			"hostname":       row.Hostname,
			"target_ip":      row.TargetIP,
			"size_bytes":     row.SizeBytes,
			"status":         row.Status,
			"last_used_at":   row.LastUsedAt,
		}),
	}).Create(&row).Error; err != nil {
		s.Logger.Error("kallsyms 账本落库失败", zap.String("sha256", sum), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "落库失败: " + err.Error()})
		return
	}
	if err := s.recordKernelSymbolArtifact(tid, objectKey, int64(compressed.Len()), sum, nil); err != nil {
		s.Logger.Warn("登记 kallsyms artifact 引用失败", zap.String("tid", tid), zap.String("sha256", sum), zap.Error(err))
	}
	c.JSON(http.StatusOK, gin.H{"sha256": sum, "status": "ready", "object_key": objectKey})
}

func kernelSymbolObjectKey(sum string) string {
	return "kernel-symbols/" + sum + "/kallsyms.gz"
}

func isKernelSymbolObjectKey(key string) bool {
	return strings.HasPrefix(key, "kernel-symbols/") && (strings.HasSuffix(key, "/kallsyms") || strings.HasSuffix(key, "/kallsyms.gz"))
}

type encodingObjectStorage interface {
	PutObjectWithEncoding(context.Context, string, string, io.Reader, int64, string, string) error
}

func (s *APIServer) putObjectWithEncoding(ctx context.Context, key string, data io.Reader, size int64, contentType, encoding string) error {
	if encoded, ok := s.Storage.(encodingObjectStorage); ok {
		return encoded.PutObjectWithEncoding(ctx, s.Config.Storage.Bucket, key, data, size, contentType, encoding)
	}
	return s.Storage.PutObject(ctx, s.Config.Storage.Bucket, key, data, size, contentType)
}

// recordKernelSymbolArtifact 登记 kallsyms artifact 引用。
// 阶段二：blobID 非空时把 artifact 直接关联到已迁移的 CAS blob
// （避免新引用指向旧 key 造成孤儿 blob、阻塞 GC）。
func (s *APIServer) recordKernelSymbolArtifact(tid, objectKey string, size int64, sum string, blobID *uint) error {
	if tid == "" || objectKey == "" {
		return nil
	}
	artifact := model.Artifact{
		TaskTID:     tid,
		Kind:        model.ArtifactKindRaw,
		ObjectKey:   objectKey,
		Size:        size,
		SHA256:      sum,
		Hash:        "sha256:" + sum,
		Retention:   "raw",
		ContentType: "application/octet-stream",
		Status:      model.ArtifactStatusReady,
		BlobID:      blobID,
		CreatedAt:   time.Now(),
	}
	return s.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "task_tid"}, {Name: "kind"}, {Name: "object_key"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"size":         artifact.Size,
			"sha256":       artifact.SHA256,
			"hash":         artifact.Hash,
			"retention":    artifact.Retention,
			"content_type": artifact.ContentType,
			"status":       artifact.Status,
			"blob_id":      artifact.BlobID,
		}),
	}).Create(&artifact).Error
}

func looksLikeKallsyms(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	lines := bytes.SplitN(body, []byte{'\n'}, 8)
	valid := 0
	for _, line := range lines {
		fields := bytes.Fields(line)
		if len(fields) < 3 {
			continue
		}
		addr := fields[0]
		if len(addr) < 8 {
			continue
		}
		ok := true
		nonZero := false
		for _, b := range addr {
			if !((b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')) {
				ok = false
				break
			}
			if b != '0' {
				nonZero = true
			}
		}
		if ok && nonZero {
			valid++
		}
	}
	return valid > 0
}
