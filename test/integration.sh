#!/bin/bash
# 集成测试：用 mock claude 验证 调度顺序 / 限额暂停+冷却 / 自动续跑 / 装配产出入队 / 完成后自动审核
# / 模型路由 / 进度自动回收 / 分工协调（实时快照注入 + 带模型入队）/ cancel 击杀在跑进程组并释放目录。
set -euo pipefail
cd "$(dirname "$0")/.."

go build -o bin/claudego .
BIN="$PWD/bin/claudego"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
export CLAUDEGO_ROOT="$TMP/root"
export MOCK_DIR="$TMP/mock"
mkdir -p "$MOCK_DIR"
PROJ="$TMP/proj" && mkdir -p "$PROJ"

pass=0; fail=0
assert() { # assert <描述> <python 表达式，tasks 为任务列表>
  local desc="$1" expr="$2"
  if "$BIN" list -json | python3 -c "
import json,sys,time
tasks=json.load(sys.stdin) or []
byid={t['id']:t for t in tasks}
def one(**kw):
    m=[t for t in tasks if all(t.get(k)==v for k,v in kw.items())]
    assert len(m)==1, f'expect 1 match for {kw}, got {len(m)}'
    return m[0]
now=time.time()
assert $expr
"; then
    echo "  ✔ $desc"; pass=$((pass+1))
  else
    echo "  ✖ $desc"; fail=$((fail+1))
  fi
}

echo "== init =="
"$BIN" init >/dev/null
python3 - "$CLAUDEGO_ROOT/config.json" "$PWD/test/mock-claude.sh" <<'EOF'
import json,sys
p,mock=sys.argv[1],sys.argv[2]
cfg=json.load(open(p))
cfg["claude_bin"]=mock
cfg["limit_fallback_min"]=1
cfg["retry_backoff_min"]=1
json.dump(cfg,open(p,"w"),indent=2,ensure_ascii=False)
EOF
chmod +x test/mock-claude.sh test/mock-codex.sh

echo "== 场景1+2: 优先级顺序 + 单次 run 排空 + 第2步撞限额冷却 =="
"$BIN" add -dir "$PROJ" -title low-prio -priority 1 -file /dev/stdin <<'EOF' >/dev/null
step one of A
---
step two of A
EOF
"$BIN" review -dir "$PROJ" -priority 5 -title high-review "focus" >/dev/null
printf 'ok\nok\nlimit\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" run -quiet
assert "高优先级 review 完成" "one(title='high-review')['status']=='done'"
L1=$(grep -n "审核关注点：focus" "$MOCK_DIR/calls.log" | head -1 | cut -d: -f1)
L2=$(grep -n "step one of A" "$MOCK_DIR/calls.log" | head -1 | cut -d: -f1)
[ -n "$L1" ] && [ -n "$L2" ] && [ "$L1" -lt "$L2" ] && echo "  ✔ 高优先级先于低优先级派发" && pass=$((pass+1)) || { echo "  ✖ 派发顺序错误"; fail=$((fail+1)); }
assert "低优先级任务在同一次 run 内接续执行并撞限额暂停" "one(title='low-prio')['status']=='limit_paused'"
assert "步骤停在 1/2 且标记中断" "one(title='low-prio')['step']==1 and one(title='low-prio')['mid_step']==True"
assert "resume_at 在未来约1小时内" "0 < one(title='low-prio')['resume_at_epoch']-now < 3700"
test -f "$CLAUDEGO_ROOT/cooldown.json" && echo "  ✔ 冷却文件已写入" && pass=$((pass+1)) || { echo "  ✖ 缺少冷却文件"; fail=$((fail+1)); }

echo "== 场景3: 冷却期内不派发 =="
calls_before=$(cat "$MOCK_DIR/n")
"$BIN" run -quiet
calls_after=$(cat "$MOCK_DIR/n")
if [ "$calls_before" = "$calls_after" ]; then echo "  ✔ 冷却期内未调用 claude"; pass=$((pass+1)); else echo "  ✖ 冷却期内仍调用了 claude"; fail=$((fail+1)); fi

echo "== 场景4: 到点自动续跑（同会话 --resume + 续跑提示）=="
python3 - "$CLAUDEGO_ROOT" <<'EOF'
import json,glob,sys,time,os
root=sys.argv[1]
past=int(time.time())-5
os.remove(os.path.join(root,"cooldown.json"))
for f in glob.glob(os.path.join(root,"tasks","*.json")):
    t=json.load(open(f))
    if t["status"]=="limit_paused":
        t["resume_at_epoch"]=past
        json.dump(t,open(f,"w"),ensure_ascii=False)
EOF
printf 'ok\nok\nlimit\nok\n' > "$MOCK_DIR/plan"
"$BIN" run -quiet
assert "续跑后任务完成" "one(title='low-prio')['status']=='done' and one(title='low-prio')['step']==2"
grep -q -- "--resume sess-2" "$MOCK_DIR/calls.log" && echo "  ✔ 使用 --resume 续接原会话" && pass=$((pass+1)) || { echo "  ✖ 未用 --resume 续接"; fail=$((fail+1)); }
grep -q "上一条指令因为用量限额被中断" "$MOCK_DIR/calls.log" && echo "  ✔ 发送了续跑提示而非重发原 prompt" && pass=$((pass+1)) || { echo "  ✖ 未发送续跑提示"; fail=$((fail+1)); }

echo "== 场景5: 装配产出入队 → 同一次 run 内接力执行 → review_after 链 =="
printf 'emit\nok\nok\nok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" assemble -dir "$PROJ" -title assembly-1 "build a thing" >/dev/null
"$BIN" run -quiet   # 排空：装配 → 产出任务 2 步 → 自动审核，一气呵成
assert "装配任务完成" "one(title='assembly-1')['status']=='done'"
assert "产出任务被接力执行完成且带 review_after" "one(title='emitted-task')['status']=='done' and one(title='emitted-task')['review_after']==True and len(one(title='emitted-task')['prompts'])==2"
assert "review_after 审核任务也在同轮完成" "one(title='审核: emitted-task')['status']=='done' and one(title='审核: emitted-task')['type']=='design-review'"

echo "== 场景6: 失败重试与退避 =="
printf 'err\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" add -dir "$PROJ" -title flaky -priority 9 "do flaky" >/dev/null
"$BIN" run -quiet
assert "失败后回到排队并带退避时间" "one(title='flaky')['status']=='queued' and one(title='flaky')['attempts']==1 and one(title='flaky')['not_before_epoch']>now"

echo "== 场景7: 任务级模型路由（--model 透传）=="
printf 'ok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" add -dir "$PROJ" -title with-model -priority 10 -model sonnet "modeled step" >/dev/null
"$BIN" run -quiet
assert "指定模型的任务完成且记录模型" "one(title='with-model')['status']=='done' and one(title='with-model')['model']=='sonnet'"
grep -q -- "--model sonnet" "$MOCK_DIR/calls.log" && echo "  ✔ 向 claude 传递了 --model sonnet" && pass=$((pass+1)) || { echo "  ✖ 未传递 --model"; fail=$((fail+1)); }

echo "== 场景8: brief -auto 自动回收会话进度（haiku + --resume + 落盘）=="
printf 'progress\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" brief -session sess-42 -dir "$PROJ" -title probe -auto >/dev/null
"$BIN" run -quiet
assert "回收任务完成且默认 haiku" "one(title='进度: probe')['status']=='done' and one(title='进度: probe')['type']=='progress-pull' and one(title='进度: probe')['model']=='haiku'"
grep -q -- "--resume sess-42" "$MOCK_DIR/calls.log" && echo "  ✔ 回收任务续接了目标会话" && pass=$((pass+1)) || { echo "  ✖ 未 --resume 目标会话"; fail=$((fail+1)); }
grep -q -- "--model haiku" "$MOCK_DIR/calls.log" && echo "  ✔ 回收任务用 haiku 模型" && pass=$((pass+1)) || { echo "  ✖ 未用 haiku"; fail=$((fail+1)); }
test -f "$CLAUDEGO_ROOT/progress/s-sess-42.json" && echo "  ✔ 进度报告已落盘" && pass=$((pass+1)) || { echo "  ✖ 进度报告未落盘"; fail=$((fail+1)); }
plist=$("$BIN" progress)
echo "$plist" | grep -q "s-sess-42" && echo "  ✔ progress 列表可见" && pass=$((pass+1)) || { echo "  ✖ progress 列表不可见"; fail=$((fail+1)); }
# 8b: 会话不按 JSON 格式回复时，原文兜底落盘（不丢已花额度换来的汇报）
printf 'ok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" brief -session sess-77 -dir "$PROJ" -title rawprobe -auto >/dev/null
"$BIN" run -quiet
assert "非 JSON 汇报的回收任务不失败" "one(title='进度: rawprobe')['status']=='done'"
"$BIN" brief -session sess-88 -dir "$PROJ" -title modelprobe -model sonnet -auto >/dev/null
assert "brief -model 覆盖回收模型" "one(title='进度: modelprobe')['model']=='sonnet'"
"$BIN" cancel "$("$BIN" list -json | python3 -c "import json,sys;print([t['id'] for t in json.load(sys.stdin) if t['title']=='进度: modelprobe'][0])")" >/dev/null
grep -q '"raw"' "$CLAUDEGO_ROOT/progress/s-sess-77.json" && echo "  ✔ 散文汇报以原文兜底落盘" && pass=$((pass+1)) || { echo "  ✖ 原文兜底未生效"; fail=$((fail+1)); }

echo "== 场景9: plan 分工协调（实时快照注入 + 产出任务带模型自动入队）=="
fid=$("$BIN" list -json | python3 -c "import json,sys;print([t['id'] for t in json.load(sys.stdin) if t['title']=='flaky'][0])")
"$BIN" cancel "$fid" >/dev/null
printf 'coord\nok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" plan -dir "$PROJ" -priority 10 -title coord-plan "把剩余工作分工推进" >/dev/null
"$BIN" run -quiet   # 排空：协调 → 产出任务接力执行
assert "协调任务完成（默认最强模型 opus）" "one(title='coord-plan')['status']=='done' and one(title='coord-plan')['type']=='coordinate' and one(title='coord-plan')['model']=='opus'"
grep -q "目标X" "$MOCK_DIR/calls.log" && echo "  ✔ 协调 prompt 注入了进度报告" && pass=$((pass+1)) || { echo "  ✖ 未注入进度报告"; fail=$((fail+1)); }
grep -q "{{QUEUE}}" "$MOCK_DIR/calls.log" && { echo "  ✖ 队列占位符未被替换"; fail=$((fail+1)); } || { echo "  ✔ 队列快照已实时注入"; pass=$((pass+1)); }
assert "分工产出任务接力完成且带模型" "one(title='coord-task')['status']=='done' and one(title='coord-task')['model']=='haiku'"
grep -q -- "--model haiku" "$MOCK_DIR/calls.log" && echo "  ✔ 分工任务按建议模型执行" && pass=$((pass+1)) || { echo "  ✖ 分工任务未按模型执行"; fail=$((fail+1)); }

echo "== 场景10: progress -in 手动导入 与 cmd 手动接管 =="
echo '{"goal":"g2","done":["x"],"remaining":["y"]}' | "$BIN" progress -in -key manual-1 >/dev/null
plist=$("$BIN" progress)
echo "$plist" | grep -q "manual-1" && echo "  ✔ 手动导入进度可见" && pass=$((pass+1)) || { echo "  ✖ 手动导入失败"; fail=$((fail+1)); }
cid=$("$BIN" list -json | python3 -c "import json,sys;print([t['id'] for t in json.load(sys.stdin) if t['title']=='coord-task'][0])")
cout=$("$BIN" cmd "$cid")
echo "$cout" | grep -q -- "--model haiku" && echo "  ✔ cmd 输出手动接管命令（含模型）" && pass=$((pass+1)) || { echo "  ✖ cmd 输出缺少模型"; fail=$((fail+1)); }

echo "== 场景11: sessions 发现桌面端/CLI 会话（共用 ~/.claude/projects）=="
CCHOME="$TMP/cchome"
ENC=$(printf '%s' "$PROJ" | sed 's/[^a-zA-Z0-9]/-/g')
mkdir -p "$CCHOME/projects/$ENC"
cat > "$CCHOME/projects/$ENC/aaaa-bbbb-cccc.jsonl" <<'EOF'
{"type":"queue-operation","operation":"enqueue","sessionId":"aaaa-bbbb-cccc"}
{"parentUuid":null,"type":"user","message":{"role":"user","content":"给上传模块加断点续传"},"uuid":"u1"}
EOF
out=$(CLAUDE_CONFIG_DIR="$CCHOME" "$BIN" sessions -dir "$PROJ")
echo "$out" | grep -q "aaaa-bbbb-cccc" && echo "  ✔ 列出会话 ID" && pass=$((pass+1)) || { echo "  ✖ 未列出会话"; fail=$((fail+1)); }
echo "$out" | grep -q "断点续传" && echo "  ✔ 解析首条用户消息作标题" && pass=$((pass+1)) || { echo "  ✖ 标题解析失败"; fail=$((fail+1)); }
echo "$out" | grep -q "brief -session" && echo "  ✔ 给出接管命令提示" && pass=$((pass+1)) || { echo "  ✖ 缺少命令提示"; fail=$((fail+1)); }

echo "== 场景12: review/assemble 挂到既有角色会话（-session）=="
"$BIN" review -dir "$PROJ" -session role-review-1 -title attach-review "增量审查" >/dev/null
"$BIN" assemble -dir "$PROJ" -session role-asm-1 -title attach-asm "下一轮目标" >/dev/null
assert "审核任务挂上既有审核会话" "one(title='attach-review')['session_id']=='role-review-1'"
assert "装配任务挂上既有装配会话" "one(title='attach-asm')['session_id']=='role-asm-1'"

echo "== 场景13: 队列预算红线（本地加权账本）=="
python3 - "$CLAUDEGO_ROOT/config.json" <<'EOF'
import json,sys
p=sys.argv[1]; c=json.load(open(p)); c["queue_budget_tokens"]=1
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
EOF
printf 'ok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" run -quiet
[ "$(cat "$MOCK_DIR/n")" = "0" ] && echo "  ✔ 红线阻止派发（未调用 claude）" && pass=$((pass+1)) || { echo "  ✖ 红线未生效"; fail=$((fail+1)); }
lout=$("$BIN" list); echo "$lout" | grep -q "额度红线" && echo "  ✔ 看板显示红线横幅" && pass=$((pass+1)) || { echo "  ✖ 看板无红线提示"; fail=$((fail+1)); }
qout=$("$BIN" quota); echo "$qout" | grep -q "红线生效" && echo "  ✔ quota 显示触线状态" && pass=$((pass+1)) || { echo "  ✖ quota 未显示触线"; fail=$((fail+1)); }
"$BIN" run -quiet -force
[ "$(cat "$MOCK_DIR/n")" = "2" ] && echo "  ✔ -force 可越线并排空（attach 两任务都跑了）" && pass=$((pass+1)) || { echo "  ✖ -force 越线执行数不对（$(cat "$MOCK_DIR/n")）"; fail=$((fail+1)); }

echo "== 场景14: 外部用量源红线（CodexBar usage-history 格式）=="
FEED="$TMP/usage-history.jsonl"
NOWISO=$(python3 -c "import datetime;print(datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ'))")
printf '{"provider":"claude","sampledAt":"%s","resetsAt":"2026-12-31T00:00:00Z","usedPercent":93,"windowKind":"primary","windowMinutes":300}\n' "$NOWISO" > "$FEED"
python3 - "$CLAUDEGO_ROOT/config.json" "$FEED" <<'EOF'
import json,sys
p,feed=sys.argv[1],sys.argv[2]; c=json.load(open(p))
c["queue_budget_tokens"]=0; c["redline_percent"]=90; c["usage_feed"]=feed
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
EOF
"$BIN" add -dir "$PROJ" -title feed-task -priority 2 "ft" >/dev/null
printf 'ok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" run -quiet
[ "$(cat "$MOCK_DIR/n")" = "0" ] && echo "  ✔ 全局用量 93%≥90% 阻止派发" && pass=$((pass+1)) || { echo "  ✖ 全局红线未生效"; fail=$((fail+1)); }
printf '{"provider":"claude","sampledAt":"%s","resetsAt":"2026-12-31T00:00:00Z","usedPercent":50,"windowKind":"primary","windowMinutes":300}\n' "$NOWISO" > "$FEED"
"$BIN" run -quiet
[ "$(cat "$MOCK_DIR/n")" = "1" ] && echo "  ✔ 用量降到 50% 后恢复派发" && pass=$((pass+1)) || { echo "  ✖ 低于红线未放行"; fail=$((fail+1)); }

echo "== 场景15: 分时段红线（每日窗口内覆盖阈值，窗口外回落全局）=="
"$BIN" add -dir "$PROJ" -title windowed -priority 3 "wnd step" >/dev/null
IN_FROM=$(python3 -c "import datetime;print((datetime.datetime.now()-datetime.timedelta(minutes=10)).strftime('%H:%M'))")
IN_TO=$(python3 -c "import datetime;print((datetime.datetime.now()+datetime.timedelta(minutes=50)).strftime('%H:%M'))")
python3 - "$CLAUDEGO_ROOT/config.json" "$IN_FROM" "$IN_TO" <<'EOF'
import json,sys
p,f,t=sys.argv[1:4]; c=json.load(open(p))
c["queue_budget_tokens"]=0; c["redline_percent"]=0; c["usage_feed"]=""
c["redline_windows"]=[{"from":f,"to":t,"queue_budget_tokens":1}]
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
EOF
printf 'ok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" run -quiet
[ "$(cat "$MOCK_DIR/n")" = "0" ] && echo "  ✔ 时段内红线阻止派发" && pass=$((pass+1)) || { echo "  ✖ 时段内未阻止"; fail=$((fail+1)); }
qout=$("$BIN" quota); echo "$qout" | grep -q "当前时段生效" && echo "  ✔ quota 标记当前时段" && pass=$((pass+1)) || { echo "  ✖ quota 未标记时段"; fail=$((fail+1)); }
OUT_FROM=$(python3 -c "import datetime;print((datetime.datetime.now()+datetime.timedelta(hours=2)).strftime('%H:%M'))")
OUT_TO=$(python3 -c "import datetime;print((datetime.datetime.now()+datetime.timedelta(hours=3)).strftime('%H:%M'))")
python3 - "$CLAUDEGO_ROOT/config.json" "$OUT_FROM" "$OUT_TO" <<'EOF'
import json,sys
p,f,t=sys.argv[1:4]; c=json.load(open(p))
c["redline_windows"]=[{"from":f,"to":t,"queue_budget_tokens":1}]
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
EOF
"$BIN" run -quiet
assert "时段外回落全局（无红线）正常执行" "one(title='windowed')['status']=='done'"

echo "== 场景16: 单次 run 排空队列 + 跨目录并行 =="
PROJ2="$TMP/proj2" && mkdir -p "$PROJ2"
python3 - "$CLAUDEGO_ROOT/config.json" <<'EOF'
import json,sys
p=sys.argv[1]; c=json.load(open(p))
c["max_parallel"]=2; c["redline_windows"]=[]
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
EOF
printf 'slow\nslow\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" add -dir "$PROJ"  -title par-a -priority 2 "pa" >/dev/null
"$BIN" add -dir "$PROJ2" -title par-b -priority 2 "pb" >/dev/null
T0=$(python3 -c "import time;print(time.time())")
"$BIN" run -quiet
EL=$(python3 -c "import time,sys;print(time.time()-$T0)")
assert "一次 run 排空两个任务" "one(title='par-a')['status']=='done' and one(title='par-b')['status']=='done'"
python3 -c "import sys;sys.exit(0 if $EL < 2.2 else 1)" && echo "  ✔ 跨目录并行（${EL%.*}s < 2.2s）" && pass=$((pass+1)) || { echo "  ✖ 未并行（耗时 ${EL}s）"; fail=$((fail+1)); }

echo "== 场景17: 同目录强制串行 =="
printf 'slow\nslow\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" add -dir "$PROJ" -title ser-a -priority 2 "sa" >/dev/null
"$BIN" add -dir "$PROJ" -title ser-b -priority 2 "sb" >/dev/null
T0=$(python3 -c "import time;print(time.time())")
"$BIN" run -quiet
EL=$(python3 -c "import time,sys;print(time.time()-$T0)")
assert "同目录两任务都完成（仍在一次 run 内）" "one(title='ser-a')['status']=='done' and one(title='ser-b')['status']=='done'"
python3 -c "import sys;sys.exit(0 if $EL > 2.2 else 1)" && echo "  ✔ 同目录串行执行（${EL%.*}s > 2.2s）" && pass=$((pass+1)) || { echo "  ✖ 同目录被并行了（耗时 ${EL}s）"; fail=$((fail+1)); }

echo "== 场景18: claude 冷却期 codex 备用执行器接管单步任务 =="
python3 - "$CLAUDEGO_ROOT/config.json" "$PWD/test/mock-codex.sh" <<'EOF'
import json,sys,time
p,mock=sys.argv[1],sys.argv[2]
c=json.load(open(p))
c["codex_bin"]=mock; c["codex_fallback"]=True; c["codex_model"]="codex-test-model"
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
# 模拟 claude 撞限额：写入未来 1 小时的全局冷却
json.dump({"until_epoch":int(time.time())+3600,"reason":"mock limit","set_at":"t"},
          open(p.replace("config.json","cooldown.json"),"w"))
EOF
"$BIN" add -dir "$PROJ"  -title cx-single -priority 2 "single step work" >/dev/null
"$BIN" add -dir "$PROJ2" -title cx-multi -priority 9 -file /dev/stdin <<'EOF' >/dev/null
multi step one
---
multi step two
EOF
echo 0 > "$MOCK_DIR/n"
"$BIN" run -quiet
assert "单步任务被 codex 接管完成" "one(title='cx-single')['status']=='done' and one(title='cx-single')['runner']=='codex'"
assert "多步任务不切 codex，等待 claude 重置" "one(title='cx-multi')['status']=='queued'"
[ "$(cat "$MOCK_DIR/n")" = "0" ] && echo "  ✔ 冷却期未调用 claude" && pass=$((pass+1)) || { echo "  ✖ 冷却期调用了 claude"; fail=$((fail+1)); }
grep -q "sandbox read-only" "$MOCK_DIR/codex-calls.log" 2>/dev/null || grep -q -- "--sandbox workspace-write" "$MOCK_DIR/codex-calls.log" && echo "  ✔ codex 以受限沙箱执行" && pass=$((pass+1)) || { echo "  ✖ codex 沙箱参数缺失"; fail=$((fail+1)); }
grep -q "writable_roots.*\.git" "$MOCK_DIR/codex-calls.log" && echo "  ✔ codex 放行 .git 供收工 commit" && pass=$((pass+1)) || { echo "  ✖ 缺 .git writable_roots"; fail=$((fail+1)); }
rm -f "$CLAUDEGO_ROOT/cooldown.json"
printf 'ok\nok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" run -quiet
assert "冷却解除后多步任务由 claude 执行完成" "one(title='cx-multi')['status']=='done' and one(title='cx-multi').get('runner') is None"

echo "== 场景19: fresh_steps 每步全新会话 + emit -hold 人工把关 =="
RB=$(grep -c -- "--resume" "$MOCK_DIR/calls.log")
"$BIN" add -dir "$PROJ" -title ff -priority 2 -fresh -file /dev/stdin <<'EOF' >/dev/null
fresh step one
---
fresh step two
EOF
printf 'ok\nok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" run -quiet
assert "fresh_steps 任务完成且不留会话" "one(title='ff')['status']=='done' and one(title='ff').get('session_id') is None"
RA=$(grep -c -- "--resume" "$MOCK_DIR/calls.log")
[ "$RA" = "$RB" ] && echo "  ✔ 步骤间未使用 --resume（每步全新会话）" && pass=$((pass+1)) || { echo "  ✖ fresh_steps 仍复用了会话"; fail=$((fail+1)); }
"$BIN" assemble -dir "$PROJ2" -title asm-hold -hold "拆一个目标" >/dev/null
printf 'emit2\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" run -quiet
assert "-hold 产出任务挂起待审" "one(title='held-task')['status']=='held' and one(title='held-task')['fresh_steps']==True"
hid=$("$BIN" list -json | python3 -c "import json,sys;print([t['id'] for t in json.load(sys.stdin) if t['title']=='held-task'][0])")
"$BIN" release "$hid" >/dev/null
assert "release 放行进入排队" "one(title='held-task')['status']=='queued'"

echo "== 场景19: runner=codex 常态钉定（claude 空闲也走 codex）+ 思考等级透传 =="
python3 - "$CLAUDEGO_ROOT/config.json" <<'EOF'
import json,sys
p=sys.argv[1]; c=json.load(open(p))
c["thinking_tokens"]=12345; c["codex_reasoning"]="high"
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
EOF
printf 'ok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" add -dir "$PROJ"  -runner codex -title pin-codex -priority 2 "filler audit" >/dev/null
"$BIN" add -dir "$PROJ2" -title think-claude -priority 2 "think work" >/dev/null
"$BIN" run -quiet
assert "钉定任务在 claude 空闲时仍走 codex" "one(title='pin-codex')['status']=='done' and one(title='pin-codex')['runner']=='codex'"
assert "普通任务同轮由 claude 执行" "one(title='think-claude')['status']=='done' and one(title='think-claude').get('runner') is None"
grep -q "model_reasoning_effort=high" "$MOCK_DIR/codex-calls.log" && echo "  ✔ codex 推理等级已拉高" && pass=$((pass+1)) || { echo "  ✖ codex 推理等级未透传"; fail=$((fail+1)); }
grep -q "thinking=12345" "$MOCK_DIR/calls.log" && echo "  ✔ claude 思考预算已透传" && pass=$((pass+1)) || { echo "  ✖ MAX_THINKING_TOKENS 未透传"; fail=$((fail+1)); }
"$BIN" add -dir "$PROJ" -runner codex -title bad-pin -file /dev/stdin <<'EOF' >/dev/null 2>&1 && { echo "  ✖ 多步非 fresh 任务不该允许钉 codex"; fail=$((fail+1)); } || { echo "  ✔ 多步非 fresh 钉 codex 被拒绝"; pass=$((pass+1)); }
s1
---
s2
EOF

echo "== 场景20: 红线前置缓冲（踩线起跑防护，codex 钉定不受影响）=="
LEAD_FROM=$(python3 -c "import datetime;print((datetime.datetime.now()+datetime.timedelta(minutes=10)).strftime('%H:%M'))")
LEAD_TO=$(python3 -c "import datetime;print((datetime.datetime.now()+datetime.timedelta(minutes=70)).strftime('%H:%M'))")
python3 - "$CLAUDEGO_ROOT/config.json" "$LEAD_FROM" "$LEAD_TO" <<'EOF'
import json,sys
p,f,t=sys.argv[1:4]; c=json.load(open(p))
c["redline_windows"]=[{"from":f,"to":t,"queue_budget_tokens":1}]
c["redline_lead_min"]=15
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
EOF
printf 'ok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
: > "$MOCK_DIR/codex-calls.log"
"$BIN" add -dir "$PROJ" -title lead-claude -priority 2 "lead work" >/dev/null
"$BIN" add -dir "$PROJ2" -runner codex -title lead-codex -priority 2 "lead filler" >/dev/null
"$BIN" run -quiet
[ "$(cat "$MOCK_DIR/n")" = "0" ] && echo "  ✔ 缓冲期内 claude 任务不起跑" && pass=$((pass+1)) || { echo "  ✖ 缓冲期未生效"; fail=$((fail+1)); }
assert "codex 钉定任务缓冲期照跑" "one(title='lead-codex')['status']=='done' and one(title='lead-codex')['runner']=='codex'"
qout=$("$BIN" quota); echo "$qout" | grep -q "缓冲期生效中" && echo "  ✔ quota 显示缓冲状态" && pass=$((pass+1)) || { echo "  ✖ quota 未显示缓冲"; fail=$((fail+1)); }
python3 - "$CLAUDEGO_ROOT/config.json" <<'EOF'
import json,sys
p=sys.argv[1]; c=json.load(open(p)); c["redline_lead_min"]=0
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
EOF
"$BIN" run -quiet
assert "关闭缓冲后 claude 任务正常执行" "one(title='lead-claude')['status']=='done'"

echo "== 场景21: 设计模型不降级（no_fallback_models）=="
python3 - "$CLAUDEGO_ROOT/config.json" <<'EOF'
import json,sys,time
p=sys.argv[1]; c=json.load(open(p))
json.dump({"until_epoch":int(time.time())+3600,"reason":"mock limit","set_at":"t"},
          open(p.replace("config.json","cooldown.json"),"w"))
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
EOF
echo 0 > "$MOCK_DIR/n"
"$BIN" add -dir "$PROJ"  -model claude-fable-5 -title nf-fable -priority 2 "design work" >/dev/null
"$BIN" add -dir "$PROJ2" -model sonnet -title nf-sonnet -priority 2 "impl work" >/dev/null
"$BIN" run -quiet
assert "fable 设计卡冷却期不降级,继续排队" "one(title='nf-fable')['status']=='queued'"
assert "sonnet 卡照常 fallback 到 codex" "one(title='nf-sonnet')['status']=='done' and one(title='nf-sonnet')['runner']=='codex'"
rm -f "$CLAUDEGO_ROOT/cooldown.json"
printf 'ok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" run -quiet
assert "冷却解除后 fable 卡由 claude 执行" "one(title='nf-fable')['status']=='done' and one(title='nf-fable').get('runner') is None"

echo "== 场景22: 协调链(coordinate 产出 coordinate,自愈式续排) =="
printf 'coordchain\nok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" add -dir "$PROJ" -type coordinate -title chain-root -priority 2 "root planning" >/dev/null
"$BIN" run -quiet
assert "链根完成且产出下一张协调卡" "one(title='chain-root')['status']=='done' and one(title='chain-next')['type']=='coordinate'"
assert "产出的协调卡具备 emit 能力" "one(title='chain-next')['emit_tasks']==True"

echo "== 场景22: 只读审核卡与同仓写者并行（写者×2 仍串行由场景17保证）=="
printf 'slow\nslow\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" add -dir "$PROJ" -title par-writer -priority 2 "write work" >/dev/null
"$BIN" add -dir "$PROJ" -type design-review -title par-review -priority 2 "review work" >/dev/null
T0=$(python3 -c "import time;print(time.time())")
"$BIN" run -quiet
EL=$(python3 -c "import time,sys;print(time.time()-$T0)")
assert "同仓审核与写者都完成" "one(title='par-writer')['status']=='done' and one(title='par-review')['status']=='done'"
python3 -c "import sys;sys.exit(0 if $EL < 2.2 else 1)" && echo "  ✔ 只读审核并行不受目录互斥（${EL%.*}s < 2.2s）" && pass=$((pass+1)) || { echo "  ✖ 审核被目录互斥挡住（耗时 ${EL}s）"; fail=$((fail+1)); }

echo "== 场景23: emit 自造未知类型回退 sequence 并烘焙权限 =="
printf 'coord_badtype\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" plan -dir "$PROJ" -priority 9 -title bt-coord "自造类型分工" >/dev/null
"$BIN" run -quiet
assert "自造 batch 类型被回退为 sequence" "one(title='badtype-task')['type']=='sequence'"
assert "回退后烘焙了 sequence 权限（非空工具）" "len(one(title='badtype-task').get('allowed_tools') or [])>0 and one(title='badtype-task').get('permission_mode')=='acceptEdits'"

echo "== 场景24: 远端执行器（SSH → 远端 codex；prompt 走 stdin + marker 回捕；非零退出不误判）=="
python3 - "$CLAUDEGO_ROOT/config.json" "$PWD/test/mock-ssh.sh" <<'EOF'
import json,sys
p,mock=sys.argv[1],sys.argv[2]
c=json.load(open(p))
c["ssh_bin"]=mock
c["remote_hosts"]={"rhost":{"sandbox":"danger-full-access","tmp_dir":"/tmp","shell":"posix"}}
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
EOF
chmod +x test/mock-ssh.sh
: > "$MOCK_DIR/ssh-calls.log"
"$BIN" add -dir "D:/remote/work" -host rhost -title r-remote -priority 8 "remote work step" >/dev/null
"$BIN" run -quiet
assert "远端任务完成且 runner=remote:rhost" "one(title='r-remote')['status']=='done' and one(title='r-remote')['runner']=='remote:rhost'"
grep -q "remote work step" "$MOCK_DIR/ssh-calls.log" && echo "  ✔ prompt 经 ssh stdin 灌入远端" && pass=$((pass+1)) || { echo "  ✖ prompt 未经 stdin 灌入"; fail=$((fail+1)); }
assert "结果取 marker 之后内容（Windows codex 非零退出不算失败）" "'remote step done' in (one(title='r-remote').get('last_summary') or '')"

echo "== 场景25: 远端 claude/fable（带 model 走远端 claude；输出 JSON 直接 parse）=="
echo "claude-ok" > "$MOCK_DIR/ssh-behavior"
: > "$MOCK_DIR/ssh-calls.log"
"$BIN" add -dir "D:/Project/MyApp" -host rhost -model claude-fable-5 -fresh -title r-fable -priority 8 "remote fable design step" >/dev/null
"$BIN" run -quiet
assert "远端 fable 任务完成且 runner=remote:rhost" "one(title='r-fable')['status']=='done' and one(title='r-fable')['runner']=='remote:rhost'"
grep -q -- "--model claude-fable-5" "$MOCK_DIR/ssh-calls.log" && echo "  ✔ 远端 claude 传了 --model claude-fable-5" && pass=$((pass+1)) || { echo "  ✖ 未传 --model"; fail=$((fail+1)); }
assert "远端 claude 结果走 parseClaudeJSON（非 marker 路径）" "one(title='r-fable').get('last_summary')=='remote design done'"

echo "== 场景26: 远端账号限额→按 resume_at 挂起（无损接力，不写全局冷却）=="
echo "claude-limit" > "$MOCK_DIR/ssh-behavior"
rm -f "$CLAUDEGO_ROOT/cooldown.json"
"$BIN" add -dir "D:/Project/MyApp" -host rhost -model claude-fable-5 -fresh -title r-limit -priority 9 "remote step hits limit" >/dev/null
"$BIN" run -quiet
assert "远端限额任务挂起 limit_paused 且 resume_at 在未来" "one(title='r-limit')['status']=='limit_paused' and one(title='r-limit')['resume_at_epoch']>now"
test ! -f "$CLAUDEGO_ROOT/cooldown.json" && echo "  ✔ 未写全局冷却（远端账号与本机独立）" && pass=$((pass+1)) || { echo "  ✖ 误写了全局冷却"; fail=$((fail+1)); }
echo "codex" > "$MOCK_DIR/ssh-behavior"

echo "== 场景27: cancel 运行中任务 → 击杀进程组、≤1 重扫周期释放同目录槽位 =="
python3 - "$CLAUDEGO_ROOT/config.json" <<'EOF'
import json,sys
p=sys.argv[1]; c=json.load(open(p)); c["drain_rescan_sec"]=1
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
EOF
printf 'hang\nok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
rm -f "$MOCK_DIR/hang-child.pid"
"$BIN" add -dir "$PROJ" -title cxl-victim -priority 9 "long hanging work" >/dev/null
"$BIN" add -dir "$PROJ" -title cxl-heir -priority 2 "heir work" >/dev/null
"$BIN" run -quiet & RUNPID=$!
( sleep 90; kill -9 $RUNPID 2>/dev/null ) & WDPID=$!
vid=""
for _ in $(seq 1 50); do
  vid=$("$BIN" list -json | python3 -c "import json,sys;ts=[t for t in (json.load(sys.stdin) or []) if t['title']=='cxl-victim' and t['status']=='running'];print(ts[0]['id'] if ts else '')")
  [ -n "$vid" ] && break
  sleep 0.2
done
[ -n "$vid" ] && echo "  ✔ 挂起任务已进入 running" && pass=$((pass+1)) || { echo "  ✖ 任务未进入 running"; fail=$((fail+1)); }
cout=$("$BIN" cancel "$vid")
echo "$cout" | grep -q "已标记取消" && echo "  ✔ 运行中任务 cancel 返回标记提示（暂不归档）" && pass=$((pass+1)) || { echo "  ✖ cancel 输出异常: $cout"; fail=$((fail+1)); }
T0=$(python3 -c "import time;print(time.time())")
if wait $RUNPID; then
  EL=$(python3 -c "import time;print(time.time()-$T0)")
  python3 -c "import sys;sys.exit(0 if $EL < 8 else 1)" && echo "  ✔ cancel 后 run 快速排空收工（${EL%.*}s，mock 本应吊 300s）" && pass=$((pass+1)) || { echo "  ✖ cancel 后收工过慢（${EL}s）"; fail=$((fail+1)); }
else
  echo "  ✖ run 未正常退出（被看门狗击杀？）"; fail=$((fail+1))
fi
{ kill $WDPID && wait $WDPID; } 2>/dev/null || true
assert "同目录后继任务在 cancel 后被派发完成" "one(title='cxl-heir')['status']=='done'"
test ! -f "$CLAUDEGO_ROOT/tasks/$vid.json" && test -f "$CLAUDEGO_ROOT/archive/$vid.json" && echo "  ✔ 被取消任务已归档（tasks/ 已清）" && pass=$((pass+1)) || { echo "  ✖ 被取消任务未归档"; fail=$((fail+1)); }
python3 -c "import json,sys;sys.exit(0 if json.load(open('$CLAUDEGO_ROOT/archive/$vid.json'))['status']=='canceled' else 1)" \
  && echo "  ✔ 归档状态为 canceled" && pass=$((pass+1)) || { echo "  ✖ 归档状态不是 canceled"; fail=$((fail+1)); }
cpid=$(cat "$MOCK_DIR/hang-child.pid" 2>/dev/null || echo "")
if [ -n "$cpid" ] && ! kill -0 "$cpid" 2>/dev/null; then
  echo "  ✔ 进程组连坐击杀（孙进程已死）"; pass=$((pass+1))
else
  echo "  ✖ 孙进程仍存活或 pid 缺失（$cpid）"; fail=$((fail+1)); [ -n "$cpid" ] && kill -9 "$cpid" 2>/dev/null || true
fi

echo "== 场景28: cancel 运行中远端卡 → 击杀本地 ssh 释放槽位（远端产物丢弃）=="
echo "claude-hang" > "$MOCK_DIR/ssh-behavior"
rm -f "$MOCK_DIR/ssh-hang-child.pid"
"$BIN" add -dir "D:/remote/hangwork" -host rhost -model claude-fable-5 -fresh -title r-victim -priority 8 "remote hanging step" >/dev/null
"$BIN" run -quiet & RUNPID=$!
( sleep 90; kill -9 $RUNPID 2>/dev/null ) & WDPID=$!
rid=""
for _ in $(seq 1 50); do
  rid=$("$BIN" list -json | python3 -c "import json,sys;ts=[t for t in (json.load(sys.stdin) or []) if t['title']=='r-victim' and t['status']=='running'];print(ts[0]['id'] if ts else '')")
  [ -n "$rid" ] && break
  sleep 0.2
done
[ -n "$rid" ] && echo "  ✔ 远端卡已进入 running" && pass=$((pass+1)) || { echo "  ✖ 远端卡未进入 running"; fail=$((fail+1)); }
"$BIN" cancel "$rid" >/dev/null
T0=$(python3 -c "import time;print(time.time())")
if wait $RUNPID; then
  EL=$(python3 -c "import time;print(time.time()-$T0)")
  python3 -c "import sys;sys.exit(0 if $EL < 8 else 1)" && echo "  ✔ cancel 后 ssh 被击杀、run 快速收工（${EL%.*}s）" && pass=$((pass+1)) || { echo "  ✖ cancel 后收工过慢（${EL}s）"; fail=$((fail+1)); }
else
  echo "  ✖ run 未正常退出（被看门狗击杀？）"; fail=$((fail+1))
fi
{ kill $WDPID && wait $WDPID; } 2>/dev/null || true
test ! -f "$CLAUDEGO_ROOT/tasks/$rid.json" && test -f "$CLAUDEGO_ROOT/archive/$rid.json" && echo "  ✔ 远端卡已归档" && pass=$((pass+1)) || { echo "  ✖ 远端卡未归档"; fail=$((fail+1)); }
spid=$(cat "$MOCK_DIR/ssh-hang-child.pid" 2>/dev/null || echo "")
if [ -n "$spid" ] && ! kill -0 "$spid" 2>/dev/null; then
  echo "  ✔ ssh 进程组已整组击杀（本地槽位与目录锁释放）"; pass=$((pass+1))
else
  echo "  ✖ ssh 孙进程仍存活或 pid 缺失（$spid）"; fail=$((fail+1)); [ -n "$spid" ] && kill -9 "$spid" 2>/dev/null || true
fi
echo "codex" > "$MOCK_DIR/ssh-behavior"

echo "== 场景29: 修复闭环——审核 concerns 自动派修复卡，修复后再审 pass 收口 =="
# 时序: 实现卡(ok) → 自动审核(review_concerns) → 自动修复R1(ok) → 自动再审(review_pass) → 停
printf 'ok\nreview_concerns\nok\nreview_pass\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" add -dir "$PROJ" -title fixloop-impl -priority 6 -review-after -model opus "implement leaf X" >/dev/null
"$BIN" run -quiet
assert "实现卡完成" "one(title='fixloop-impl')['status']=='done'"
assert "审核卡带谱系(review_of=实现卡)" "one(title='审核: fixloop-impl')['review_of']==one(title='fixloop-impl')['id']"
assert "concerns 自动派修复R1卡且完成" "one(title='修复R1: fixloop-impl [concerns:1P0+1P1]')['status']=='done'"
assert "修复卡继承模型并抬 effort=high" "one(title='修复R1: fixloop-impl [concerns:1P0+1P1]')['model']=='opus' and one(title='修复R1: fixloop-impl [concerns:1P0+1P1]')['effort']=='high'"
assert "修复卡自动挂再审且 pass 后停（无修复R2）" "one(title='审核: 修复R1: fixloop-impl [concerns:1P0+1P1]')['status']=='done' and len([t for t in tasks if t['title'].startswith('修复R2')])==0"
grep -q "按类闭合" "$MOCK_DIR/calls.log" && echo "  ✔ 修复 prompt 含按类闭合纪律" && pass=$((pass+1)) || { echo "  ✖ 修复 prompt 缺按类闭合"; fail=$((fail+1)); }
grep -q -- "--effort high" "$MOCK_DIR/calls.log" && echo "  ✔ 修复调用传了 --effort high" && pass=$((pass+1)) || { echo "  ✖ 未传 --effort"; fail=$((fail+1)); }

echo "== 场景30: 修复闭环——超轮限挂 held 升级卡不再自动修 =="
# 手工造一张已到第 3 轮的修复卡(带 review-after),模拟循环打转到轮限
python3 - "$CLAUDEGO_ROOT" "$PROJ" <<'PYEOF'
import json,sys,os
root,proj=sys.argv[1],sys.argv[2]
tid="t9999-0000-fx03"
task={"id":tid,"title":"修复R3: fixloop-impl2 [concerns:0P0+1P1]","type":"sequence","priority":6,
"status":"queued","dir":proj,"prompts":["fix again"],"step":0,
"review_after":True,"fix_round":3,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}
open(os.path.join(root,"tasks",tid+".json"),"w").write(json.dumps(task,ensure_ascii=False))
PYEOF
printf 'ok\nreview_concerns\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" run -quiet
assert "R3 修复卡完成" "one(id='t9999-0000-fx03')['status']=='done'"
assert "超轮限→held 升级卡而非修复R4" "len([t for t in tasks if '超轮限' in t['title'] and t['status']=='held'])==1 and len([t for t in tasks if t['title'].startswith('修复R4')])==0"

echo "== 场景31: 交叉验证链——引擎甲(claude)独立作答→引擎乙(codex)独立作答(不见甲)→引擎乙交叉查漏 =="
# codex_bin/codex_model 已在前序 codex 场景配好；cross_profiles opus-codex 由 defaultConfig 合并提供。
printf 'ok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
: > "$MOCK_DIR/codex-calls.log"  # 清空以精确断言交叉引擎乙卡的 reasoning 档
"$BIN" cross -dir "$PROJ" -title xc "XCHECK_TASK_MARKER 某配置键缺省语义待裁决" >/dev/null
"$BIN" run -quiet
assert "交叉A(引擎甲=claude opus max)完成" "one(x_role='A')['status']=='done' and one(x_role='A')['model']=='claude-opus-4-8' and one(x_role='A')['effort']=='max'"
assert "交叉B 自动派出并由 codex(引擎乙)完成" "one(x_role='B')['status']=='done' and one(x_role='B')['runner']=='codex'"
assert "交叉B 独立性(不持甲结论字段)+与甲同题" "one(x_role='B').get('x_peer') is None and one(x_role='B')['prompts'][0]==one(x_role='A')['prompts'][0] and 'step ok' not in one(x_role='B')['prompts'][0]"
assert "交叉A 结论不落 LastSummary(不暴露 list 摘要)" "one(x_role='A')['status']=='done' and (one(x_role='A').get('last_summary') in (None,''))"
# B1 被动暴露最小化:甲结论从 A 的日志抹除(codex read-only 读全盘,logs/<A>.log 是最直白的被动泄漏面)
AID=$("$BIN" list -json | python3 -c "import json,sys;print([t['id'] for t in json.load(sys.stdin) if t.get('x_role')=='A'][0])")
grep -q "已隔离" "$CLAUDEGO_ROOT/logs/$AID.log" && ! grep -q "step ok" "$CLAUDEGO_ROOT/logs/$AID.log" && echo "  ✔ 交叉A 结论已从日志抹除(被动暴露最小化)" && pass=$((pass+1)) || { echo "  ✖ 交叉A 结论仍暴露在 logs/<A>.log"; fail=$((fail+1)); }
assert "交叉C 自动派出并由 codex(引擎乙)完成" "one(x_role='C')['status']=='done' and one(x_role='C')['runner']=='codex'"
assert "交叉C 合并注入 甲结论+乙结论+任务" "'step ok' in one(x_role='C')['prompts'][0] and 'codex done' in one(x_role='C')['prompts'][0] and 'XCHECK_TASK_MARKER' in one(x_role='C')['prompts'][0]"
assert "交叉C 落进度报告(键=不透明链ID,非A卡ID)" "one(x_role='C')['progress_key']==one(x_role='A')['x_key'] and one(x_role='A')['x_key']!=one(x_role='A')['id'] and one(x_role='C')['emit_progress']==True"
# 引擎乙 B+C 两次 codex 调用都应带 model_reasoning_effort=max（任务级 Effort 覆盖全局 codex_reasoning=high）
[ "$(grep -c 'model_reasoning_effort=max' "$MOCK_DIR/codex-calls.log")" -ge 2 ] && echo "  ✔ 交叉引擎乙卡(B+C)跑在 max 思考档(覆盖全局 high)" && pass=$((pass+1)) || { echo "  ✖ 交叉引擎乙未用 max 思考档"; fail=$((fail+1)); }
# B7：prompt 经 stdin 送达 codex(非 argv)。任务标记出现在 stdin dump 里,且**不**在任何 args 行——证明走 stdin。
if grep -q "XCHECK_TASK_MARKER" "$MOCK_DIR/codex-calls.log" && ! grep 'codex call.*args:' "$MOCK_DIR/codex-calls.log" | grep -q "XCHECK_TASK_MARKER"; then
  echo "  ✔ B7:合并 prompt 经 stdin(非 argv)送达 codex"; pass=$((pass+1))
else echo "  ✖ B7:prompt 未经 stdin 送达 codex"; fail=$((fail+1)); fi

echo "== 场景32: 交叉A(claude引擎)在 claude 冷却 + codex_fallback 下绝不降级 codex(独立性护栏) =="
# 若 A 被降级到 codex,则甲乙同引擎、交叉验证形同虚设——A 必须排队等 claude 窗口。
python3 - "$CLAUDEGO_ROOT/config.json" <<'PYEOF'
import json,sys,time
p=sys.argv[1]; c=json.load(open(p))
c["codex_fallback"]=True; c["codex_model"]="codex-test-model"
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
json.dump({"until_epoch":int(time.time())+3600,"reason":"mock","set_at":"t"}, open(p.replace("config.json","cooldown.json"),"w"))
PYEOF
: > "$MOCK_DIR/codex-calls.log"
"$BIN" cross -dir "$PROJ" -title xcool "cooldown 下的裁决" >/dev/null
"$BIN" run -quiet >/dev/null 2>&1
assert "冷却期交叉A 未降级 codex(仍排队等 claude)" "one(x_role='A',title='交叉A[opus-codex]: xcool')['status']=='queued' and (one(x_role='A',title='交叉A[opus-codex]: xcool').get('runner') in (None,''))"
grep -q "codex call" "$MOCK_DIR/codex-calls.log" && { echo "  ✖ 交叉A 被错误降级到 codex(独立性破产)"; fail=$((fail+1)); } || { echo "  ✔ 交叉A 未触发任何 codex 调用"; pass=$((pass+1)); }

echo "== 场景33: codex 钉定卡在 codex 不可用时绝不 fail-open 到 claude(引擎身份冻结) =="
# codex_bin 就位时入队钉定卡→抽走 codex_bin→清冷却让 claude 可用→跑一轮：卡须排队,绝不落 claude。
rm -f "$CLAUDEGO_ROOT/cooldown.json"
"$BIN" add -dir "$PROJ" -runner codex -title pin-freeze -priority 9 "codex-only frozen work" >/dev/null
python3 - "$CLAUDEGO_ROOT/config.json" <<'PYEOF'
import json,sys
p=sys.argv[1]; c=json.load(open(p)); c["codex_bin"]=""  # 抽走 codex
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
PYEOF
: > "$MOCK_DIR/calls.log"
"$BIN" run -quiet >/dev/null 2>&1
assert "codex 不可用时钉定卡仍排队(未 fail-open 到 claude)" "one(title='pin-freeze')['status']=='queued'"
grep -q "codex-only frozen work" "$MOCK_DIR/calls.log" && { echo "  ✖ 钉定卡被 fail-open 到 claude 执行"; fail=$((fail+1)); } || { echo "  ✔ 钉定卡未落到 claude"; pass=$((pass+1)); }

echo "== 场景34: codex 瞬时网络失败——取真错误(跨过横幅)+判 transient 退避重试,不烧成硬失败 =="
# 单腿审核可靠性的根:codex 失败别一律显示 "Reading additional input from stdin"(横幅),别把瞬时网络当硬失败。
python3 - "$CLAUDEGO_ROOT/config.json" "$PWD/test/mock-codex.sh" <<'PYEOF'
import json,sys
p,mock=sys.argv[1],sys.argv[2]; c=json.load(open(p))
c["codex_bin"]=mock; c["codex_model"]="codex-test-model"
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
PYEOF
rm -f "$CLAUDEGO_ROOT/cooldown.json"
echo 0 > "$MOCK_DIR/codex-n"; touch "$MOCK_DIR/codex-fail-all"   # 本轮所有 codex 调用一律网络失败(确定性,不受 drain 顺序影响)
"$BIN" add -dir "$PROJ" -runner codex -title codex-netfail -priority 9 "codex single-leg work" >/dev/null
"$BIN" run -quiet >/dev/null 2>&1
assert "codex 瞬时失败→取真错误(非横幅)且判 transient 退避重试" "one(title='codex-netfail')['status']=='queued' and one(title='codex-netfail')['attempts']==1 and 'stream' in one(title='codex-netfail')['last_error'].lower() and 'reading additional input' not in one(title='codex-netfail')['last_error'].lower()"
rm -f "$MOCK_DIR/codex-fail-all"  # 解除失败标记

echo
echo "结果: $pass 通过, $fail 失败"
[ "$fail" -eq 0 ]
