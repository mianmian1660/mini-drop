package model

import (
	"testing"
	"time"
)

func TestHotmethodTaskFields(t *testing.T) {
	now := time.Now()
	task := HotmethodTask{
		TID: "test-tid", Name: "CPU采样", Type: 0, ProfilerType: 3,
		TargetIP: "10.0.0.1", Status: 0, StatusInfo: "新建",
		AnalysisStatus: 0, UID: "u1", UserName: "测试",
		MasterTaskTID: "sch-001", CreateTime: now,
	}
	if task.TID != "test-tid" || task.Name != "CPU采样" {
		t.Error("basic fields mismatch")
	}
	if task.ProfilerType != 3 {
		t.Error("eBPF profilerType should be 3")
	}
	if task.MasterTaskTID != "sch-001" {
		t.Error("master_task_tid mismatch")
	}
}

func TestAgentInfoFields(t *testing.T) {
	a := AgentInfo{Hostname: "h1", IPAddr: "10.0.0.1", Online: true, UID: "a1", Version: "1.0"}
	if !a.Online || a.Hostname != "h1" || a.IPAddr != "10.0.0.1" {
		t.Error("AgentInfo fields mismatch")
	}
}

func TestUserInfoFields(t *testing.T) {
	u := UserInfo{UID: "u1", Name: "test"}
	if u.UID != "u1" || u.Name != "test" {
		t.Error("UserInfo fields mismatch")
	}
}

func TestScheduleTaskFields(t *testing.T) {
	s := ScheduleTask{SID: "sch-1", Name: "cp", CronExpr: "*/5 * * * *", Enabled: true}
	if s.SID != "sch-1" || s.CronExpr != "*/5 * * * *" || !s.Enabled {
		t.Error("ScheduleTask fields mismatch")
	}
}

func TestGroupFields(t *testing.T) {
	g := Group{GID: "g1", Name: "team", OwnerID: "u1"}
	if g.GID != "g1" || g.Name != "team" {
		t.Error("Group fields mismatch")
	}
}

func TestGroupMemberFields(t *testing.T) {
	gm := GroupMember{GID: "g1", UID: "u2"}
	if gm.GID != "g1" || gm.UID != "u2" {
		t.Error("GroupMember fields mismatch")
	}
}

func TestAnalysisSuggestionFields(t *testing.T) {
	as := AnalysisSuggestion{TID: "t1", Func: "malloc", Suggestion: "use jemalloc"}
	if as.TID != "t1" || as.Func != "malloc" {
		t.Error("AnalysisSuggestion fields mismatch")
	}
}

func TestMultiTaskFields(t *testing.T) {
	mt := MultiTask{TID: "mt1", Type: 0}
	if mt.TID != "mt1" {
		t.Error("MultiTask fields mismatch")
	}
}

func TestStatusConstants(t *testing.T) {
	task := HotmethodTask{}
	statuses := []int{0, 1, 2, 3, 4}
	for _, s := range statuses {
		task.Status = s
		if task.Status != s {
			t.Error("status set/get mismatch")
		}
	}
}

func TestProfilerTypes(t *testing.T) {
	task := HotmethodTask{}
	for _, pt := range []uint32{0, 1, 2, 3} {
		task.ProfilerType = pt
		if task.ProfilerType != pt {
			t.Errorf("profilerType %d mismatch", pt)
		}
	}
}

func TestMasterTaskTIDLink(t *testing.T) {
	child := HotmethodTask{TID: "c1", MasterTaskTID: "sch-001"}
	if child.MasterTaskTID != "sch-001" {
		t.Error("child task should link to schedule")
	}
	standalone := HotmethodTask{TID: "s1"}
	if standalone.MasterTaskTID != "" {
		t.Error("standalone task should have empty master_task_tid")
	}
}
