// ============================================================
// server/frames.go — 阶段五：结构化 frames 采集协议
// ============================================================
// Agent 上报样本时保留旧字段兼容：stack[]（字符串栈）保留；新增 frames[]
// 结构化栈（function/file/line/address/mapping_file/build_id/
// normalized_offset/resolved）。apiserver 优先使用 frames；缺失时将旧
// stack 转成仅有 raw/function 的兼容 frame，保证 v1 查询与 v2 Parquet
// 两条路径都不丢帧。
//
// 敏感 labels 剥离（服务端双保险）：db_targets、password、token、secret、
// credential 等字段禁止进入 Parquet 与查询索引。
// ============================================================

package server

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// ContinuousStackFrame 结构化栈帧。perf 解析保留 IP、symbol、DSO，并从
// perf mmap/build-id 信息计算 file-relative normalized_offset；py-spy、
// memray、bpftrace 能获取的字段如实填写；无法获取的字段为 NULL/0，不推测。
type ContinuousStackFrame struct {
	// Function 符号名（无符号时为空，靠 address/mapping_file 描述）。
	Function string `json:"function,omitempty"`
	// Raw 原始帧串（perf script 原样 / 旧 stack 字符串）。
	Raw string `json:"raw,omitempty"`
	// File 源文件（行号可解析时），如 py-spy/memray 提供。
	File string `json:"file,omitempty"`
	// Line 源文件行号（0 = 未知）。
	Line int32 `json:"line,omitempty"`
	// Address 指令地址（perf IP / bpftrace address；0 = 未知）。
	Address uint64 `json:"address,omitempty"`
	// MappingFile 所属 DSO（如 libc.so.6 路径；"" = 未知）。
	MappingFile string `json:"mapping_file,omitempty"`
	// BuildID ELF build-id（16 进制；"" = 未知）。
	BuildID string `json:"build_id,omitempty"`
	// NormalizedOffset 相对 mapping 基址的偏移（file-relative；0 = 未知）。
	NormalizedOffset uint64 `json:"normalized_offset,omitempty"`
	// Resolved 是否解析出符号（false = [unknown]/地址形式）。
	Resolved bool `json:"resolved,omitempty"`
}

// framesFromLegacyStack 把旧字符串栈转成兼容 frame：形如
// "0x7f1234 [libc.so.6]" 的地址帧解析出 address/mapping_file；其余视为
// 函数名（resolved=true）。
var legacyAddressFrameRE = regexp.MustCompile(`^0x([0-9a-fA-F]+)\s*\[(.*)\]$`)

func framesFromLegacyStack(stack []string) []ContinuousStackFrame {
	if len(stack) == 0 {
		return nil
	}
	out := make([]ContinuousStackFrame, 0, len(stack))
	for _, item := range stack {
		item = strings.TrimSpace(item)
		frame := ContinuousStackFrame{Raw: item}
		if match := legacyAddressFrameRE.FindStringSubmatch(item); match != nil {
			if address, err := strconv.ParseUint(match[1], 16, 64); err == nil {
				frame.Address = address
			}
			frame.MappingFile = strings.TrimSpace(match[2])
			frame.Resolved = false
		} else if item != "" && !strings.HasPrefix(item, "[") {
			frame.Function = item
			frame.Resolved = true
		} else {
			frame.Resolved = false
		}
		out = append(out, frame)
	}
	return out
}

// normalizedSampleFrames 返回样本的有效 frames：优先 Agent 上报的结构化
// frames；为空时回退由旧 stack 转换。返回值在 frame 缺失时可扩展旧字段
// （如 stack_scope/backend 由调用方补充）。
func normalizedSampleFrames(sample ContinuousStackSample) []ContinuousStackFrame {
	if len(sample.Frames) > 0 {
		return sample.Frames
	}
	// continuousSampleStack 同时兼容旧版 stack_string，避免历史 batch
	// 回填到 v2 时被写成空栈。
	if stack := continuousSampleStack(sample); len(stack) > 0 {
		return framesFromLegacyStack(stack)
	}
	return nil
}

// frameDisplayName 生成展示名称（服务端按 frames 生成，v2-only 模式不再
// 依赖旧 stack）：优先 function；无符号时用 "0x%x [%s]" 模仿旧格式。
func frameDisplayName(frame ContinuousStackFrame) string {
	if frame.Function != "" {
		return frame.Function
	}
	if frame.Raw != "" {
		return frame.Raw
	}
	if frame.Address != 0 || frame.MappingFile != "" {
		address := ""
		if frame.Address != 0 {
			address = "0x" + strconv.FormatUint(frame.Address, 16)
		} else {
			address = "0x0"
		}
		if frame.MappingFile != "" {
			return address + " [" + frame.MappingFile + "]"
		}
		return address
	}
	return "[unknown]"
}

// sensitiveLabelKeys 敏感字段白名单匹配：这些 key 禁止进入 Parquet/查询索引。
var sensitiveLabelKeys = []string{
	"password", "passwd", "pwd", "token", "secret", "credential", "credentials",
	"db_targets", "db_password", "db_passwd", "api_key", "apikey", "access_key",
	"accesskey", "private_key", "auth", "authorization",
}

// sanitizeContinuousLabels 深拷贝并剥离敏感 labels（服务端双保险；
// Agent 侧同样剥离）。只保留可查询标量（string/number/bool），
// 嵌套 map/slice 丢弃（Parquet labels 为 map<string,string>）。
func sanitizeContinuousLabels(labels map[string]interface{}) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels))
	for key, value := range labels {
		if isSensitiveLabelKey(key) {
			continue
		}
		switch typed := value.(type) {
		case string:
			out[key] = typed
		case bool:
			if typed {
				out[key] = "true"
			} else {
				out[key] = "false"
			}
		case float64:
			out[key] = strconv.FormatFloat(typed, 'g', -1, 64)
		case int64:
			out[key] = strconv.FormatInt(typed, 10)
		case int:
			out[key] = strconv.Itoa(typed)
		case json.Number:
			out[key] = typed.String()
		}
		// 嵌套结构（map/slice）不进入 Parquet，跳过
	}
	return out
}

func isSensitiveLabelKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	for _, sensitive := range sensitiveLabelKeys {
		if lower == sensitive || strings.Contains(lower, sensitive) {
			return true
		}
	}
	return false
}
