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

// buildIDPattern 校验 build_id 只能是十六进制字符串，防止被用来拼出
// 越界的 MinIO 对象 key。
var buildIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8,64}$`)

type symbolCheckEntry struct {
	BuildID string `json:"build_id" binding:"required"`
	DSOPath string `json:"dso_path"`
}

type symbolCheckReq struct {
	TID     string             `json:"tid" binding:"required"`
	Entries []symbolCheckEntry `json:"entries" binding:"required"`
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
