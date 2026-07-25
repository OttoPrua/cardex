// app.js — ClaudeGo 看板前端。零依赖、零构建步骤，直接被 //go:embed 打进二进制。
//
// 三条纪律（后端 boardeta.go / boardburn.go 立的规矩，前端必须接住）：
//
//  1. **可空字段一律显式分支**。契约里这些字段随时可能是 null：
//     eta.finish_at / eta.p50_minutes / eta.p80_minutes、
//     burn source 的 resets_at / minutes_to_reset / burn_rate_pct_per_hour / exhaust_at、
//     以及 quota 的四个槽位本身。null 要渲染成「数据不足」并把 basis 摊开给用户看，
//     绝不回退成 0 或「—」——0 会被读成「马上就完」，那是编造。
//     特别注意 resets_at 为 null 与 verdict/stale **无关**：一条完全新鲜、
//     有速率、结论「充裕」的源同样可能没有重置时刻，不能拿 verdict 当护栏。
//
//  2. **数据不足就不画外推**。verdict === "数据不足" 或 exhaust_at 为 null 时
//     不画预测线、不显示耗尽时刻；stale 的源必须带显式过期标记。
//
//  3. **口径要说清**。token 曲线是四类 token 等权相加（缓存读取占九成以上），
//     与 queue_spend 的额度口径差一个数量级，所以后端给的 token_series.basis
//     原样呈现，不许把两个数字并排放着让人自行误会。

'use strict';

/* ============================ 小工具 ============================ */

/** 建 DOM。children 里的字符串一律走 textContent，不存在 innerHTML 注入面。 */
function h(tag, props, ...children) {
  const e = document.createElement(tag);
  if (props) {
    for (const [k, v] of Object.entries(props)) {
      if (v === null || v === undefined || v === false) continue;
      if (k === 'class') e.className = v;
      else if (k === 'text') e.textContent = v;
      else if (k === 'html') e.innerHTML = v;          // 只用于本文件内写死的 SVG 图标
      else if (k === 'style') e.setAttribute('style', v);
      else if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
      else e.setAttribute(k, v === true ? '' : String(v));
    }
  }
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined || c === false) continue;
    e.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return e;
}

const SVG_NS = 'http://www.w3.org/2000/svg';
function sv(tag, props, ...children) {
  const e = document.createElementNS(SVG_NS, tag);
  if (props) {
    for (const [k, v] of Object.entries(props)) {
      if (v === null || v === undefined || v === false) continue;
      e.setAttribute(k, String(v));
    }
  }
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined || c === false) continue;
    e.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return e;
}

const NUM = new Intl.NumberFormat('zh-CN');
const num = (n) => NUM.format(Math.round(n));

/** 紧凑数字：4.5 亿这种量级不能整串印出来。 */
function compact(n) {
  const a = Math.abs(n);
  if (a >= 1e8) return (n / 1e8).toFixed(2) + ' 亿';
  if (a >= 1e4) return (n / 1e4).toFixed(1) + ' 万';
  return num(n);
}

const isNum = (v) => typeof v === 'number' && Number.isFinite(v);

function parseTime(v) {
  if (!v) return null;
  const d = new Date(v);
  return Number.isNaN(d.getTime()) ? null : d;
}

function fmtTime(v) {
  const d = parseTime(v);
  if (!d) return null;
  const p = (x) => String(x).padStart(2, '0');
  const hm = `${p(d.getHours())}:${p(d.getMinutes())}`;
  if (d.toDateString() === new Date().toDateString()) return hm;
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${hm}`;
}

/** 分钟 → 人话时长。null 进 null 出，绝不折成 0。 */
function fmtDur(mins) {
  if (!isNum(mins)) return null;
  if (mins < 1) return '不到 1 分钟';
  if (mins < 60) return `${Math.round(mins)} 分钟`;
  const hrs = mins / 60;
  if (hrs < 48) {
    const hh = Math.floor(hrs);
    const mm = Math.round(mins - hh * 60);
    return mm ? `${hh} 小时 ${mm} 分钟` : `${hh} 小时`;
  }
  return `${(hrs / 24).toFixed(1)} 天`;
}

function relTime(v) {
  const d = parseTime(v);
  if (!d) return null;
  const secs = (Date.now() - d.getTime()) / 1000;
  if (secs < 60) return '刚刚';
  if (secs < 3600) return `${Math.floor(secs / 60)} 分钟前`;
  if (secs < 86400) return `${Math.floor(secs / 3600)} 小时前`;
  return `${Math.floor(secs / 86400)} 天前`;
}

const STATUS_ZH = {
  running: '进行中', queued: '排队中', limit_paused: '限额暂停',
  held: '已挂起', failed: '失败', done: '已完成', canceled: '已取消',
};
// 字形是状态的第二重编码：色盲用户与灰度打印都不能只靠颜色。
const STATUS_GLYPH = {
  running: '▶', queued: '•', limit_paused: '⏸',
  held: '✋', failed: '✕', done: '✓', canceled: '⊘',
};
const STATUS_ORDER = ['running', 'queued', 'limit_paused', 'held', 'failed', 'done', 'canceled'];

const PHASE_ZH = {
  active: '进行中', blocked: '受阻', queued: '排队中',
  done: '已完成', canceled: '已取消',
};

const SERIES_COLORS = ['--s1', '--s2', '--s3', '--s4', '--s5', '--s6', '--s7', '--s8'];

/* ============================ 通用片段 ============================ */

function statusDot(status, small) {
  return h('span', {
    class: `dot${small ? ' dot-sm' : ''} st-${status}`,
    role: 'img',
    'aria-label': STATUS_ZH[status] || status,
  });
}

function statusBadge(status) {
  return h('span', { class: `badge badge-${status}` },
    h('span', { class: 'glyph', 'aria-hidden': 'true', text: STATUS_GLYPH[status] || '' }),
    STATUS_ZH[status] || status);
}

function tierBadge(model, tier) {
  return h('span', {
    class: `tier tier-${tier || '未知'}`,
    title: `模型 ${model || '(账号默认)'}／${tier || '未知'} 档`,
  },
    h('span', { class: 'tier-bar', 'aria-hidden': 'true' }),
    model || '(账号默认)');
}

function metaChip(text, opts) {
  const o = opts || {};
  return h('span', { class: `meta-chip${o.mono ? ' is-mono' : ''}`, title: o.title || text, text });
}

/** 分段进度条：展示状态构成，而不是只给一个百分比。 */
function progressBar(stats, pct) {
  const total = stats.total || 0;
  const segs = [];
  if (total > 0) {
    for (const k of STATUS_ORDER) {
      const n = stats[k] || 0;
      if (!n) continue;
      segs.push(h('span', {
        class: 'prog-seg',
        style: `width:${(n / total) * 100}%;background:var(--k-${k})`,
        title: `${STATUS_ZH[k]} ${n}`,
      }));
    }
  }
  const label = STATUS_ORDER.filter((k) => stats[k]).map((k) => `${STATUS_ZH[k]} ${stats[k]}`).join('，');
  return h('div', { class: 'prog-wrap' },
    h('div', { class: 'prog', role: 'img', 'aria-label': `共 ${total} 张卡：${label}` }, segs),
    h('span', {
      class: 'prog-pct',
      title: '完成占比，分母已排除已取消卡',
      text: `${isNum(pct) ? pct : 0}%`,
    }));
}

function statusLegend(stats) {
  return h('div', { class: 'legend' }, STATUS_ORDER.filter((k) => stats[k] > 0).map((k) =>
    h('span', { class: 'legend-item' },
      h('span', { class: 'legend-sw', style: `background:var(--k-${k})` }),
      STATUS_ZH[k],
      h('span', { class: 'n', text: String(stats[k]) }))));
}

/**
 * ETA 一行。这是全站最容易骗人的地方，规则写死在这里：
 *   p50 为 null → 「数据不足」，一个数字都不给；
 *   finish_at 为 null 但 p50 有值 → 只说还需多久，不编一个完成时刻。
 * basis 默认可见——估算依据本身就是结论的一部分。
 */
function etaLine(eta, opts) {
  if (!eta) return null;
  const o = opts || {};
  const wrap = h('div');
  const line = h('div', { class: 'eta' });
  const p50 = isNum(eta.p50_minutes) ? eta.p50_minutes : null;
  const p80 = isNum(eta.p80_minutes) ? eta.p80_minutes : null;

  if (p50 === null) {
    line.append(h('span', { class: 'eta-na', text: '预计完成：数据不足' }));
  } else if (p50 === 0 && p80 === 0) {
    line.append(h('span', { class: 'eta-val', text: '已无待推进任务' }));
  } else {
    const at = fmtTime(eta.finish_at);
    line.append(h('span', { class: 'eta-val', text: at ? `预计 ${at} 完成` : `预计还需 ${fmtDur(p50)}` }));
    const detail = [];
    if (at) detail.push(`约 ${fmtDur(p50)}`);
    if (p80 !== null) detail.push(`p80 ${fmtDur(p80)}`);
    if (detail.length) line.append(h('span', { class: 'eta-conf', text: detail.join('・') }));
  }
  if (eta.confidence) {
    line.append(h('span', { class: 'eta-conf', title: '估算置信度', text: `置信度 ${eta.confidence}` }));
  }
  wrap.append(line);
  if (eta.basis && !o.hideBasis) wrap.append(h('p', { class: 'eta-basis', text: eta.basis }));
  return wrap;
}

/** 任务行右侧的极短 ETA。同样：null 就说数据不足。 */
function etaShort(eta) {
  if (!eta || !isNum(eta.p50_minutes)) {
    return h('span', { class: 'tr-eta is-na', title: eta ? eta.basis : '', text: '数据不足' });
  }
  const at = fmtTime(eta.finish_at);
  return h('span', {
    class: 'tr-eta', title: eta.basis || '',
    text: at ? `~${at}` : `~${fmtDur(eta.p50_minutes)}`,
  });
}

function descBlock(text, source) {
  if (!text) return null;
  return h('p', { class: 'desc' }, text,
    h('span', {
      class: 'desc-src',
      title: source === 'override' ? '来自 board.json 的人工文案' : '由任务卡统计自动推导，非人工撰写',
      text: source === 'override' ? '人工' : '自动',
    }));
}

/**
 * 落地进度块（CG-8）：与"卡片进度"**并列**，不替换。
 * 契约与后端约定：
 *   - goal == null 或未定义 → 完全不渲染（后端已 omitempty，前端再兜一层）；
 *   - goal.insufficient === true → 只显示"数据不足"+原因，禁止画百分数；
 *   - goal.landed_percent 数值有效但 goal.partial===true → 数值旁标 "partial"；
 *   - 每个 milestone 同理：insufficient→"数据不足"，stale→额外标 stale 徽标。
 * 这些"不显示"分支不是可选优化，是 fail-honest 承重防线——写死在这里。
 */
function goalBlock(goal) {
  if (!goal) return null;
  const head = h('div', { class: 'goal-head' },
    h('span', { class: 'goal-label', title: '离项目目标多远（人工/机械里程碑合成），与上方的完成占比并列而非替换', text: '落地进度' }));
  const pctNum = isNum(goal.landed_percent) ? goal.landed_percent : null;
  if (goal.insufficient || pctNum === null) {
    head.append(h('span', {
      class: 'goal-na', title: goal.insufficient_reason || '合成所需数据不足', text: '数据不足',
    }));
  } else {
    head.append(h('span', { class: 'goal-pct', text: `${pctNum}%` }));
    if (goal.partial) {
      head.append(h('span', {
        class: 'goal-partial',
        title: '部分里程碑数据不足，此值仅基于可用里程碑合成',
        text: 'partial',
      }));
    }
  }
  if (goal.goal_source) {
    head.append(h('span', {
      class: 'goal-src', title: '数据来源：manual=人工评估@as_of；evidence=机械化取数@文件mtime；mixed=混合',
      text: goal.goal_source,
    }));
  }
  const wrap = h('div', { class: 'goal-wrap' }, head);
  if (goal.statement) {
    wrap.append(h('p', { class: 'goal-stmt', text: goal.statement }));
  }
  const ms = goal.milestones || [];
  if (!ms.length) return wrap;
  const det = h('details', { class: 'goal-details' },
    h('summary', { class: 'goal-summary', text: `里程碑 ${ms.length} 条（点开）` }));
  for (const m of ms) {
    const row = h('div', { class: 'goal-ms' });
    row.append(h('span', { class: 'goal-ms-title', text: `${m.id ? m.id + ' ' : ''}${m.title || ''}` }));
    row.append(h('span', { class: 'goal-ms-weight', text: `权重 ${m.weight}` }));
    if (m.insufficient || !isNum(m.done_percent)) {
      row.append(h('span', {
        class: 'goal-ms-na', title: m.insufficient_reason || '此里程碑数据不足', text: '数据不足',
      }));
    } else {
      row.append(h('span', { class: 'goal-ms-pct', text: `${m.done_percent}%` }));
    }
    if (m.stale) {
      row.append(h('span', {
        class: 'goal-ms-stale', title: 'evidence 已超过 max_age_hours', text: 'stale',
      }));
    }
    if (m.basis) row.append(h('span', { class: 'goal-ms-basis', text: m.basis }));
    if (m.source) {
      row.append(h('span', { class: 'goal-ms-src', title: '数据来源', text: m.source }));
    }
    det.append(row);
  }
  wrap.append(det);
  return wrap;
}

function callout(kind, mark, ...body) {
  return h('div', { class: `callout callout-${kind}` },
    h('span', { class: 'co-mark', 'aria-hidden': 'true', text: mark }),
    h('div', {}, body));
}

const emptyState = (msg) => h('div', { class: 'empty-state', text: msg });

/* ============================ 数据层 ============================ */

async function apiGet(path) {
  const r = await fetch(path, { headers: { Accept: 'application/json' } });
  const text = await r.text();
  let data = null;
  try { data = JSON.parse(text); } catch (_) { /* 交给下面统一报错 */ }
  if (!r.ok) throw new Error(data && data.error ? data.error : `HTTP ${r.status}`);
  if (data === null) throw new Error('响应不是合法 JSON');
  return data;
}

/* ============================ 应用外壳 ============================ */

const el = {
  app: document.getElementById('app'),
  quota: document.getElementById('quota-strip'),
  freshness: document.getElementById('freshness'),
  loadbar: document.getElementById('load-bar'),
  refresh: document.getElementById('btn-refresh'),
  auto: document.getElementById('auto-refresh'),
  live: document.getElementById('live'),
};

// 自动刷新 30 秒；页面隐藏时不轮询——看板常年开在后台标签页，
// 没人看的时候每 30 秒重扫近 2000 个 JSON 是纯粹的浪费。
const AUTO_REFRESH_MS = 30000;

const state = {
  route: null,
  loading: false,
  timer: null,
  lastLoaded: null,
  // 记住展开态与分栏选择，自动刷新后不跳回默认。
  // allCollapsed：全局「全部收起」开关。默认 false=所有阶段默认展开（含已完成项目），
  // 这样 100% 完成的项目（如 PerlicaAnywhere/TLink，所有阶段都是 done）也把任务铺开，
  // 不会因为「已完成=收起」而整卡看着空空如也。用户可一键收起，或单独折叠某个阶段。
  ui: { openPhases: new Set(), closedPhases: new Set(), allCollapsed: false, laneMode: false, burnAll: false },
};

function setLoading(on) {
  state.loading = on;
  el.loadbar.classList.toggle('is-active', on);
  el.refresh.classList.toggle('is-spinning', on);
  el.app.classList.toggle('is-refetching', on && el.app.dataset.ready === '1');
}

function tickFreshness() {
  if (!state.lastLoaded) return;
  const secs = (Date.now() - state.lastLoaded) / 1000;
  el.freshness.textContent = secs < 60 ? '刚刚更新' : `${Math.floor(secs / 60)} 分钟前更新`;
  // 超过两个刷新周期还没成功拉到数据 = 这屏在骗人，明确标出来。
  el.freshness.classList.toggle('is-stale', secs > (AUTO_REFRESH_MS / 1000) * 2 + 15);
  el.freshness.title = `数据于 ${new Date(state.lastLoaded).toLocaleTimeString('zh-CN')} 拉取`;
}

function parseRoute() {
  const raw = (location.hash || '#/').replace(/^#/, '');
  const m = raw.match(/^\/p\/(.+)$/);
  if (m) return { view: 'project', id: decodeURIComponent(m[1]) };
  if (raw === '/burn') return { view: 'burn' };
  return { view: 'overview' };
}

function syncNav(view) {
  for (const a of document.querySelectorAll('.nav-link')) {
    const on = (view === 'burn') === (a.dataset.nav === 'burn');
    if (on) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  }
}

function renderError(err, retry) {
  return h('div', { class: 'err' },
    h('h2', { text: '加载失败' }),
    h('p', {}, '无法取得看板数据：', h('code', { text: String((err && err.message) || err) })),
    h('p', { text: '看板是只读视图，这个错误不会影响队列本身。' }),
    h('button', { class: 'btn-ghost', onclick: retry, text: '重试' }));
}

function mount(node) {
  el.app.replaceChildren(node);
  el.app.dataset.ready = '1';
}

async function load(opts) {
  const silent = !!(opts && opts.silent);
  const route = state.route;
  setLoading(true);
  try {
    let node;
    if (route.view === 'overview') node = await viewOverview();
    else if (route.view === 'project') node = await viewProject(route.id);
    else node = await viewBurn();
    if (state.route !== route) return;   // 路由在 await 期间被切走了，丢弃这次结果
    mount(node);
    state.lastLoaded = Date.now();
    tickFreshness();
    if (!silent) el.live.textContent = '看板数据已更新';
  } catch (err) {
    if (state.route !== route) return;
    mount(renderError(err, () => load()));
  } finally {
    setLoading(false);
  }
}

function scheduleAuto() {
  clearInterval(state.timer);
  state.timer = null;
  if (!el.auto.checked) return;
  state.timer = setInterval(() => {
    if (document.hidden || state.loading) return;
    load({ silent: true });
  }, AUTO_REFRESH_MS);
}

function navigate() {
  state.route = parseRoute();
  syncNav(state.route.view);
  el.app.replaceChildren(h('div', { class: 'boot', text: '正在加载…' }));
  el.app.dataset.ready = '';
  load();
}

/* ============================ 顶部额度条 ============================ */

const QUOTA_SLOTS = [
  ['claude_session', 'Claude 5h'],
  ['claude_weekly', 'Claude 周'],
  ['codex_primary', 'Codex 5h'],
  ['codex_secondary', 'Codex 周'],
];

/** 用量百分比 → 状态色。语义色，不是系列色。 */
function usageColor(pct) {
  if (pct >= 90) return 'var(--st-critical)';
  if (pct >= 75) return 'var(--st-serious)';
  if (pct >= 50) return 'var(--st-warning)';
  return 'var(--st-good)';
}

function renderQuota(quota) {
  el.quota.replaceChildren();
  if (!quota) return;
  for (const [key, label] of QUOTA_SLOTS) {
    const src = quota[key];
    // 槽位本身可能是 null（实测 codex_primary 就经常没有），此时明说「无数据」。
    if (!src) {
      el.quota.append(h('span', {
        class: 'quota-pill is-stale', title: `${label}：本机没有该窗口的用量样本`,
      },
        h('span', { class: 'qp-name', text: label }),
        h('span', { class: 'qp-val', text: '无数据' })));
      continue;
    }
    const pct = isNum(src.used_percent) ? src.used_percent : 0;
    const bits = [`${src.account_label}／${src.window_label}`, `结论：${src.verdict}`];
    if (isNum(src.minutes_to_reset)) bits.push(`${fmtDur(src.minutes_to_reset)}后重置`);
    else bits.push('源数据未提供重置时刻');
    if (src.stale) bits.push('样本已过期');
    el.quota.append(h('a', {
      class: `quota-pill${src.stale ? ' is-stale' : ''}`, href: '#/burn', title: bits.join('｜'),
    },
      h('span', { class: 'qp-name', text: label }),
      h('span', { class: 'qp-meter', 'aria-hidden': 'true' },
        h('span', { class: 'qp-fill', style: `width:${Math.min(100, pct)}%;background:${usageColor(pct)}` })),
      h('span', { class: 'qp-val', text: `${Math.round(pct)}%${src.stale ? ' ⚠' : ''}` })));
  }
}

/* ============================ 视图：总览 ============================ */

async function viewOverview() {
  const d = await apiGet('/api/overview');
  renderQuota(d.quota);

  const frag = document.createDocumentFragment();
  // board.json 解析失败必须显式披露：静默降级会让"配错了但没生效"看起来完全正常，
  // 违反 fail-honest。这条告警在总览顶端常驻，直到 board.json 修好为止。
  // 【R3·P1-2】按 error_kind 分渲染：type 错时 loadBoardOverride 会跳过出错字段但保留
  // 其他字段（json 反序列化在 UnmarshalTypeError 后仍继续填充其余字段）；syntax 错时全丢。
  // 前端旧文案一律说"全部失效"→ 与后端"type 错保留部分"的实际行为对不上,委托人按告警去找
  // 名称/阶段"没生效"实则在生效,误导排障。
  // 【R2·P1-2 加固】error_kind 是首选信道;后端 msg 前缀 也自描述("部分字段类型手误..." vs
  // "整块 JSON 语法错...") 是 belt-and-suspenders 备份信道。若某次前后端契约漂移
  // (如 kind 键 typo/被移除),裸 msg 里的自描述前缀仍可让用户识别两态,不至于错读全部失效。
  if (d.board_override_error) {
    const subText = d.board_override_error_kind === 'type'
      ? '出错字段已按类型不匹配跳过，其余 name/desc/phases/goal 覆盖仍生效。'
      : '项目 name/desc/phases/goal 覆盖已全部失效，页面显示回落自动推导。';
    frag.append(callout('critical', '⚠',
      h('div', {},
        h('strong', { text: 'board.json 未生效：' }),
        h('code', { text: d.board_override_error }),
        h('p', { class: 'callout-sub', text: subText }))));
  }
  const totals = d.totals || {};
  const activeN = (totals.queued || 0) + (totals.running || 0) +
    (totals.limit_paused || 0) + (totals.held || 0);

  frag.append(h('div', { class: 'page-head' },
    h('div', {},
      h('h1', { class: 'page-title', text: '总览' }),
      h('p', { class: 'page-sub' },
        `${d.projects.length} 个项目并行・${activeN} 张待推进・最多 ${d.max_parallel} 路并发`,
        h('span', { text: `　数据目录 ${d.root}` }))),
    h('div', { class: 'head-right' },
      h('button', {
        class: 'ghost-btn', type: 'button',
        'aria-pressed': String(state.ui.allCollapsed),
        title: state.ui.allCollapsed ? '展开所有项目的所有阶段' : '收起所有阶段的任务清单（阶段介绍仍保留）',
        onclick: () => {
          state.ui.allCollapsed = !state.ui.allCollapsed;
          // 清掉逐阶段的手动开关，让全局态说了算；否则旧的单独展开/收起会盖过全局按钮。
          state.ui.openPhases.clear();
          state.ui.closedPhases.clear();
          load({ silent: true });
        },
        text: state.ui.allCollapsed ? '全部展开' : '全部收起',
      }),
      h('div', { class: 'totals' }, STATUS_ORDER.filter((k) => totals[k] > 0).map((k) =>
        h('span', { class: 'total-chip' },
          statusDot(k, true),
          h('span', { class: 'tc-n', text: String(totals[k]) }),
          h('span', { class: 'tc-l', text: STATUS_ZH[k] })))))));

  if (!d.projects.length) {
    frag.append(emptyState('队列里还没有任何任务卡。'));
    return frag;
  }
  const grid = h('div', { class: 'project-grid' });
  for (const p of d.projects) grid.append(projectCard(p));
  frag.append(grid);
  return frag;
}

function projectCard(p) {
  const head = h('div', { class: 'proj-head' },
    h('div', { class: 'proj-titlerow' },
      h('h2', { class: 'proj-name' },
        h('a', { href: `#/p/${encodeURIComponent(p.id)}`, text: p.name })),
      h('a', { class: 'proj-open', href: `#/p/${encodeURIComponent(p.id)}`, text: '看板 →' })),
    descBlock(p.desc, p.desc_source),
    h('div', { class: 'proj-metarow' },
      p.models.slice(0, 3).map((m) => tierBadge(m.model, m.tier)),
      p.models.length > 3 ? metaChip(`+${p.models.length - 3} 个模型`) : null,
      p.dirs.length
        ? metaChip(p.dirs.length > 1 ? `${p.dirs.length} 个目录` : p.dirs[0],
          { mono: true, title: p.dirs.join('\n') })
        : null,
      relTime(p.last_activity) ? metaChip(`活动 ${relTime(p.last_activity)}`) : null),
    progressBar(p.stats, p.progress_percent),
    goalBlock(p.goal),
    statusLegend(p.stats),
    etaLine(p.eta));

  const phases = h('div', { class: 'phases' });
  for (const ph of p.phases) phases.append(phaseBlock(ph));
  return h('section', { class: 'card proj' }, head, phases);
}

function phaseBlock(ph) {
  const key = ph.id;
  // 默认展开所有阶段（含已完成的），让「所有内容都显示出来」；只有用户显式收起某阶段
  // （closedPhases）或点了「全部收起」（allCollapsed）才折叠。openPhases 记录被单独重开的阶段，
  // 优先级最高，这样在 allCollapsed 状态下也能单独展开某一个阶段。
  const open = state.ui.openPhases.has(key) ||
    (!state.ui.allCollapsed && !state.ui.closedPhases.has(key));

  // 折叠只影响「具体任务清单」。阶段名/介绍/进度/ETA 都是**阶段自身属性**，
  // 放进 <summary> 里恒常可见——折叠一个阶段不该把它是什么、进度如何一起藏掉。
  const body = h('div', { class: 'phase-body' });
  let filled = false;
  const fill = () => {
    if (filled) return;
    filled = true;
    body.append(taskList(ph.tasks, ph.stats.total));
  };

  const det = h('details', { class: 'phase', open: open || null },
    h('summary', {},
      h('div', { class: 'phase-headrow' },
        h('span', {
          class: 'phase-caret', 'aria-hidden': 'true',
          html: '<svg viewBox="0 0 12 12" width="11" height="11"><path d="M4 2.5 8 6l-4 3.5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>',
        }),
        h('span', { class: 'phase-name', text: ph.name }),
        h('span', { class: `phase-status ps-${ph.status}`, text: PHASE_ZH[ph.status] || ph.status }),
        h('span', { class: 'phase-mini' },
          h('span', { text: `${ph.stats.done}/${ph.stats.total}` }),
          progressBar(ph.stats, ph.progress_percent).querySelector('.prog'))),
      ph.desc ? h('p', { class: 'phase-desc', text: ph.desc }) : null,
      h('div', { class: 'phase-etaline' }, etaLine(ph.eta))),
    body);

  det.addEventListener('toggle', () => {
    if (det.open) { fill(); state.ui.openPhases.add(key); state.ui.closedPhases.delete(key); }
    else { state.ui.openPhases.delete(key); state.ui.closedPhases.add(key); }
  });
  if (det.open) fill();
  return det;
}

function taskList(tasks, total) {
  if (!tasks || !tasks.length) return h('div', { class: 'kcol-empty', text: '本阶段没有任务卡。' });
  const list = h('div', { class: 'task-list' });
  for (const t of tasks) list.append(taskRow(t));
  const wrap = h('div', {}, list);
  // 总览每阶段最多带 40 条（后端截断）。差额必须说出来，不能让人以为这就是全部。
  if (isNum(total) && total > tasks.length) {
    wrap.append(h('p', {
      class: 'more-hint',
      text: `显示 ${tasks.length} / 共 ${total} 张，完整清单见项目看板。`,
    }));
  }
  return wrap;
}

function taskRow(t) {
  const tags = [];
  if (t.model) tags.push(tierBadge(t.model, t.model_tier));
  if (t.runner && t.runner !== 'claude') tags.push(metaChip(t.runner, { mono: true }));
  if (t.effort) tags.push(metaChip(`档位 ${t.effort}`));
  // step 是 0-based（契约如此），展示成「第 N/M 步」必须 +1，与活动流文案对齐。
  if (t.steps_total > 1) tags.push(metaChip(`第 ${t.step + 1}/${t.steps_total} 步`));
  if (t.status === 'running' && t.elapsed_minutes > 0) tags.push(metaChip(`已跑 ${fmtDur(t.elapsed_minutes)}`));
  if (t.blocked_reason) tags.push(metaChip(t.blocked_reason, { title: t.blocked_reason }));

  // 行左缘按模型等级着色（tier-旗舰/高/中/轻/未知），一眼分清 fable / opus / sonnet。
  return h('div', { class: `task-row tier-${t.model_tier || '未知'}` },
    h('span', { class: 'tr-dot' }, statusDot(t.status)),
    h('div', { class: 'tr-main' },
      h('p', { class: 'task-title', text: t.title, title: t.title }),
      t.desc ? h('p', { class: 'task-desc', text: t.desc }) : null,
      tags.length ? h('div', { class: 'task-tags' }, tags) : null),
    h('div', { class: 'tr-right' }, statusBadge(t.status), etaShort(t.eta)));
}

/* ============================ 视图：项目 kanban ============================ */

async function viewProject(id) {
  const d = await apiGet(`/api/project?id=${encodeURIComponent(id)}`);
  // 项目页不带 quota；直接进这一页时补拉一次总览把顶部条填上。
  if (!el.quota.childElementCount) {
    apiGet('/api/overview').then((o) => renderQuota(o.quota)).catch(() => {});
  }
  const p = d.project;
  const frag = document.createDocumentFragment();

  frag.append(h('div', { class: 'page-head' },
    h('div', {},
      h('p', { class: 'page-sub' }, h('a', { href: '#/', text: '← 总览' })),
      h('h1', { class: 'page-title', text: p.name }),
      descBlock(p.desc, p.desc_source)),
    h('div', { class: 'totals' }, STATUS_ORDER.filter((k) => p.stats[k] > 0).map((k) =>
      h('span', { class: 'total-chip' },
        statusDot(k, true),
        h('span', { class: 'tc-n', text: String(p.stats[k]) }),
        h('span', { class: 'tc-l', text: STATUS_ZH[k] }))))));

  frag.append(h('div', { class: 'card', style: 'padding:13px 17px;margin-bottom:16px' },
    progressBar(p.stats, p.progress_percent),
    goalBlock(p.goal),
    etaLine(p.eta),
    p.dirs.length
      ? h('div', { class: 'proj-metarow', style: 'margin-top:9px' },
        p.dirs.map((dir) => metaChip(dir, { mono: true })))
      : null));

  // 总列 / 阶段泳道。两者渲染同一批卡、同一套 ETA（后端已统一到项目级排位）。
  const host = h('div');
  const toolbar = h('div', { class: 'kanban-toolbar' });
  const setMode = (lane) => {
    state.ui.laneMode = lane;
    for (const b of toolbar.querySelectorAll('button')) {
      b.setAttribute('aria-pressed', String(b.dataset.mode === (lane ? 'lane' : 'col')));
    }
    host.replaceChildren(lane ? lanesView(d) : kanbanView(d.columns));
  };
  toolbar.append(
    h('div', { class: 'seg', role: 'group', 'aria-label': '看板视图' },
      h('button', { type: 'button', 'data-mode': 'col', onclick: () => setMode(false), text: '总列' }),
      h('button', { type: 'button', 'data-mode': 'lane', onclick: () => setMode(true), text: '阶段泳道' })),
    h('span', { class: 'section-note', text: '点开任务卡看 prompt 摘录与估算依据' }));
  frag.append(toolbar, host);
  setMode(state.ui.laneMode);

  frag.append(activitySection(d.recent_activity));
  return frag;
}

function kanbanView(columns) {
  const k = h('div', { class: 'kanban' });
  for (const c of columns) k.append(kanbanColumn(c));
  return k;
}

function kanbanColumn(c) {
  const body = h('div', { class: 'kcol-body' });
  if (!c.tasks.length) {
    body.append(h('div', { class: 'kcol-empty', text: '空' }));
  } else {
    for (const t of c.tasks) body.append(taskCard(t));
    // done 列被后端截到 60 条。不说出来的话，622 张完成卡会被读成 60 张。
    if (c.truncated) {
      body.append(h('p', {
        class: 'more-hint',
        text: `显示 ${c.tasks.length} / 共 ${c.total} 张（已完成卡过多，只回吐最近的）`,
      }));
    }
  }
  return h('section', { class: 'kcol' },
    h('header', { class: 'kcol-head' },
      statusDot(c.key),
      h('span', { class: 'kcol-label', text: c.label }),
      h('span', {
        class: 'kcol-n',
        title: c.truncated ? `本列共 ${c.total} 张，只回吐了最近 ${c.tasks.length} 张` : null,
        text: c.truncated ? `${c.tasks.length}/${c.total}` : String(c.total),
      })),
    body);
}

const MODEL_SOURCE_ZH = { task: '卡上指定', codex_model: 'codex 侧解析', type_default: '类型默认' };

function taskCard(t) {
  const tags = [];
  // 卡片左缘改由模型等级着色（见下方 details 的 tier class），状态改由列/泳道位置 + 这枚
  // 状态徽章承载——避免「状态只靠颜色」，也把左缘这条最显眼的色带让给用户要的模型区分。
  tags.push(statusBadge(t.status));
  if (t.model) tags.push(tierBadge(t.model, t.model_tier));
  if (t.runner && t.runner !== 'claude') tags.push(metaChip(t.runner, { mono: true }));
  if (t.remote_host) tags.push(metaChip(`@${t.remote_host}`, { mono: true }));
  if (t.steps_total > 1) tags.push(metaChip(`第 ${t.step + 1}/${t.steps_total} 步`));
  if (t.status === 'running' && t.elapsed_minutes > 0) tags.push(metaChip(`已跑 ${fmtDur(t.elapsed_minutes)}`));

  const body = h('div', { class: 'tcard-body' });
  let filled = false;
  const fill = () => {
    if (filled) return;
    filled = true;
    const kv = h('dl', { class: 'kv' });
    const row = (k, v, mono) => {
      if (v === null || v === undefined || v === '') return;
      kv.append(h('dt', { text: k }), h('dd', { class: mono ? 'is-mono' : null, text: String(v) }));
    };
    row('任务 ID', t.id, true);
    row('类型', t.type);
    row('优先级', t.priority);
    row('模型来源', MODEL_SOURCE_ZH[t.model_source] || t.model_source);
    row('执行档位', t.effort);
    row('目录', t.dir, true);
    row('创建', fmtTime(t.created_at));
    row('更新', fmtTime(t.updated_at));
    if (t.attempts) row('尝试次数', t.attempts);
    if (t.fix_round) row('修复轮次', t.fix_round);
    if (t.review_of) row('复审对象', t.review_of, true);
    if (t.x_role) row('交叉角色', t.x_role);
    if (isNum(t.cost_usd) && t.cost_usd > 0) row('花费', `$${t.cost_usd.toFixed(4)}`);
    if (t.turns_used) row('轮数', t.turns_used);
    body.append(kv);

    if (t.blocked_reason) {
      body.append(callout(t.status === 'failed' ? 'critical' : 'serious', '⚠', t.blocked_reason));
    }
    if (t.last_error && t.last_error !== t.blocked_reason) {
      body.append(callout('critical', '✕', h('div', {}, h('strong', { text: '最近错误：' }), t.last_error)));
    }
    if (t.last_summary) {
      body.append(callout('good', '✓', h('div', {}, h('strong', { text: '最近小结：' }), t.last_summary)));
    }
    body.append(h('div', {}, etaLine(t.eta)));
    if (t.prompt_excerpt) {
      body.append(h('details', {},
        h('summary', {
          style: 'cursor:pointer;font-size:11.5px;color:var(--ink-mute)', text: 'prompt 摘录',
        }),
        h('pre', { class: 'excerpt', text: t.prompt_excerpt })));
    }
  };

  const det = h('details', { class: `tcard tier-${t.model_tier || '未知'}` },
    h('summary', {},
      h('p', { class: 'tcard-title', text: t.title }),
      t.desc ? h('p', { class: 'task-desc', text: t.desc }) : null,
      tags.length ? h('div', { class: 'task-tags' }, tags) : null,
      etaShort(t.eta)),
    body);
  det.addEventListener('toggle', () => { if (det.open) fill(); });
  return det;
}

function lanesView(d) {
  const wrap = h('div');
  for (const lane of d.phase_lanes) {
    const ph = lane.phase;
    const n = lane.columns.reduce((a, c) => a + c.total, 0);
    wrap.append(h('section', { class: 'lane' },
      h('div', { class: 'lane-head' },
        h('h2', { class: 'lane-title', text: ph.name }),
        h('span', { class: `phase-status ps-${ph.status}`, text: PHASE_ZH[ph.status] || ph.status }),
        h('span', { class: 'section-note', text: `${ph.stats.done}/${ph.stats.total} 完成・共 ${n} 张` }),
        etaLine(ph.eta, { hideBasis: true })),
      kanbanView(lane.columns)));
  }
  return wrap;
}

function activitySection(items) {
  const sec = h('section', { class: 'section' },
    h('div', { class: 'section-head' },
      h('h2', { class: 'section-title', text: '最近活动' }),
      h('span', {
        class: 'section-note',
        text: '活动流读 per-task events.jsonl 事件账本；seq 跳号处显式标注「事件缺口」，绝不用状态反推补齐',
      })));
  if (!items || !items.length) {
    sec.append(emptyState('没有活动记录。'));
    return sec;
  }
  const list = h('div', { class: 'card activity', style: 'padding:6px 17px' });
  for (const a of items) {
    list.append(h('div', { class: 'act-row' },
      h('span', { class: 'act-at', text: fmtTime(a.at) || '—' }),
      h('span', { class: 'act-ev', text: a.event }),
      h('span', { class: 'act-title', text: a.title, title: a.task_id })));
  }
  sec.append(list);
  return sec;
}

/* ============================ 视图：燃尽 ============================ */

async function viewBurn() {
  const d = await apiGet('/api/burn');
  const frag = document.createDocumentFragment();

  const sources = d.sources || [];
  const fresh = sources.filter((x) => !x.stale);
  const risky = fresh.filter((x) => x.verdict === '将在重置前烧完' || x.verdict === '已耗尽');

  frag.append(h('div', { class: 'page-head' },
    h('div', {},
      h('h1', { class: 'page-title', text: '额度燃尽' }),
      h('p', {
        class: 'page-sub',
        text: `${sources.length} 个「账号 × 窗口」源，其中 ${fresh.length} 个样本新鲜`,
      }))));

  // 用户最关心的那句话放最前面。
  if (risky.length) {
    frag.append(callout('critical', '⚠', h('div', {},
      h('strong', { text: `${risky.length} 个窗口会在重置前烧完：` }),
      risky.map((x) => `${x.account_label}／${x.window_label}（${Math.round(x.used_percent)}%）`).join('、'))));
  } else if (fresh.length) {
    frag.append(callout('good', '✓', '当前没有窗口按现有速率会在重置前烧完。'));
  } else {
    frag.append(callout('warning', '!', '所有额度样本都已过期，下面的百分比不代表现状。'));
  }

  frag.append(tokenSection(d.token_series || {}, d.queue_spend || {}));

  const shown = state.ui.burnAll ? sources : fresh;
  const sec = h('section', { class: 'section' },
    h('div', { class: 'section-head' },
      h('h2', { class: 'section-title', text: '各账号窗口' }),
      h('span', { class: 'section-note', text: '百分比来自 CodexBar 采样；速率只在当前窗口周期内拟合' }),
      h('button', {
        class: 'btn-ghost',
        'aria-pressed': String(state.ui.burnAll),
        onclick: () => { state.ui.burnAll = !state.ui.burnAll; load({ silent: true }); },
        text: state.ui.burnAll
          ? `隐藏过期源（共 ${sources.length}）`
          : `显示全部（含 ${sources.length - fresh.length} 个过期）`,
      })));
  if (!shown.length) {
    sec.append(emptyState('没有可显示的额度源。'));
  } else {
    const grid = h('div', { class: 'burn-grid' });
    for (const src of shown) grid.append(burnCard(src));
    sec.append(grid);
  }
  frag.append(sec);
  return frag;
}

function verdictKind(v) {
  if (v === '已耗尽' || v === '将在重置前烧完') return 'critical';
  if (v === '偏紧') return 'warning';
  if (v === '充裕') return 'good';
  return 'warning';
}

function burnCard(src) {
  const pct = isNum(src.used_percent) ? src.used_percent : 0;
  const head = h('div', { class: 'chart-head' },
    h('div', {},
      h('h3', { class: 'chart-title' }, `${src.account_label}　`,
        h('span', { style: 'font-weight:500;color:var(--ink-mute)', text: src.window_label })),
      h('p', {
        class: 'chart-sub',
        text: `采样于 ${fmtTime(src.captured_at)}（${relTime(src.captured_at)}）・${src.series.length} 个样本点`,
      })),
    h('div', { class: 'chart-actions' },
      src.stale
        ? h('span', { class: 'meta-chip', title: '样本已过期，不代表当前窗口的现状', text: '⚠ 已过期' })
        : null,
      h('span', { class: 'meta-chip', text: `${Math.round(pct)}%` })));

  // 结论横幅。每个可空字段都有自己的措辞，不含糊带过。
  const detail = [];
  detail.push(isNum(src.burn_rate_pct_per_hour) ? `速率 ${src.burn_rate_pct_per_hour}%/小时` : '速率未知');
  if (src.exhaust_at) detail.push(`预计 ${fmtTime(src.exhaust_at)} 耗尽`);
  else if (src.verdict !== '已耗尽') detail.push('无可信的耗尽时刻');
  // resets_at 为 null 与 verdict 无关：新鲜且有结论的源也可能没有重置时刻。
  if (isNum(src.minutes_to_reset)) detail.push(`${fmtDur(src.minutes_to_reset)}后重置`);
  else if (src.resets_at) detail.push(`重置于 ${fmtTime(src.resets_at)}`);
  else detail.push('源数据未提供重置时刻');

  const card = h('section', { class: 'card chart-card' }, head,
    h('div', { class: `verdict callout-${verdictKind(src.verdict)}` },
      h('span', { class: 'verdict-label', text: src.verdict }),
      h('span', { class: 'verdict-detail', text: detail.join('・') })));

  if (src.series.length < 2) {
    card.append(h('div', {
      class: 'nodata',
      text: src.series.length === 1
        ? '当前窗口周期内只有 1 个样本点，画不出趋势，也算不出燃烧速率。'
        : '当前窗口周期内没有样本点。',
    }));
  } else {
    card.append(burnChart(src));
  }
  card.append(burnTable(src));
  return card;
}

/**
 * 单个额度窗口的折线图。
 * 只有 exhaust_at 非 null（后端已过速率下限、地平线上限、不给过去时刻三道闸）
 * 且 verdict 不是「数据不足」时，才画那条虚线外推——否则一条预测线都不画。
 */
function burnChart(src) {
  const W = 560, H = 170, PAD = { t: 12, r: 16, b: 26, l: 34 };
  const pts = src.series.map((p) => ({ t: parseTime(p.t), v: p.used_percent })).filter((p) => p.t);
  if (pts.length < 2) return h('div', { class: 'nodata', text: '样本不足，不画图。' });

  const drawProjection = !!src.exhaust_at && src.verdict !== '数据不足';
  const exhaust = drawProjection ? parseTime(src.exhaust_at) : null;
  const reset = parseTime(src.resets_at);
  const tMin = pts[0].t.getTime();
  let tMax = pts[pts.length - 1].t.getTime();
  if (exhaust) tMax = Math.max(tMax, exhaust.getTime());
  if (reset) tMax = Math.max(tMax, reset.getTime());
  const span = Math.max(1, tMax - tMin);

  const x = (t) => PAD.l + ((t - tMin) / span) * (W - PAD.l - PAD.r);
  const y = (v) => PAD.t + (1 - Math.min(100, Math.max(0, v)) / 100) * (H - PAD.t - PAD.b);

  const g = sv('svg', {
    viewBox: `0 0 ${W} ${H}`, role: 'img',
    'aria-label': `${src.account_label} ${src.window_label} 用量曲线，当前 ${Math.round(src.used_percent)}%`,
  });

  for (const v of [0, 25, 50, 75, 100]) {
    g.append(sv('line', {
      x1: PAD.l, x2: W - PAD.r, y1: y(v), y2: y(v), style: 'stroke:var(--grid);stroke-width:1',
    }));
    g.append(sv('text', {
      x: PAD.l - 6, y: y(v) + 3.5, 'text-anchor': 'end',
      style: 'font-size:9px;fill:var(--ink-mute)', 'aria-hidden': 'true',
    }, String(v)));
  }

  const line = pts.map((p, i) => `${i ? 'L' : 'M'}${x(p.t.getTime()).toFixed(1)},${y(p.v).toFixed(1)}`).join(' ');
  g.append(sv('path', {
    d: `${line} L${x(pts[pts.length - 1].t.getTime()).toFixed(1)},${y(0)} L${x(tMin).toFixed(1)},${y(0)} Z`,
    style: 'fill:var(--s1);opacity:.13',
  }));
  g.append(sv('path', { d: line, style: 'fill:none;stroke:var(--s1);stroke-width:2;stroke-linejoin:round' }));
  for (const p of pts) {
    g.append(sv('circle', { cx: x(p.t.getTime()), cy: y(p.v), r: 2.4, style: 'fill:var(--s1)' },
      sv('title', {}, `${fmtTime(p.t.toISOString())}　${p.v}%`)));
  }

  if (exhaust) {
    const last = pts[pts.length - 1];
    g.append(sv('path', {
      d: `M${x(last.t.getTime()).toFixed(1)},${y(last.v).toFixed(1)} L${x(exhaust.getTime()).toFixed(1)},${y(100).toFixed(1)}`,
      style: 'fill:none;stroke:var(--st-critical);stroke-width:1.6;stroke-dasharray:4 3',
    }, sv('title', {}, `按当前速率预计 ${fmtTime(src.exhaust_at)} 打到 100%`)));
    g.append(sv('circle', { cx: x(exhaust.getTime()), cy: y(100), r: 3, style: 'fill:var(--st-critical)' }));
  }
  if (reset) {
    g.append(sv('line', {
      x1: x(reset.getTime()), x2: x(reset.getTime()), y1: PAD.t, y2: H - PAD.b,
      style: 'stroke:var(--st-good-ink);stroke-width:1.4;stroke-dasharray:3 3',
    }, sv('title', {}, `窗口于 ${fmtTime(src.resets_at)} 重置`)));
    g.append(sv('text', {
      x: Math.min(W - PAD.r - 20, x(reset.getTime()) + 3), y: PAD.t + 9,
      style: 'font-size:9px;fill:var(--ink-mute)',
    }, '重置'));
  }
  g.append(sv('text', { x: PAD.l, y: H - 7, style: 'font-size:9px;fill:var(--ink-mute)' },
    fmtTime(pts[0].t.toISOString()) || ''));
  g.append(sv('text', {
    x: W - PAD.r, y: H - 7, 'text-anchor': 'end', style: 'font-size:9px;fill:var(--ink-mute)',
  }, fmtTime(new Date(tMax).toISOString()) || ''));

  const legend = h('div', { class: 'chart-legend' },
    h('span', { class: 'cl-item' },
      h('span', { class: 'cl-line', style: 'border-top-color:var(--s1)' }), '实测用量'),
    exhaust ? h('span', { class: 'cl-item' },
      h('span', { class: 'cl-line', style: 'border-top-color:var(--st-critical);border-top-style:dashed' }),
      '按当前速率外推') : null,
    reset ? h('span', { class: 'cl-item' },
      h('span', { class: 'cl-line', style: 'border-top-color:var(--st-good-ink);border-top-style:dashed' }),
      '重置时刻') : null,
    !exhaust ? h('span', { class: 'cl-item', text: '（数据不足以外推，未画预测线）' }) : null);

  return h('div', {}, h('div', { class: 'chart-host' }, g), legend);
}

/** 每张图的无障碍替身：同一份数字的表格视图。 */
function burnTable(src) {
  const det = h('details', {},
    h('summary', {
      style: 'cursor:pointer;font-size:11.5px;color:var(--ink-mute);margin-top:8px',
      text: '查看样本数值',
    }));
  det.append(
    h('div', { class: 'tbl-wrap' },
      h('table', { class: 'tbl' },
        h('thead', {}, h('tr', {}, h('th', { text: '采样时刻' }), h('th', { text: '用量 %' }))),
        h('tbody', {}, src.series.map((p) =>
          h('tr', {}, h('td', { text: fmtTime(p.t) || p.t }), h('td', { text: String(p.used_percent) })))))),
    h('p', {
      class: 'chart-sub',
      text: `账号键 ${src.account_key}・窗口 ${src.window}（${src.window_minutes} 分钟）`,
    }));
  return det;
}

/* ---- token 曲线 ---- */

function tokenSection(ts, spend) {
  const sec = h('section', { class: 'section' },
    h('div', { class: 'section-head' },
      h('h2', { class: 'section-title', text: 'Token 用量曲线' }),
      h('span', {
        class: 'section-note',
        text: `最近 24 小时・${ts.bucket_minutes || 15} 分钟一桶・来自 transcript，不分账号`,
      })));

  const comp = ts.by_component || {};
  const total = Object.values(comp).reduce((a, b) => a + b, 0);
  const fresh = (comp.input || 0) + (comp.output || 0) + (comp.cache_creation || 0);

  sec.append(h('div', { class: 'tiles' },
    tile('等权口径合计', compact(total), 'token', 'input+output+cache_creation+cache_read 四项等权相加'),
    tile('其中缓存读取', compact(comp.cache_read || 0),
      total ? `${((comp.cache_read || 0) / total * 100).toFixed(1)}%` : '',
      '缓存读取的额度成本远低于全价，占比高时等权合计会严重高估消耗'),
    tile('真正新处理', compact(fresh), 'token', '不含缓存读取'),
    tile('额度口径折算', compact(ts.weighted_total || 0), 'token',
      '按 budget.go 权重（cache_read 计 0.1）折算'),
    tile(`队列账本近 ${spend.window_hours || 5} 小时`, compact(spend.weighted_tokens || 0), 'token',
      '来自 ~/.claudego/usage.json，只含本队列派发的 claude 调用')));

  // 口径披露原样呈现：两个数字口径差一个数量级，不说清楚就是误导。
  if (ts.basis) sec.append(h('div', { style: 'margin-top:12px' }, callout('warning', 'ⓘ', ts.basis)));

  if (!ts.points || !ts.points.length) {
    sec.append(h('div', {
      class: 'nodata', style: 'margin-top:12px', text: '最近 24 小时没有 transcript 用量样本。',
    }));
    return sec;
  }
  sec.append(h('div', { class: 'card chart-card', style: 'margin-top:14px' }, tokenChart(ts)));

  if (spend.by_model && Object.keys(spend.by_model).length) {
    const rows = Object.entries(spend.by_model).sort((a, b) => b[1] - a[1]);
    sec.append(h('section', { class: 'card chart-card', style: 'margin-top:14px' },
      h('h3', { class: 'chart-title', text: `队列账本（近 ${spend.window_hours} 小时，额度口径）` }),
      h('p', {
        class: 'chart-sub',
        text: '与上面的曲线不是同一口径，也不是同一批样本：这里只有本队列派发的调用。',
      }),
      h('div', { class: 'tbl-wrap' }, h('table', { class: 'tbl' },
        h('thead', {}, h('tr', {}, h('th', { text: '模型' }), h('th', { text: '加权 token' }))),
        h('tbody', {}, rows.map(([m, v]) =>
          h('tr', {}, h('td', { text: m }), h('td', { text: num(v) }))))))));
  }
  return sec;
}

function tile(label, value, unit, foot) {
  return h('div', { class: 'card tile' },
    h('p', { class: 'tile-label', text: label }),
    h('div', { class: 'tile-value' }, value, unit ? h('span', { class: 'tile-unit', text: unit }) : null),
    foot ? h('p', { class: 'tile-foot', text: foot }) : null);
}

/** 按模型堆叠的面积图 + 游标 tooltip + 表格替身。 */
function tokenChart(ts) {
  const W = 900, H = 260, PAD = { t: 14, r: 16, b: 26, l: 54 };
  const models = (ts.models || []).slice();
  const pts = ts.points.map((p) => ({ t: parseTime(p.t), by: p.by_model || {} })).filter((p) => p.t);
  if (!pts.length) return h('div', { class: 'nodata', text: '没有样本。' });

  const tMin = pts[0].t.getTime();
  const tMax = Math.max(pts[pts.length - 1].t.getTime(), tMin + 1);
  let vMax = 0;
  for (const p of pts) {
    let sum = 0;
    for (const m of models) sum += p.by[m] || 0;
    vMax = Math.max(vMax, sum);
  }
  vMax = vMax || 1;

  const x = (t) => PAD.l + ((t - tMin) / (tMax - tMin)) * (W - PAD.l - PAD.r);
  const y = (v) => PAD.t + (1 - v / vMax) * (H - PAD.t - PAD.b);

  const g = sv('svg', {
    viewBox: `0 0 ${W} ${H}`, role: 'img',
    'aria-label': `最近 24 小时按模型分的 token 用量堆叠面积图，峰值 ${num(vMax)}`,
  });
  for (let i = 0; i <= 4; i++) {
    const v = (vMax / 4) * i;
    g.append(sv('line', {
      x1: PAD.l, x2: W - PAD.r, y1: y(v), y2: y(v), style: 'stroke:var(--grid);stroke-width:1',
    }));
    g.append(sv('text', {
      x: PAD.l - 7, y: y(v) + 3.5, 'text-anchor': 'end', style: 'font-size:9px;fill:var(--ink-mute)',
    }, compact(v)));
  }

  // 自下而上堆叠
  const base = pts.map(() => 0);
  models.forEach((m, mi) => {
    const color = `var(${SERIES_COLORS[mi % SERIES_COLORS.length]})`;
    const top = pts.map((p, i) => base[i] + (p.by[m] || 0));
    const up = pts.map((p, i) => `${i ? 'L' : 'M'}${x(p.t.getTime()).toFixed(1)},${y(top[i]).toFixed(1)}`).join(' ');
    const down = pts.map((_, i) => {
      const j = pts.length - 1 - i;
      return `L${x(pts[j].t.getTime()).toFixed(1)},${y(base[j]).toFixed(1)}`;
    }).join(' ');
    g.append(sv('path', { d: `${up} ${down} Z`, style: `fill:${color};opacity:.72` },
      sv('title', {}, m)));
    for (let i = 0; i < pts.length; i++) base[i] = top[i];
  });

  const cursor = sv('line', { y1: PAD.t, y2: H - PAD.b, style: 'stroke:var(--axis);stroke-width:1;opacity:0' });
  g.append(cursor);
  g.append(sv('text', { x: PAD.l, y: H - 7, style: 'font-size:9px;fill:var(--ink-mute)' },
    fmtTime(pts[0].t.toISOString()) || ''));
  g.append(sv('text', {
    x: W - PAD.r, y: H - 7, 'text-anchor': 'end', style: 'font-size:9px;fill:var(--ink-mute)',
  }, fmtTime(pts[pts.length - 1].t.toISOString()) || ''));

  const tip = h('div', { class: 'tooltip' });
  const host = h('div', { class: 'chart-host' }, g, tip);
  host.addEventListener('pointermove', (ev) => {
    const box = host.getBoundingClientRect();
    if (!box.width) return;
    const relX = ((ev.clientX - box.left) / box.width) * W;
    let best = 0, bestD = Infinity;
    pts.forEach((p, i) => {
      const dd = Math.abs(x(p.t.getTime()) - relX);
      if (dd < bestD) { bestD = dd; best = i; }
    });
    const p = pts[best];
    cursor.setAttribute('x1', x(p.t.getTime()));
    cursor.setAttribute('x2', x(p.t.getTime()));
    cursor.style.opacity = '1';
    const rows = models.map((m, mi) => [m, p.by[m] || 0, mi])
      .filter((r) => r[1] > 0).sort((a, b) => b[1] - a[1]);
    tip.replaceChildren(
      h('div', { class: 'tt-t', text: fmtTime(p.t.toISOString()) || '' }),
      ...rows.map(([m, v, mi]) => h('div', { class: 'tt-row' },
        h('span', { class: 'cl-sw', style: `background:var(${SERIES_COLORS[mi % SERIES_COLORS.length]})` }),
        h('span', { text: m }),
        h('span', { class: 'n', text: compact(v) }))),
      rows.length ? null : h('div', { class: 'tt-row', text: '无用量' }));
    tip.classList.add('is-on');
    const px = (x(p.t.getTime()) / W) * box.width;
    tip.style.left = `${Math.max(0, Math.min(box.width - 190, px + 12))}px`;
    tip.style.top = '10px';
  });
  host.addEventListener('pointerleave', () => {
    tip.classList.remove('is-on');
    cursor.style.opacity = '0';
  });

  const legend = h('div', { class: 'chart-legend' }, models.map((m, mi) =>
    h('span', { class: 'cl-item' },
      h('span', { class: 'cl-sw', style: `background:var(${SERIES_COLORS[mi % SERIES_COLORS.length]})` }), m)));

  const table = h('details', {},
    h('summary', {
      style: 'cursor:pointer;font-size:11.5px;color:var(--ink-mute);margin-top:8px', text: '查看数值表',
    }),
    h('div', { class: 'tbl-wrap' }, h('table', { class: 'tbl' },
      h('thead', {}, h('tr', {}, h('th', { text: '时刻' }), models.map((m) => h('th', { text: m })))),
      h('tbody', {}, pts.map((p) => h('tr', {},
        h('td', { text: fmtTime(p.t.toISOString()) || '' }),
        models.map((m) => h('td', { text: p.by[m] ? num(p.by[m]) : '—' }))))))));

  return h('div', {},
    h('div', { class: 'chart-head' },
      h('div', {},
        h('h3', { class: 'chart-title', text: '按模型分的 token 吞吐（等权口径）' }),
        h('p', {
          class: 'chart-sub',
          text: '纵轴含缓存读取且按全价计入，不等同于额度消耗——额度口径见上面的磁贴。',
        }))),
    host, legend, table);
}

/* ============================ 启动 ============================ */

el.refresh.addEventListener('click', () => load());
el.auto.addEventListener('change', scheduleAuto);
window.addEventListener('hashchange', navigate);
document.addEventListener('visibilitychange', () => {
  // 从后台切回来先补一次刷新，别让用户对着几分钟前的数字做决定。
  if (!document.hidden && el.auto.checked && state.lastLoaded &&
      Date.now() - state.lastLoaded > AUTO_REFRESH_MS) {
    load({ silent: true });
  }
});
setInterval(tickFreshness, 15000);

navigate();
scheduleAuto();
