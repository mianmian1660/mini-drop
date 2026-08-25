package server

import (
	"math"
	"testing"
	"time"

	"github.com/mini-drop/apiserver/model"
)

// topItem 构造一条 top.json 记录。字段名和 analysis 端
// collapsed_data_parser.get_top_functions 的输出保持一致。
func topItem(function string, percentage, samples float64) map[string]interface{} {
	return map[string]interface{}{
		"function":   function,
		"percentage": percentage,
		"samples":    samples,
	}
}

func findEntry(entries []DiffEntry, function string) (DiffEntry, bool) {
	for _, e := range entries {
		if e.Function == function {
			return e, true
		}
	}
	return DiffEntry{}, false
}

// 两侧都有的函数：算差值、定方向、原始值都要带回去
func TestDiffTopFunctions_UpAndDown(t *testing.T) {
	baseline := []map[string]interface{}{
		topItem("burnCPU", 10, 100),
		topItem("idle", 60, 600),
	}
	compare := []map[string]interface{}{
		topItem("burnCPU", 45, 450),
		topItem("idle", 25, 250),
	}

	entries := diffTopFunctions(baseline, compare, 1)

	burn, ok := findEntry(entries, "burnCPU")
	if !ok {
		t.Fatal("burnCPU 应该出现在对比结果里")
	}
	if burn.Direction != "up" {
		t.Errorf("burnCPU 方向应为 up，实际 %q", burn.Direction)
	}
	if burn.DeltaPercentage != 35 {
		t.Errorf("burnCPU 差值应为 35，实际 %v", burn.DeltaPercentage)
	}
	if burn.BaselinePercentage != 10 || burn.ComparePercentage != 45 {
		t.Errorf("两侧原始占比应一并返回，实际 baseline=%v compare=%v",
			burn.BaselinePercentage, burn.ComparePercentage)
	}
	if burn.BaselineSamples != 100 || burn.CompareSamples != 450 {
		t.Errorf("两侧原始采样数应一并返回，实际 baseline=%v compare=%v",
			burn.BaselineSamples, burn.CompareSamples)
	}

	idle, ok := findEntry(entries, "idle")
	if !ok {
		t.Fatal("idle 应该出现在对比结果里")
	}
	if idle.Direction != "down" {
		t.Errorf("idle 方向应为 down，实际 %q", idle.Direction)
	}
	if idle.DeltaPercentage != -35 {
		t.Errorf("idle 差值应为 -35，实际 %v", idle.DeltaPercentage)
	}
}

// 只进了一侧 Top20 的函数：方向要能区分是哪一侧，缺失侧按 0 计
func TestDiffTopFunctions_OneSidedOnly(t *testing.T) {
	baseline := []map[string]interface{}{topItem("oldHotspot", 20, 200)}
	compare := []map[string]interface{}{topItem("newHotspot", 30, 300)}

	entries := diffTopFunctions(baseline, compare, 1)

	newOne, ok := findEntry(entries, "newHotspot")
	if !ok {
		t.Fatal("newHotspot 应该出现在对比结果里")
	}
	if newOne.Direction != "compare_only" {
		t.Errorf("newHotspot 方向应为 compare_only，实际 %q", newOne.Direction)
	}
	if newOne.BaselinePercentage != 0 || newOne.DeltaPercentage != 30 {
		t.Errorf("缺失侧应按 0 计，实际 baseline=%v delta=%v",
			newOne.BaselinePercentage, newOne.DeltaPercentage)
	}

	oldOne, ok := findEntry(entries, "oldHotspot")
	if !ok {
		t.Fatal("oldHotspot 应该出现在对比结果里")
	}
	if oldOne.Direction != "baseline_only" {
		t.Errorf("oldHotspot 方向应为 baseline_only，实际 %q", oldOne.Direction)
	}
	if oldOne.DeltaPercentage != -20 {
		t.Errorf("oldHotspot 差值应为 -20，实际 %v", oldOne.DeltaPercentage)
	}
}

// 自己跟自己比：所有差值为 0，应被阈值滤成空表而不是报错
func TestDiffTopFunctions_IdenticalSides(t *testing.T) {
	side := []map[string]interface{}{
		topItem("a", 50, 500),
		topItem("b", 50, 500),
	}

	if entries := diffTopFunctions(side, side, 1); len(entries) != 0 {
		t.Errorf("完全相同的两侧应无差异条目，实际 %d 条", len(entries))
	}
	// 阈值为 0 时不过滤，但差值仍应全是 0
	for _, e := range diffTopFunctions(side, side, 0) {
		if e.DeltaPercentage != 0 {
			t.Errorf("%s 差值应为 0，实际 %v", e.Function, e.DeltaPercentage)
		}
	}
}

// 阈值按百分点过滤噪声，边界值（正好等于阈值）应保留
func TestDiffTopFunctions_ThresholdFiltersNoise(t *testing.T) {
	baseline := []map[string]interface{}{
		topItem("noise", 10, 100),
		topItem("real", 10, 100),
	}
	compare := []map[string]interface{}{
		topItem("noise", 10.5, 105),
		topItem("real", 30, 300),
	}

	entries := diffTopFunctions(baseline, compare, 1)
	if _, ok := findEntry(entries, "noise"); ok {
		t.Error("差值 0.5 低于阈值 1，noise 应被滤掉")
	}
	if _, ok := findEntry(entries, "real"); !ok {
		t.Error("差值 20 高于阈值 1，real 应保留")
	}

	// |delta| == threshold 不算低于阈值，应保留
	if entries := diffTopFunctions(baseline, compare, 0.5); len(entries) != 2 {
		t.Errorf("阈值恰好等于差值时应保留，实际 %d 条", len(entries))
	}
}

// 排序：按差值绝对值降序，绝对值相同时按函数名升序，保证输出稳定
func TestDiffTopFunctions_SortStable(t *testing.T) {
	baseline := []map[string]interface{}{
		topItem("small", 10, 100),
		topItem("zebra", 10, 100),
		topItem("alpha", 10, 100),
	}
	compare := []map[string]interface{}{
		topItem("small", 13, 130),
		topItem("zebra", 30, 300),
		topItem("alpha", 30, 300),
	}

	entries := diffTopFunctions(baseline, compare, 1)
	if len(entries) != 3 {
		t.Fatalf("应有 3 条差异，实际 %d 条", len(entries))
	}
	// alpha 和 zebra 差值都是 20，按名字升序；small 差值 3 排最后
	want := []string{"alpha", "zebra", "small"}
	for i, name := range want {
		if entries[i].Function != name {
			t.Errorf("第 %d 条应为 %s，实际 %s", i, name, entries[i].Function)
		}
	}
}

// 脏数据不应让对比崩掉：空输入、函数名缺失、字段类型不对
func TestDiffTopFunctions_MalformedInput(t *testing.T) {
	if entries := diffTopFunctions(nil, nil, 1); len(entries) != 0 {
		t.Errorf("空输入应返回空结果，实际 %d 条", len(entries))
	}

	dirty := []map[string]interface{}{
		{"function": "", "percentage": 50.0},             // 函数名为空，跳过
		{"percentage": 50.0},                             // 没有 function 字段，跳过
		{"function": "typed", "percentage": "not-a-num"}, // 类型不对，按 0 处理
	}
	entries := diffTopFunctions(dirty, []map[string]interface{}{topItem("typed", 40, 400)}, 1)
	if len(entries) != 1 {
		t.Fatalf("只有 typed 一条有效，实际 %d 条", len(entries))
	}
	if entries[0].BaselinePercentage != 0 || entries[0].DeltaPercentage != 40 {
		t.Errorf("非数值字段应按 0 处理，实际 baseline=%v delta=%v",
			entries[0].BaselinePercentage, entries[0].DeltaPercentage)
	}
}

// 同名函数重复出现时取占比更高的一条，避免被后面的低值覆盖
func TestDiffTopFunctions_DuplicateFunctionKeepsHighest(t *testing.T) {
	baseline := []map[string]interface{}{
		topItem("dup", 40, 400),
		topItem("dup", 5, 50),
	}
	compare := []map[string]interface{}{topItem("dup", 10, 100)}

	entries := diffTopFunctions(baseline, compare, 1)
	if len(entries) != 1 {
		t.Fatalf("应只有 1 条，实际 %d 条", len(entries))
	}
	if entries[0].BaselinePercentage != 40 {
		t.Errorf("重复函数应保留占比更高的 40，实际 %v", entries[0].BaselinePercentage)
	}
	if math.Abs(entries[0].DeltaPercentage-(-30)) > 1e-9 {
		t.Errorf("差值应为 -30，实际 %v", entries[0].DeltaPercentage)
	}
}

func TestTaskDiffReadsActiveGenerationArtifacts(t *testing.T) {
	s := newTestAPIServer(t)
	store := newContinuousMemoryStorage()
	s.Storage = store
	now := time.Now()
	baseTask := model.HotmethodTask{TID: "generation-base", Name: "base", Status: TaskStatusDone, AnalysisStatus: 2, CreateTime: now}
	compareTask := model.HotmethodTask{TID: "generation-compare", Name: "compare", Status: TaskStatusDone, AnalysisStatus: 2, CreateTime: now}
	if err := s.DB.Create(&baseTask).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Create(&compareTask).Error; err != nil {
		t.Fatal(err)
	}
	baseJob := model.AnalysisJob{TaskTID: baseTask.TID, Pipeline: "perf_flamegraph", Status: model.AnalysisJobStatusSuccess, Generation: 1, CreatedAt: now, UpdatedAt: now}
	compareJob := model.AnalysisJob{TaskTID: compareTask.TID, Pipeline: "perf_flamegraph", Status: model.AnalysisJobStatusSuccess, Generation: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.DB.Create(&baseJob).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Create(&compareJob).Error; err != nil {
		t.Fatal(err)
	}
	baseTask.ActiveAnalysisJobID = &baseJob.ID
	compareTask.ActiveAnalysisJobID = &compareJob.ID
	if err := s.DB.Model(&baseTask).Update("active_analysis_job_id", baseJob.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Model(&compareTask).Update("active_analysis_job_id", compareJob.ID).Error; err != nil {
		t.Fatal(err)
	}

	type artifactInput struct {
		task *model.HotmethodTask
		job  *model.AnalysisJob
		name string
		body string
	}
	inputs := []artifactInput{
		{&baseTask, &baseJob, "top.json", `{"self_time_top":[{"function":"hot","percentage":20,"samples":20}]}`},
		{&compareTask, &compareJob, "top.json", `{"self_time_top":[{"function":"hot","percentage":70,"samples":70}]}`},
		{&baseTask, &baseJob, "folded.txt", "main;hot 2\n"},
		{&compareTask, &compareJob, "folded.txt", "main;hot 7\n"},
	}
	for _, in := range inputs {
		key := "tasks/" + in.task.TID + "/analysis/perf_flamegraph/test/generation/" + in.name
		jobID := in.job.ID
		artifact := model.Artifact{
			TaskTID: in.task.TID, AnalysisJobID: &jobID, Kind: model.ArtifactKindResult,
			ObjectKey: key, LogicalName: in.name, Status: model.ArtifactStatusReady,
			Compression: model.CompressionGzip, CreatedAt: now,
		}
		if in.name == "folded.txt" {
			artifact.Kind = model.ArtifactKindIntermediate
		}
		if err := s.DB.Create(&artifact).Error; err != nil {
			t.Fatal(err)
		}
		store.objects[key] = string(mustGzip(t, []byte(in.body)))
		store.modified[key] = now
	}

	baseTop := s.fetchTopFunctionsForTask(&baseTask)
	compareTop := s.fetchTopFunctionsForTask(&compareTask)
	entries := diffTopFunctions(baseTop, compareTop, 1)
	if len(entries) != 1 || entries[0].Function != "hot" || entries[0].DeltaPercentage != 50 {
		t.Fatalf("generation-scoped top diff mismatch: %#v", entries)
	}

	flame, reason := s.buildTaskDiffFlamegraph(&baseTask, &compareTask, 100)
	if reason != "" {
		t.Fatalf("generation-scoped folded artifacts should be readable: %s", reason)
	}
	if flame.Empty || flame.BaseTotal != 2 || flame.CompareTotal != 7 || len(flame.Root.Children) == 0 {
		t.Fatalf("generation-scoped flame diff mismatch: %#v", flame)
	}
}
