#!/bin/bash
# 模拟 codex CLI：从 stdin 读 prompt（真 codex exec 行为），把"最终消息"写到 -o 文件。
# 总先打 codex 横幅（"Reading additional input from stdin..." + 配置块）到 stdout——真 codex 如此，
# 用于验证 claudego 的 codexErrorLine 会跨过横幅取真错误。行为由 $MOCK_DIR/codex-plan 控制（每行一次调用）。
set -u
out=""; prev=""
for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; prev="$a"; done
stdin_prompt=$(cat)   # 消费 stdin：codex exec 无 prompt 参数时从 stdin 读；不读会触发子进程早退 EPIPE 竞态
cn=$(cat "$MOCK_DIR/codex-n" 2>/dev/null || echo 0); cn=$((cn + 1)); echo "$cn" > "$MOCK_DIR/codex-n"
beh=$(sed -n "${cn}p" "$MOCK_DIR/codex-plan" 2>/dev/null); [ -z "$beh" ] && beh=ok
[ -f "$MOCK_DIR/codex-fail-all" ] && beh=netfail   # 存在该标记则本 mock 所有 codex 调用一律网络失败(确定性)
{
  echo "===== codex call $cn args: $*"
  echo "----- codex stdin prompt: $stdin_prompt"
} >> "$MOCK_DIR/codex-calls.log"

# 横幅永远先打（真 codex 第一行就是它）——claudego 提取错误时必须跨过它。
printf 'Reading additional input from stdin...\nOpenAI Codex v0.144.1\n--------\nmodel: mock\nsandbox: read-only\n--------\n'

case "$beh" in
  netfail)
    # 瞬时网络错误：真错误在末尾，-o 不写（空结果）→ claudego 应取到真错误、判 transient 退避重试
    echo "stream error: stream disconnected before completion" >&2
    exit 1 ;;
  *)
    [ -n "$out" ] && printf 'codex done\n```json\n{"verdict":"合并结论","confidence":"high","summary":"mock 合并结论"}\n```\n' > "$out"
    echo "codex running" ;;
esac
