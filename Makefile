SHELL := /bin/bash

API ?= http://localhost:8191
USER_UID ?= demo
USER_NAME ?= demo
TARGET_IP ?= 127.0.0.1
DURATION ?= 6
FREQUENCY ?= 99

.PHONY: demo demo-cpu demo-ebpf-io demo-ebpf-sched health test e2e verify

health:
	@echo "[health] API: $(API)"
	@curl -fsS "$(API)/healthz" || curl -fsS "$(API)/health"
	@echo

demo: health demo-cpu demo-ebpf-io demo-ebpf-sched
	@echo
	@echo "[demo] 已创建 CPU、eBPF IO、eBPF 调度三个演示任务。"
	@echo "[demo] 打开 http://localhost/ 查看任务列表，或进入上面输出的 /task/result?tid=... 页面。"

demo-cpu:
	@echo "[demo-cpu] 创建 perf CPU 火焰图任务"
	@RESP=$$(curl -fsS -X POST "$(API)/api/v1/tasks" \
		-H "Content-Type: application/json" \
		-H "Drop_user_uid: $(USER_UID)" \
		-H "Drop_user_name: $(USER_NAME)" \
		-d '{"name":"make demo - CPU flamegraph","task_type":0,"profiler_type":0,"target_ip":"$(TARGET_IP)","target_pid":0,"duration":$(DURATION),"frequency":$(FREQUENCY),"callgraph":"fp","event":"cpu-cycles"}'); \
		echo "$$RESP"; \
		TID=$$(printf '%s' "$$RESP" | sed -n 's/.*"tid":"\([^"]*\)".*/\1/p'); \
		if [[ -n "$$TID" ]]; then echo "[demo-cpu] result: http://localhost/task/result?tid=$$TID"; fi

demo-ebpf-io:
	@echo "[demo-ebpf-io] 制造一次短 IO 写入并创建 eBPF IO 直方图任务"
	@(dd if=/dev/zero of=/tmp/mini-drop-demo-io.dat bs=1M count=128 conv=fsync >/tmp/mini-drop-demo-io.log 2>&1 || true) &
	@RESP=$$(curl -fsS -X POST "$(API)/api/v1/tasks" \
		-H "Content-Type: application/json" \
		-H "Drop_user_uid: $(USER_UID)" \
		-H "Drop_user_name: $(USER_NAME)" \
		-d '{"name":"make demo - eBPF IO histogram","task_type":5,"profiler_type":3,"target_ip":"$(TARGET_IP)","target_pid":0,"duration":$(DURATION),"frequency":1,"callgraph":"fp","event":"io"}'); \
		echo "$$RESP"; \
		TID=$$(printf '%s' "$$RESP" | sed -n 's/.*"tid":"\([^"]*\)".*/\1/p'); \
		if [[ -n "$$TID" ]]; then echo "[demo-ebpf-io] result: http://localhost/task/result?tid=$$TID"; fi

demo-ebpf-sched:
	@echo "[demo-ebpf-sched] 制造一次短 CPU 忙等并创建 eBPF 调度直方图任务"
	@(timeout 8s bash -c 'while :; do :; done' >/dev/null 2>&1 || true) &
	@RESP=$$(curl -fsS -X POST "$(API)/api/v1/tasks" \
		-H "Content-Type: application/json" \
		-H "Drop_user_uid: $(USER_UID)" \
		-H "Drop_user_name: $(USER_NAME)" \
		-d '{"name":"make demo - eBPF sched histogram","task_type":5,"profiler_type":3,"target_ip":"$(TARGET_IP)","target_pid":0,"duration":$(DURATION),"frequency":1,"callgraph":"fp","event":"sched"}'); \
		echo "$$RESP"; \
		TID=$$(printf '%s' "$$RESP" | sed -n 's/.*"tid":"\([^"]*\)".*/\1/p'); \
		if [[ -n "$$TID" ]]; then echo "[demo-ebpf-sched] result: http://localhost/task/result?tid=$$TID"; fi

test:
	$(MAKE) -C drop/build
	cd apiserver && go test ./...
	cd analysis && python3 test_analysis.py
	cd web_frontend && npm run build

e2e:
	bash scripts/e2e_smoke.sh

verify: test
	git diff --check
