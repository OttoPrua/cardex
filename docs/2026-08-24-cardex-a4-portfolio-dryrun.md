# 2026-08-24 · A4 离线组合编排 dry-run 收据（portfolio orchestration receipt）

一份**只读演练**收据：用 W2 候选构建的二进制，在一个一次性 `/tmp` 数据根上证明
「三槽队列、writer/reviewer 分离、held 不释放集成、模块状态本地持久化」四条边界。
全程离线；**没有** install、launchd、调度 tick、Board/web、live/cutover 释放；
`~/.cardex` 未被触碰。

## 身份

| 项 | 值 |
|---|---|
| 候选 commit | `f9c0c2aed5418e5a5382f0f9e10e699d40b4ce99`（branch `cursor/cardex-w1w4-first-packet-9d89`） |
| 候选 tree | `6467e6341be7ad214767bd9d1dcb1450e2c24708` |
| 二进制 sha256 | `508a6900910626b02e10fc0fdf60ec0aa5b3d36ebf44dc04e658619aa88b0ae3`（`go build` 双构建同 hash，可复现） |
| 演练脚本 sha256 | `11fa3abfd662cc706864b802bc2612ba72c0f3a5650d773d5f0c1c643d952fd0`（`/tmp/cardex-a4-dryrun/run-dryrun.sh`，本文附录内嵌全文） |
| transcript sha256 | `822c4f693ccd2475b428138100b92285441b5fda2eafb5986bc76dda65f4cd41` |
| 数据根 | `/tmp/cardex-a4-dryrun/root`（一次性；收据落档后可删） |

## 四条边界与对应证据

1. **三槽队列**：`config.max_parallel = 3`，三条模块线（auth / billing / search，
   写域两两不相交）各派一个 writer，`cardex list -json` 读回恰好 3 张 queued 写卡
   占满三槽 + 3 张 held 集成卡。没有任何 tick 运行，队列只入不派。
2. **writer/reviewer 分离是机器拒绝，不是约定**：writer 还 live 时
   `freeze-candidate` 被拒（`writer … still live; bytes are not frozen`）；
   没有冻结候选时 `review` 被拒（`freeze a candidate before review`）；
   同一线重放 `writer` 只得 duplicate（`duplicate active role`），不会铸出第二个写者。
3. **held 不释放集成**：对 held 集成卡执行 `cardex release` 与
   `workflow try-release-integration` 均被拒（`missing_review_task`——门重新推导，
   连 review 卡都没有，更谈不上 verdict/custody），三张集成卡全部保持 `held`；
   live 与 cutover 门全程 `held`。
4. **模块状态本地持久化**：每条线的 `workflows/<id>.json` + `.progress.json|.md`
   都在数据根下（digest 见 transcript 第 59-65 行），**新进程**重读得到同一组记录，
   其中 billing 线因 try-release 被拒而落盘 `integration_held`——状态演化本身也被
   持久化。没有 root-notify 收据（未发生 material 转移），没有 custody 收据
   （从未 ingest 任何 review）。

## transcript（原样）

```text
== 0. candidate identity and binary ==
commit f9c0c2aed5418e5a5382f0f9e10e699d40b4ce99
tree 6467e6341be7ad214767bd9d1dcb1450e2c24708
508a6900910626b02e10fc0fdf60ec0aa5b3d36ebf44dc04e658619aa88b0ae3  /tmp/cardex-a4-dryrun/cardex

== 1. fixture repository (offline, throwaway) ==
root initialized at /tmp/cardex-a4-dryrun/root (local files only)

== 2. three-slot queue: max_parallel=3 ==
config.max_parallel = 3

== 3. three module lanes (disjoint write domains) ==
已创建 workflow wf0824-0926-b624c2 mode=serial module=auth integration=t0824-0926-a490（held）
已创建 workflow wf0824-0926-1c7b05 mode=serial module=billing integration=t0824-0926-5006（held）
已创建 workflow wf0824-0926-753874 mode=serial module=search integration=t0824-0926-7ceb（held）
wf0824-0926-1c7b05	serial	billing	design	round 0/2	integration=held	live=held
wf0824-0926-753874	serial	search	design	round 0/2	integration=held	live=held
wf0824-0926-b624c2	serial	auth	design	round 0/2	integration=held	live=held

== 4. one writer per lane -> three occupied queue slots (no tick ever runs) ==
workflow wf0824-0926-1c7b05 writer=t0824-0926-8b62 round=0 engine=claude
workflow wf0824-0926-753874 writer=t0824-0926-feec round=0 engine=claude
workflow wf0824-0926-b624c2 writer=t0824-0926-5d5b round=0 engine=claude
-- queued/held cards --
queued writer cards: 3
  t0824-0926-5d5b  wf=wf0824-0926-b624c2  paths=['internal/auth']  runner=claude
  t0824-0926-8b62  wf=wf0824-0926-1c7b05  paths=['internal/billing']  runner=claude
  t0824-0926-feec  wf=wf0824-0926-753874  paths=['internal/search']  runner=claude
held integration cards: 3
  t0824-0926-5006  wf=wf0824-0926-1c7b05
  t0824-0926-7ceb  wf=wf0824-0926-753874
  t0824-0926-a490  wf=wf0824-0926-b624c2

== 5. writer/reviewer separation is machine-refused, not convention ==
-- freeze while the writer is live must refuse --
错误: workflow duplicate active role: writer t0824-0926-8b62 still live; bytes are not frozen
-- reviewer before a frozen candidate must refuse --
错误: workflow malformed: freeze a candidate before review
-- second writer on the same lane must be a duplicate --
错误: workflow duplicate active role: writer t0824-0926-8b62

== 6. held does not release integration ==
-- cardex release on the held gated card must refuse --
错误: t0824-0926-5006 集成门仍 held（missing_review_task）；durable review done 不等于 verdict=pass，不足以 release
-- workflow try-release-integration must refuse --
错误: workflow integration remains held: missing_review_task
all 3 integration cards still held

== 7. module state persists locally and survives a fresh process ==
wf0824-0926-1c7b05.json
wf0824-0926-1c7b05.progress.json
wf0824-0926-1c7b05.progress.md
wf0824-0926-753874.json
wf0824-0926-753874.progress.json
wf0824-0926-753874.progress.md
wf0824-0926-b624c2.json
wf0824-0926-b624c2.progress.json
wf0824-0926-b624c2.progress.md
-- record digests --
1cb9c8aeec083f02b3f99130ab0c4915da10c912b28a973bbba73480f5db7306  <root>/workflows/wf0824-0926-1c7b05.json
ee9b4d62c8f5271f336d48c113c369474819313915cdbba20512a06b3d1f936c  <root>/workflows/wf0824-0926-1c7b05.progress.json
930d5c4156a22f5b08f2b849ebad84ab451cc8377becc393634d9da72caff5bb  <root>/workflows/wf0824-0926-753874.json
f28e7598af06e8e968e37f8880de241dfca68772fd81657c895cf16fc0b08727  <root>/workflows/wf0824-0926-753874.progress.json
a7aac50055b5989a4bcd6d52ea327b2168b32a4f7ec4e8d9d1b719d3ffd87095  <root>/workflows/wf0824-0926-b624c2.json
b2c07f5a79c918b00af2b90ed4fa8b4da948e4b93c1eb0fd4f93e88827b79d1d  <root>/workflows/wf0824-0926-b624c2.progress.json
-- fresh process re-reads the same records --
wf0824-0926-1c7b05	serial	billing	integration_held	round 0/2	integration=held	live=held
wf0824-0926-753874	serial	search	writing	round 0/2	integration=held	live=held
wf0824-0926-b624c2	serial	auth	writing	round 0/2	integration=held	live=held
-- no receipts were minted: root-notify absent or empty --
(root-notify not created)
-- no custody receipts: no review was ever ingested --
(control/custody not created)

== 8. boundary attestation ==
never invoked: tick, install, install-launchd, board/web, live/cutover release
state root: /tmp/cardex-a4-dryrun/root (throwaway; ~/.cardex untouched)
DRY RUN COMPLETE
```

## 附录：演练脚本全文

```bash
#!/bin/bash
# Offline portfolio-orchestration dry run for Cardex lane A4.
# Boundary: no install, no launchd, no scheduler tick, no Board, no live.
# All state lives under a throwaway /tmp root; nothing touches ~/.cardex.
set -euo pipefail
export PATH=/usr/local/go/bin:$PATH

REPO=/agent/repos/Cardex
BASE=/tmp/cardex-a4-dryrun
ROOT="$BASE/root"
PROJ="$BASE/proj"
BIN="$BASE/cardex"
rm -rf "$ROOT" "$PROJ"
mkdir -p "$PROJ"

echo "== 0. candidate identity and binary =="
git -C "$REPO" log -1 --format='commit %H%ntree %T'
(cd "$REPO" && go build -o "$BIN" .)
sha256sum "$BIN"

echo
echo "== 1. fixture repository (offline, throwaway) =="
mkdir -p "$PROJ/internal/auth" "$PROJ/internal/billing" "$PROJ/internal/search"
echo 'package auth' > "$PROJ/internal/auth/a.go"
echo 'package billing' > "$PROJ/internal/billing/b.go"
echo 'package search' > "$PROJ/internal/search/s.go"
git -C "$PROJ" init -q
git -C "$PROJ" -c user.name=dryrun -c user.email=dryrun@example.invalid add -A
git -C "$PROJ" -c user.name=dryrun -c user.email=dryrun@example.invalid commit -q -m fixture

export CARDEX_ROOT="$ROOT"
"$BIN" init >/dev/null
echo "root initialized at $ROOT (local files only)"

echo
echo "== 2. three-slot queue: max_parallel=3 =="
python3 - "$ROOT/config.json" <<'EOF'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["max_parallel"] = 3
json.dump(cfg, open(p, "w"), ensure_ascii=False, indent=2)
print("config.max_parallel =", json.load(open(p))["max_parallel"])
EOF

echo
echo "== 3. three module lanes (disjoint write domains) =="
for m in auth billing search; do
  "$BIN" workflow list >/dev/null 2>&1 || true
  "$BIN" workflow init -mode serial -module "$m" -goal-id "$m-v1" \
    -goal "dry-run vertical for $m" -dir "$PROJ" \
    -terminal-criteria "independent review pass with empty p0/p1; integration and live stay held" \
    -write-domain-id "$m-core" -write-domain-lineage "$m-core-lineage" \
    -write-domain-component "$m" -write-paths "internal/$m" \
    -engine claude -max-rounds 2
done
"$BIN" workflow list

echo
echo "== 4. one writer per lane -> three occupied queue slots (no tick ever runs) =="
WF_IDS=$("$BIN" workflow list | awk '{print $1}')
for id in $WF_IDS; do
  "$BIN" workflow writer "$id"
done
echo "-- queued/held cards --"
"$BIN" list -json | python3 -c '
import json, sys
tasks = json.load(sys.stdin) or []
writers = [t for t in tasks if t["status"] == "queued" and not t.get("integration_gate")]
integs  = [t for t in tasks if t["status"] == "held" and t.get("integration_gate")]
print(f"queued writer cards: {len(writers)}")
for t in writers:
    wd = t.get("write_domain") or {}
    print(f"  {t["id"]}  wf={t.get("workflow_id")}  paths={wd.get("paths")}  runner={t.get("runner_pref")}")
print(f"held integration cards: {len(integs)}")
for t in integs:
    print(f"  {t["id"]}  wf={t.get("workflow_id")}")
assert len(writers) == 3 and len(integs) == 3
'

echo
echo "== 5. writer/reviewer separation is machine-refused, not convention =="
FIRST=$(echo "$WF_IDS" | head -1)
echo "-- freeze while the writer is live must refuse --"
if "$BIN" workflow freeze-candidate -commit HEAD -tree "HEAD^{tree}" "$FIRST" 2>&1; then
  echo "UNEXPECTED: freeze passed"; exit 1
fi
echo "-- reviewer before a frozen candidate must refuse --"
if "$BIN" workflow review "$FIRST" 2>&1; then
  echo "UNEXPECTED: reviewer admitted"; exit 1
fi
echo "-- second writer on the same lane must be a duplicate --"
if "$BIN" workflow writer "$FIRST" 2>&1; then
  echo "UNEXPECTED: second writer admitted"; exit 1
fi

echo
echo "== 6. held does not release integration =="
INTEG=$("$BIN" list -json | python3 -c '
import json, sys
tasks = json.load(sys.stdin) or []
print(next(t["id"] for t in tasks if t.get("integration_gate")))
')
echo "-- cardex release on the held gated card must refuse --"
if "$BIN" release "$INTEG" 2>&1; then
  echo "UNEXPECTED: release passed"; exit 1
fi
echo "-- workflow try-release-integration must refuse --"
if "$BIN" workflow try-release-integration "$FIRST" 2>&1; then
  echo "UNEXPECTED: try-release passed"; exit 1
fi
"$BIN" list -json | python3 -c '
import json, sys
tasks = json.load(sys.stdin) or []
integs = [t for t in tasks if t.get("integration_gate")]
assert all(t["status"] == "held" for t in integs), integs
print(f"all {len(integs)} integration cards still held")
'

echo
echo "== 7. module state persists locally and survives a fresh process =="
ls -1 "$ROOT/workflows/" | sort
echo "-- record digests --"
sha256sum "$ROOT"/workflows/wf*.json | sed "s#$ROOT#<root>#"
echo "-- fresh process re-reads the same records --"
"$BIN" workflow list
echo "-- no receipts were minted: root-notify absent or empty --"
ls -1 "$ROOT/workflows/root-notify" 2>/dev/null || echo "(root-notify not created)"
echo "-- no custody receipts: no review was ever ingested --"
ls -1 "$ROOT/control/custody" 2>/dev/null || echo "(control/custody not created)"

echo
echo "== 8. boundary attestation =="
echo "never invoked: tick, install, install-launchd, board/web, live/cutover release"
echo "state root: $ROOT (throwaway; ~/.cardex untouched)"
echo "DRY RUN COMPLETE"
```

## 边界重申

- 本收据只证明离线编排语义；它**不是**审核结论、集成决定、live 授权或安装授权。
- W2 候选本身仍待独立 reviewer（另一 agent 上下文）复核后，才谈集成；集成后 live/
  cutover 仍是各自独立的 held 门。
- 演练用的 Python 内嵌 f-string 依赖 Python ≥ 3.12（PEP 701 同引号嵌套）。
