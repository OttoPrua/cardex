#!/bin/bash
# 模拟 ssh：末参为远端命令，stdin=prompt。按 $MOCK_DIR/ssh-behavior 决定输出（默认 codex）。
#   codex（默认）: 输出噪声 + marker + 结果，exit 3（模拟 Windows codex 非零退出）
#   claude-ok    : 输出 claude -p 的成功 JSON（走 parseClaudeJSON）
#   claude-limit : 输出 claude 限额 JSON（触发远端账号限额→limit_paused）
#   claude-hang  : 不输出直接吊死（模拟远端进程吊住 ssh 管道，供 cancel 击杀断言）
set -u
PROMPT=$(cat)
mkdir -p "$MOCK_DIR"
{
  echo "===== ssh call args: $*"
  echo "PROMPT: $PROMPT"
} >> "$MOCK_DIR/ssh-calls.log"

beh=$(cat "$MOCK_DIR/ssh-behavior" 2>/dev/null || echo "codex")
case "$beh" in
  claude-ok)
    echo '{"type":"result","subtype":"success","is_error":false,"result":"remote design done","session_id":"rs-1","num_turns":2,"total_cost_usd":0.02,"usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}'
    ;;
  claude-hang)
    sleep 300 &
    echo $! > "$MOCK_DIR/ssh-hang-child.pid"
    sleep 300
    ;;
  claude-limit)
    echo '{"type":"result","subtype":"error","is_error":true,"result":"usage limit reached · resets 1am"}'
    ;;
  *)
    echo "OpenAI Codex 执行日志噪声（marker 之前，应被解析器忽略）"
    echo "===CARDEX_REMOTE_RESULT==="
    echo "remote step done"
    exit 3
    ;;
esac
