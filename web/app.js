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
/**
 * 状态的呈现次序：**已尘埃落定的在左，越往右越需要人管**。
 *
 *   已取消 → 已完成 │ 进行中 → 排队中 → 限额暂停 → 已挂起 → 失败
 *   └── 不会再动的 ──┘ └────── 按"离完成还有多远"递增 ──────┘
 *
 * 进度条是一条**填充计**（像电量条），左边那截就是"已经不用再管的部分"，
 * 于是"未完成的都堆在右侧"这个直觉才成立。旧次序把 running/queued 放最左、
 * done 放右边第二，读起来是"进度条越长越糟"——与所有人对进度条的默认预期相反。
 *
 * 右半段内部按「离完成还有多远」递增：进行中(正在跑) → 排队中(等槽位) →
 * 限额暂停(等额度) → 已挂起(等人) → 失败(坏了)。前三档机器自己会往前走，
 * 后两档必须人介入——右端因此天然是"该看这里"。
 *
 * 这个次序同时管进度条分段、状态图例、页头状态芯片三处：三者是同一份读数的
 * 三种呈现，各排各的会让芯片与它下面那条彩带对不上号。
 *
 * **不管 kanban 的列序**（那个在后端 boardColumnOrder）：看板是工作流看板，
 * 卡从左往右流向"已完成"是通用惯例；把 done 挪到最左会把它读成起点。
 * 填充计与流程板是两套不同的隐喻，各守各的惯例才对。
 */
const STATUS_ORDER = ['canceled', 'done', 'running', 'queued', 'limit_paused', 'held', 'failed'];

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

const KIND_ZH = {
  design: '设计', impl: '落地', fix: '修复', review: '审核', coord: '协调',
};
const KIND_SOURCE_ZH = {
  review_of: '被审卡链接（盘上事实）',
  fix_round: '修复轮次字段（盘上事实）',
  x_role: '交叉验证链角色（盘上事实）',
  type: '任务类型（盘上事实）',
  override: 'board.json 人工规则',
  title: '标题关键词（启发式，可能判错）',
  default: '兜底：未命中任何信号，按待落地计',
};

const KIND_BASIS = '每行与总条同口径（已完成 ÷ 非取消卡）。审核／修复卡张数大、完成率天然高，'
  + '只看总条会高估落地进度。分类优先取卡上的 review_of／fix_round／类型等结构信号，'
  + '标题关键词垫底，判不出的按待落地计。';

// 每一桶收哪些卡，悬停在类别名上可见——不写清楚的话，「落地 41%」的分母里到底
// 混了什么全靠猜，尤其是"判不出类别的卡也算落地"这条必须让人知道。
const KIND_SCOPE = {
  design: '设计／方案／调研类卡（按标题关键词判定，可能判错）',
  impl: '落地实现卡；判不出类别的卡也计在这里——本页要防的是低估剩余工作量，故往保守一侧偏',
  fix: '自动修复卡（fix_round>0，或标题「修复R<n>:」）',
  review: '对抗审核／复审卡（review_of 非空、design-review 类型，或交叉验证链的 C 卡）',
  coord: '协调／收口／进度回收类记账卡（coordinate、progress-pull、prompt-assembly 等）',
};

/**
 * 按工作性质拆分的进度块。与总条**并列而非替换**。
 *
 * 【为什么必须拆】总条口径是「done ÷ 非取消卡」，没算错，但它把三种性质完全不同的活
 * 按张数等权平均了：审核卡与修复卡生命周期短、完成率天然高，张数又常占七成以上，
 * 于是总条被它们抬到 90% 上下，而真正的落地卡可能才走了四成。
 * 拆开后「审核 96% ／ 落地 41%」一眼可见，读者不会再把总条读成"快完了"。
 *
 * 【为什么总条还留着】它是唯一与历史读数可比的口径，删掉等于把过去所有截图作废；
 * 而且分桶依赖启发式，总条是那条不依赖任何分类判断的锚。
 */
function kindProgress(stats, pct, kinds, opts) {
  const o = opts || {};
  const wrap = h('div', { class: 'prog-group' });
  wrap.append(h('div', { class: 'prog-row is-total' },
    h('span', { class: 'prog-key', title: '全部卡的完成占比，分母已排除已取消卡', text: '总进度' }),
    h('span', { class: 'prog-n', text: `${stats.done || 0}/${(stats.total || 0) - (stats.canceled || 0)}` }),
    progressBar(stats, pct)));

  if (!kinds || !kinds.length) return wrap;
  for (const k of kinds) {
    const den = (k.stats.total || 0) - (k.stats.canceled || 0);
    wrap.append(h('div', { class: 'prog-row' },
      h('span', { class: 'prog-key', title: KIND_SCOPE[k.key] || k.label, text: k.label }),
      h('span', { class: 'prog-n', text: `${k.stats.done || 0}/${den}` }),
      progressBar(k.stats, k.progress_percent)));
  }
  // 总览一屏并排十来个项目，整段口径说明逐列重复会把任务清单挤出视口；
  // 压成一行 + 悬停给全文，说明本身一个字都没少（项目页仍给全文）。
  wrap.append(o.compact
    ? h('p', { class: 'prog-note', title: KIND_BASIS, text: '口径同总条；分类依据见悬停说明 ⓘ' })
    : h('p', { class: 'prog-note', text: KIND_BASIS }));
  return wrap;
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

/**
 * 往 DocumentFragment 上挂一个**可能为 null** 的节点。
 *
 * 原生 append 不像 h() 那样跳过空值——它会把 null 转成字符串 "null" 插进页面
 * （实测两个可选横幅都不显示时，页面顶上就多出一行 "nullnull"）。
 * 凡是"条件才出现"的区块一律走这里，别再手写 if。
 */
function appendMaybe(parent, node) {
  if (node) parent.append(node);
}

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

/**
 * 唯一的写入通道（目前只有归档）。Content-Type 必须是 application/json——
 * 后端拿它当 CSRF 闸门之一（HTML 表单发不出这个类型），不是可选的装饰。
 */
async function apiPost(path, body) {
  const r = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify(body),
  });
  const text = await r.text();
  let data = null;
  try { data = JSON.parse(text); } catch (_) { /* 同上 */ }
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

// HIDDEN_STATUS_KEY 必须声明在 state 之前：state 的初始化会调 loadHiddenStatuses()，
// 而 const 在声明之前处于 TDZ——放到后面会抛 ReferenceError，又被那里的 try/catch
// 吞成"读不出，按不筛选处理"。表现是筛选静默失效、localStorage 里明明存着值。
const HIDDEN_STATUS_KEY = 'claudego.board.hiddenStatuses';
const PROJECT_ORDER_KEY = 'claudego.board.projectOrder';

const state = {
  route: null,
  loading: false,
  timer: null,
  lastLoaded: null,
  // 记住展开态与分栏选择，自动刷新后不跳回默认。
  // allCollapsed：全局「全部收起」开关。默认 false=所有阶段默认展开（含已完成项目），
  // 这样 100% 完成的项目（如 PerlicaAnywhere/TLink，所有阶段都是 done）也把任务铺开，
  // 不会因为「已完成=收起」而整卡看着空空如也。用户可一键收起，或单独折叠某个阶段。
  // flash 是一次性提示（归档写入失败等）。写操作失败必须看得见——
  // 只在 console 里报错等于让用户以为"点了就生效了"，而盘上什么都没变。
  flash: null,
  // pendingGripFocus：键盘移动项目后要还回焦点的那个手柄的 project id。
  pendingGripFocus: null,
  ui: {
    openPhases: new Set(), closedPhases: new Set(), allCollapsed: false,
    laneMode: false, burnAll: false,
    // showArchived：总览是否把已归档项目也铺出来（默认折叠，这正是归档的用途）。
    showArchived: false,
    // hiddenStatuses：被折叠掉的状态（只影响**卡片清单**，见 statusFilterChips 的注释）。
    hiddenStatuses: loadHiddenStatuses(),
    // projectOrder：人工拖拽出来的项目次序（project id 数组，空=用后端的默认排序）。
    projectOrder: loadProjectOrder(),
    // spendRange：队列消耗的统计窗口。默认 7 天而不是 24 小时——
    // 24 小时里常常只跑过一两个模型（实测就是这样），一进来只看到一个模型
    // 会被当成"看板漏了数据"，而真实原因只是窗口太窄。
    spendRange: '7d',
  },
};

/* ---- 项目次序（拖拽排序）---- */

/**
 * 人工次序存 localStorage，与状态筛选同一套理由：这是**观看偏好**，
 * 不是队列事实——不同机器/不同人盯的重点不一样，没道理互相覆盖。
 * （归档走服务端是因为那是"这个项目还看不看"的判断，性质不同。）
 */
function loadProjectOrder() {
  try {
    const raw = localStorage.getItem(PROJECT_ORDER_KEY);
    if (!raw) return [];
    const arr = JSON.parse(raw);
    return Array.isArray(arr) ? arr.filter((x) => typeof x === 'string') : [];
  } catch (_) {
    return [];
  }
}

function saveProjectOrder() {
  try {
    localStorage.setItem(PROJECT_ORDER_KEY, JSON.stringify(state.ui.projectOrder));
  } catch (_) { /* 存不进就只在本次会话里生效 */ }
}

/**
 * 按人工次序重排项目。
 *
 * 【没排到的怎么办】人工次序是一张快照，新项目不在里面。把它们**排到最前**，
 * 不是排到最后：轨道是横向滚动的，末尾意味着要横滚好几屏才看得见——
 * 一个刚冒出来的项目恰恰是最该被看见的。orderNote() 会把"有几个是新的"说出来，
 * 免得用户以为自己的次序被打乱了。
 *
 * 【为什么不落库到服务端】见 loadProjectOrder。
 */
function orderProjects(projects) {
  const order = state.ui.projectOrder;
  if (!order.length) return projects;
  const rank = new Map(order.map((id, i) => [id, i]));
  const known = [];
  const fresh = [];
  for (const p of projects) (rank.has(p.id) ? known : fresh).push(p);
  known.sort((a, b) => rank.get(a.id) - rank.get(b.id));
  return fresh.concat(known);
}

/** 把当前铺出来的次序落盘。只记这一批的 id，未展示的项目次序原样保留。 */
function commitProjectOrder(ids) {
  const rest = state.ui.projectOrder.filter((id) => !ids.includes(id));
  state.ui.projectOrder = ids.concat(rest);
  saveProjectOrder();
  load({ silent: true });
}

function clearProjectOrder() {
  state.ui.projectOrder = [];
  saveProjectOrder();
  load({ silent: true });
}

/** 人工次序生效时的披露。默认排序本身带信息量（有活儿的排前面），盖掉了就得说。 */
function orderNote(shown) {
  if (!state.ui.projectOrder.length) return null;
  const known = new Set(state.ui.projectOrder);
  const freshN = shown.filter((p) => !known.has(p.id)).length;
  return h('div', { class: 'filter-note' },
    h('span', { class: 'fn-mark', 'aria-hidden': 'true', text: '⇅' }),
    h('span', { class: 'fn-text' },
      '项目按手动次序排列，已覆盖默认的「有活儿的排前面・其次按最近活动」。',
      freshN ? h('strong', { text: `其中 ${freshN} 个是排序之后新出现的，暂列最前。` }) : null),
    h('button', {
      class: 'ghost-btn', type: 'button', onclick: clearProjectOrder, text: '恢复默认排序',
    }));
}

/* ---- 状态筛选（只藏清单，不动读数）---- */

/**
 * 筛选状态存 localStorage：一屏上千张卡，把「已完成」藏起来是常态操作，
 * 每次刷新都要重设一遍的话没人会用。代价是"刷新后还藏着"，
 * 靠 filterNote() 的常驻横幅兜住——藏了什么必须一直看得见。
 * 读不出/存不进（隐私模式、配额满）一律静默降级成"不筛选"：
 * 这是个显示偏好，不该让它把整页拖挂。
 */
function loadHiddenStatuses() {
  try {
    const raw = localStorage.getItem(HIDDEN_STATUS_KEY);
    if (!raw) return new Set();
    const arr = JSON.parse(raw);
    // 过一遍白名单：手改过的 localStorage 不该往状态集里塞未知键。
    return new Set(Array.isArray(arr) ? arr.filter((k) => STATUS_ORDER.includes(k)) : []);
  } catch (_) {
    return new Set();
  }
}

function saveHiddenStatuses() {
  try {
    localStorage.setItem(HIDDEN_STATUS_KEY, JSON.stringify([...state.ui.hiddenStatuses]));
  } catch (_) { /* 存不进就只在本次会话里生效，不值得报错打断 */ }
}

const statusHidden = (s) => state.ui.hiddenStatuses.has(s);

function toggleStatus(s) {
  if (statusHidden(s)) state.ui.hiddenStatuses.delete(s);
  else state.ui.hiddenStatuses.add(s);
  saveHiddenStatuses();
  load({ silent: true });
}

function clearStatusFilter() {
  state.ui.hiddenStatuses.clear();
  saveHiddenStatuses();
  load({ silent: true });
}

/**
 * 状态计数条，同时是筛选开关。
 *
 * 【纪律】筛选**只藏卡片清单**，绝不改任何读数：这排数字、进度条、各分类桶、ETA
 * 一律按全部卡算。藏掉「已完成」之后进度条跟着掉下去的话，那不是筛选，那是伪造快照
 * ——用户会拿着一个自己无意中造出来的数字去做决定。
 * 所以数字**永远显示全量**，哪怕该状态的卡一张都没铺出来。
 */
function statusFilterChips(counts) {
  const wrap = h('div', { class: 'totals' });
  for (const k of STATUS_ORDER) {
    if (!counts[k]) continue;
    const off = statusHidden(k);
    wrap.append(h('button', {
      class: `total-chip${off ? ' is-off' : ''}`, type: 'button',
      'aria-pressed': String(!off),
      title: off
        ? `「${STATUS_ZH[k]}」的卡片清单当前已隐藏，点击恢复。计数 ${counts[k]} 与进度不受筛选影响。`
        : `点击隐藏「${STATUS_ZH[k]}」的卡片清单。计数与进度仍按全部卡算，不会跟着变。`,
      onclick: () => toggleStatus(k),
    },
      statusDot(k, true),
      h('span', { class: 'tc-n', text: String(counts[k]) }),
      h('span', { class: 'tc-l', text: STATUS_ZH[k] })));
  }
  return wrap;
}

/** 筛选生效时的常驻披露。藏了东西却不说，就成了"这个项目怎么空了"的悬案。 */
function filterNote() {
  const hidden = STATUS_ORDER.filter(statusHidden);
  if (!hidden.length) return null;
  return h('div', { class: 'filter-note' },
    h('span', { class: 'fn-mark', 'aria-hidden': 'true', text: '⃠' }),
    h('span', { class: 'fn-text' },
      `已隐藏 ${hidden.map((k) => STATUS_ZH[k]).join('、')} 的卡片清单——`,
      h('strong', { text: '上方计数、进度条与 ETA 仍按全部卡计算' }),
      '，未受筛选影响。'),
    h('button', { class: 'ghost-btn', type: 'button', onclick: clearStatusFilter, text: '全部显示' }));
}

/** 归档 / 取消归档一个项目。只写看板自己的视图状态，任务卡一个字节都不动。 */
async function setArchived(id, archived) {
  try {
    await apiPost('/api/project/archive', { id, archived });
    el.live.textContent = archived ? '项目已归档' : '项目已恢复活跃';
  } catch (err) {
    state.flash = { kind: 'critical', text: `归档状态写入失败：${(err && err.message) || err}` };
  }
  await load({ silent: true });
}

/** 取出并清空一次性提示——它是"这次操作的结果"，不该在下一次渲染里继续常驻。 */
function takeFlash() {
  const f = state.flash;
  state.flash = null;
  if (!f) return null;
  return callout(f.kind, f.kind === 'critical' ? '✕' : 'ⓘ', f.text);
}

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
  syncRailHeight();
  // 再补一帧：字体/图标加载完、顶栏额度条换行与否定下来之后，轨道顶端还会挪一次。
  // 只测一次的话会把"布局还没落定"时的位置当成最终值，列高卡在 min-height 上，
  // 外层滚动条就回来了。
  requestAnimationFrame(syncRailHeight);
  restoreGripFocus();
}

/** 键盘移动项目后把焦点还给同一个手柄（见 dragGrip 里 pendingGripFocus 的注释）。 */
function restoreGripFocus() {
  const id = state.pendingGripFocus;
  if (!id) return;
  state.pendingGripFocus = null;
  const grip = el.app.querySelector(`.proj[data-pid="${CSS.escape(id)}"] .drag-grip`);
  if (grip) grip.focus();
}

/**
 * 按轨道的实际位置算出列高（--rail-h），让轨道正好落在视口底边。
 *
 * 为什么不写死一个常数：页头高度会变——项目数变化会换行、数据目录路径长了会折成两行、
 * 多出「已归档 N」按钮也会撑高。常数一旦对不上就会同时出现外层与列内两条滚动条，
 * 那正是横排要解决的问题（先纵向滚回去才能横滚）。
 *
 * 用**文档相对**的顶端而不是视口相对：页面已经滚过一段时再测，视口相对值会偏小、
 * 算出一个比视口还高的列，于是外层滚动条又回来了。
 * 轨道顶端只由它上面的内容决定、与列高无关，所以这里不会触发反复重排。
 */
function syncRailHeight() {
  const rail = el.app.querySelector('.project-rail');
  if (!rail) return;
  const docTop = rail.getBoundingClientRect().top + window.scrollY;
  const padBottom = parseFloat(getComputedStyle(el.app).paddingBottom) || 0;
  // 12 = 轨道自身给横向滚动条留的下边距
  const h = Math.max(320, Math.round(window.innerHeight - docTop - padBottom - 12));
  const next = `${h}px`;
  // 值没变就不写：写入会触发 ResizeObserver 再跑一遍，同值短路让它一轮就收敛。
  if (document.documentElement.style.getPropertyValue('--rail-h') === next) return;
  document.documentElement.style.setProperty('--rail-h', next);
}

// 视口/布局尺寸一变就重算列高。为什么不只监听 window.resize：顶栏额度条换不换行、
// 页头路径折几行都会挪动轨道顶端，而这些变化不一定伴随 window 尺寸事件。
// 观察 #app 会形成"改列高 → app 变高 → 再触发"的回路，靠 syncRailHeight 的同值短路一轮收敛。
if (typeof ResizeObserver === 'function') {
  new ResizeObserver(syncRailHeight).observe(el.app);
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

/**
 * 剩余额度百分比。全站额度读数的**唯一主口径**。
 *
 * 为什么主读数是剩余而不是已用：用户在这一屏要做的决定是「还能不能再派一批卡」，
 * 那是「还剩多少」的直接函数；「已经烧了多少」要先在脑子里做一次减法才能用。
 * 后端 remaining_percent 已钳到 [0,100]，这里的 100-used 只是老响应的兜底
 * （字段缺失 → null，绝不折成 0：0 会被读成「一点不剩」，那是编造）。
 */
function remainPct(src) {
  if (!src) return null;
  if (isNum(src.remaining_percent)) return src.remaining_percent;
  if (isNum(src.used_percent)) return Math.max(0, Math.min(100, 100 - src.used_percent));
  return null;
}

/** 剩余额度 → 状态色。语义色，不是系列色；阈值与 usedColor 互补，只在这一处定义。 */
function remainColor(rem) {
  if (rem <= 10) return 'var(--st-critical)';
  if (rem <= 25) return 'var(--st-serious)';
  if (rem <= 50) return 'var(--st-warning)';
  return 'var(--st-good)';
}

function renderQuota(quota) {
  el.quota.replaceChildren();
  if (!quota) return;
  for (const [key, label] of QUOTA_SLOTS) {
    const src = quota[key];
    const rem = remainPct(src);
    // 槽位可能是 null（实测 codex_primary 就经常没有），样本在但百分比字段缺失也可能。
    // 两种都明说「无数据」——不拿 0% 或 100% 顶上。
    if (!src || rem === null) {
      el.quota.append(h('span', {
        class: 'quota-pill is-stale',
        title: `${label}：本机没有该窗口的可用量样本，剩余额度未知`,
      },
        h('span', { class: 'qp-name', text: label }),
        h('span', { class: 'qp-val', text: '无数据' })));
      continue;
    }
    const bits = [`${src.account_label}／${src.window_label}`];
    if (isNum(src.used_percent)) bits.push(`已用 ${Math.round(src.used_percent)}%`);
    bits.push(`结论：${src.verdict}`);
    if (isNum(src.minutes_to_reset)) bits.push(`${fmtDur(src.minutes_to_reset)}后重置`);
    else bits.push('源数据未提供重置时刻');
    if (src.stale) bits.push('样本已过期');
    el.quota.append(h('a', {
      class: `quota-pill${src.stale ? ' is-stale' : ''}`, href: '#/burn', title: bits.join('｜'),
    },
      h('span', { class: 'qp-name', text: label }),
      // 条形填的是**剩余**：条越短越危险，与「剩余 8%」的文字同向。
      // 填已用而写剩余会让长条配小数字，是最容易读反的组合。
      h('span', { class: 'qp-meter', 'aria-hidden': 'true' },
        h('span', { class: 'qp-fill', style: `width:${rem}%;background:${remainColor(rem)}` })),
      h('span', { class: 'qp-val', text: `剩 ${Math.round(rem)}%${src.stale ? ' ⚠' : ''}` })));
  }
}

/* ============================ 视图：总览 ============================ */

async function viewOverview() {
  const d = await apiGet('/api/overview');
  renderQuota(d.quota);

  const frag = document.createDocumentFragment();
  const flash = takeFlash();
  if (flash) frag.append(flash);
  // 归档状态读失败必须显式披露：静默按"未归档"渲染会让用户手动折叠的项目一次性全冒出来，
  // 而界面上零提示——与 board.json 解析失败同一条纪律。
  if (d.archive_state_error) {
    frag.append(callout('warning', '⚠', h('div', {},
      h('strong', { text: '归档状态未生效：' }),
      h('code', { text: d.archive_state_error }))));
  }
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

  const all = d.projects || [];
  const live = all.filter((p) => !p.archived);
  const archived = all.filter((p) => p.archived);

  const sub = h('p', { class: 'page-sub' },
    `${live.length} 个项目并行・${activeN} 张待推进・最多 ${d.max_parallel} 路并发`);
  // 顶部状态计数覆盖**全部**卡（含已归档项目的），这与下面只铺活跃项目的列表不同口径。
  // 不写出来的话，"计数 900 张但只看得见 5 个项目"会被读成有卡凭空消失了。
  if (archived.length) {
    sub.append(h('span', { text: `　已归档 ${archived.length} 个项目（下方状态计数仍含它们的卡）` }));
  }
  sub.append(h('span', { text: `　数据目录 ${d.root}` }));

  frag.append(h('div', { class: 'page-head' },
    h('div', {}, h('h1', { class: 'page-title', text: '总览' }), sub),
    h('div', { class: 'head-right' },
      archived.length
        ? h('button', {
          class: 'ghost-btn', type: 'button',
          'aria-pressed': String(state.ui.showArchived),
          title: '已归档项目默认折叠；点开可临时查看并取消归档',
          onclick: () => { state.ui.showArchived = !state.ui.showArchived; load({ silent: true }); },
          text: state.ui.showArchived ? `隐藏已归档（${archived.length}）` : `已归档 ${archived.length}`,
        })
        : null,
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
      statusFilterChips(totals))));
  appendMaybe(frag, filterNote());
  appendMaybe(frag, orderNote(live));

  if (!all.length) {
    frag.append(emptyState('队列里还没有任何任务卡。'));
    return frag;
  }
  if (!live.length) {
    frag.append(emptyState(`${archived.length} 个项目全部已归档。点右上「已归档」查看或取消归档。`));
  } else {
    frag.append(projectRail(orderProjects(live)));
  }
  if (archived.length && state.ui.showArchived) {
    frag.append(h('div', { class: 'section-head', style: 'margin:22px 0 8px' },
      h('h2', { class: 'section-title', text: `已归档项目（${archived.length}）` }),
      h('span', {
        class: 'section-note',
        text: '归档只影响本页折叠，任务卡状态与调度完全不受影响；项目一旦有新卡会自动切回活跃。',
      })));
    frag.append(projectRail(orderProjects(archived)));
  }
  return frag;
}

/**
 * 项目横向轨道：一个项目一列，左右滚动切项目，纵向空间全部留给单个项目的任务清单。
 *
 * 【为什么改成横排】原来是自适应网格，一个项目的阶段/任务铺开就占满一屏，
 * 想看第二个项目得往下滚过几百张卡。项目之间是并列关系而不是先后关系，
 * 横排 + 列内独立纵向滚动才对得上"多项目并行"的实际用法。
 * tabindex/role 是给键盘用户的：横向滚动容器不可聚焦的话，只能靠鼠标横滚。
 */
function projectRail(projects) {
  const rail = h('div', {
    class: 'project-rail', role: 'region', tabindex: '0',
    'aria-label': `项目横向滚动区，共 ${projects.length} 个项目，用左右方向键或横向滚动切换；`
      + '拖动列头的手柄可调整项目次序',
  });
  for (const p of projects) rail.append(projectCard(p));
  wireRailDrag(rail);
  return rail;
}

/**
 * 给轨道装上拖拽排序。
 *
 * 【为什么用专门的手柄而不是整卡可拖】项目卡里全是链接、可展开的阶段与任务卡，
 * 整卡 draggable 会把"想选一段标题文字"变成"把整列拖走"，也会和 <details>
 * 的点击抢事件。手柄是一小块专职区域，代价是多一个图标，换来其余交互一点不受影响。
 *
 * 【为什么 dragover 就重排 DOM】拖到哪就地插到哪，松手前所见即所得；
 * 松手时只把最终次序落盘一次，中途不写 localStorage、不触发重新拉数据。
 */
function wireRailDrag(rail) {
  let dragged = null;

  rail.addEventListener('dragstart', (ev) => {
    const card = ev.target.closest('.proj');
    if (!card || !ev.target.closest('.drag-grip')) return;
    dragged = card;
    card.classList.add('is-dragging');
    ev.dataTransfer.effectAllowed = 'move';
    // Firefox 不设 data 就不触发 drag 事件流；内容本身用不上。
    ev.dataTransfer.setData('text/plain', card.dataset.pid || '');
  });

  rail.addEventListener('dragover', (ev) => {
    if (!dragged) return;
    ev.preventDefault();
    ev.dataTransfer.dropEffect = 'move';
    const over = ev.target.closest('.proj');
    if (!over || over === dragged) return;
    // 以被悬停卡片的水平中点为界：越过中点就插到它后面，否则插到前面。
    // 用中点而不是"总是插到前面"，否则往右拖时永远差一位、拖到末尾根本到不了。
    const box = over.getBoundingClientRect();
    const after = ev.clientX > box.left + box.width / 2;
    rail.insertBefore(dragged, after ? over.nextSibling : over);
  });

  const finish = () => {
    if (!dragged) return;
    dragged.classList.remove('is-dragging');
    dragged = null;
    commitProjectOrder([...rail.querySelectorAll('.proj')].map((c) => c.dataset.pid));
  };
  rail.addEventListener('drop', (ev) => { ev.preventDefault(); finish(); });
  // dragend 兜底：拖到轨道外面松手不会触发 drop，不收尾的话卡片会一直挂着拖拽态。
  rail.addEventListener('dragend', finish);
}

/**
 * 拖拽手柄。同时是键盘通道：拖放对键盘用户完全不可用，
 * 所以 ←/→ 直接移动本列，这不是可选的补充。
 */
function dragGrip(p) {
  const move = (dir) => {
    const rail = document.querySelector('.project-rail');
    if (!rail) return;
    const ids = [...rail.querySelectorAll('.proj')].map((c) => c.dataset.pid);
    const i = ids.indexOf(p.id);
    const j = i + dir;
    if (i < 0 || j < 0 || j >= ids.length) return;
    [ids[i], ids[j]] = [ids[j], ids[i]];
    // 落盘会触发重渲染，手柄这个 DOM 节点随之被换掉、焦点掉回 body。
    // 不记一笔的话，键盘用户每移动一格都要重新 Tab 回来——等于这条通道不可用。
    state.pendingGripFocus = p.id;
    commitProjectOrder(ids);
  };
  return h('span', {
    class: 'drag-grip', draggable: 'true', role: 'button', tabindex: '0',
    'aria-label': `拖动以调整「${p.name}」的位置，或用左右方向键移动`,
    title: '拖动调整项目次序（也可聚焦后按 ←/→ 移动）。次序只存在本浏览器，不影响调度。',
    onkeydown: (ev) => {
      if (ev.key === 'ArrowLeft') { ev.preventDefault(); move(-1); }
      else if (ev.key === 'ArrowRight') { ev.preventDefault(); move(1); }
    },
    html: '<svg viewBox="0 0 12 12" width="11" height="11" aria-hidden="true">'
      + '<circle cx="4" cy="2.5" r="1.1" fill="currentColor"/><circle cx="8" cy="2.5" r="1.1" fill="currentColor"/>'
      + '<circle cx="4" cy="6" r="1.1" fill="currentColor"/><circle cx="8" cy="6" r="1.1" fill="currentColor"/>'
      + '<circle cx="4" cy="9.5" r="1.1" fill="currentColor"/><circle cx="8" cy="9.5" r="1.1" fill="currentColor"/></svg>',
  });
}

/** 归档按钮。总览卡与项目页共用，保证两处措辞一致。 */
function archiveBtn(p) {
  return h('button', {
    class: `arch-btn${p.archived ? ' is-on' : ''}`, type: 'button',
    title: p.archived
      ? '取消归档：重新在总览常驻显示'
      : '归档：在总览折叠该项目。只改看板视图状态，不动任何任务卡、不影响调度；该项目一旦有新卡会自动切回活跃。',
    onclick: (ev) => { ev.preventDefault(); setArchived(p.id, !p.archived); },
    text: p.archived ? '取消归档' : '归档',
  });
}

function projectCard(p) {
  const head = h('div', { class: 'proj-head' },
    h('div', { class: 'proj-titlerow' },
      dragGrip(p),
      h('h2', { class: 'proj-name' },
        h('a', { href: `#/p/${encodeURIComponent(p.id)}`, text: p.name })),
      h('div', { class: 'proj-actions' },
        archiveBtn(p),
        h('a', { class: 'proj-open', href: `#/p/${encodeURIComponent(p.id)}`, text: '看板 →' }))),
    descBlock(p.desc, p.desc_source),
    h('div', { class: 'proj-metarow' },
      p.archived ? metaChip(`已归档 ${fmtTime(p.archived_at) || ''}`.trim()) : null,
      // 自动复活必须说出来，否则用户会以为自己没点上归档按钮。
      p.archive_revived
        ? metaChip('已自动恢复活跃', { title: p.archive_revived_reason || '归档后检测到新卡' })
        : null,
      p.models.slice(0, 3).map((m) => tierBadge(m.model, m.tier)),
      p.models.length > 3 ? metaChip(`+${p.models.length - 3} 个模型`) : null,
      p.dirs.length
        ? metaChip(p.dirs.length > 1 ? `${p.dirs.length} 个目录` : p.dirs[0],
          { mono: true, title: p.dirs.join('\n') })
        : null,
      relTime(p.last_activity) ? metaChip(`活动 ${relTime(p.last_activity)}`) : null),
    kindProgress(p.stats, p.progress_percent, p.kinds, { compact: true }),
    p.kind_rule_error ? callout('warning', '⚠', p.kind_rule_error) : null,
    goalBlock(p.goal),
    statusLegend(p.stats),
    etaLine(p.eta));

  const phases = h('div', { class: 'phases' });
  for (const ph of p.phases) phases.append(phaseBlock(ph));
  // data-pid 是拖拽排序读次序用的主键（querySelectorAll 之后直接取，不必回查数据）。
  return h('section', {
    class: `card proj${p.archived ? ' is-archived' : ''}`, 'data-pid': p.id,
  }, head, phases);
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
  const shown = tasks.filter((t) => !statusHidden(t.status));
  const filteredOut = tasks.length - shown.length;

  const wrap = h('div', {});
  if (shown.length) {
    const list = h('div', { class: 'task-list' });
    for (const t of shown) list.append(taskRow(t));
    wrap.append(list);
  } else {
    wrap.append(h('div', {
      class: 'kcol-empty',
      text: `本阶段 ${tasks.length} 张卡都被状态筛选隐藏了。`,
    }));
  }
  // 「后端只发了 40 条」与「我自己把某些状态藏了」是两件完全不同的事，
  // 合成一句"显示 3 / 共 40"会让人以为看板丢了数据。两条提示分开写。
  if (isNum(total) && total > tasks.length) {
    wrap.append(h('p', {
      class: 'more-hint',
      text: `显示 ${tasks.length} / 共 ${total} 张，完整清单见项目看板。`,
    }));
  }
  if (filteredOut) {
    wrap.append(h('p', {
      class: 'more-hint',
      text: `另有 ${filteredOut} 张被状态筛选隐藏（进度与计数不受影响）。`,
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
  const flash = takeFlash();
  if (flash) frag.append(flash);
  if (d.archive_state_error) {
    frag.append(callout('warning', '⚠', h('div', {},
      h('strong', { text: '归档状态未生效：' }),
      h('code', { text: d.archive_state_error }))));
  }

  frag.append(h('div', { class: 'page-head' },
    h('div', {},
      h('p', { class: 'page-sub' }, h('a', { href: '#/', text: '← 总览' })),
      h('h1', { class: 'page-title', text: p.name }),
      descBlock(p.desc, p.desc_source)),
    h('div', { class: 'head-right' },
      archiveBtn(p),
      statusFilterChips(p.stats))));
  appendMaybe(frag, filterNote());

  frag.append(h('div', { class: 'card', style: 'padding:13px 17px;margin-bottom:16px' },
    p.archive_revived
      ? callout('warning', 'ⓘ', p.archive_revived_reason || '归档后检测到新卡，已自动切回活跃')
      : null,
    kindProgress(p.stats, p.progress_percent, p.kinds),
    p.kind_rule_error ? callout('warning', '⚠', p.kind_rule_error) : null,
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

// kanban 的列**就是**状态，所以状态筛选在这里表现为"整列消失"，
// 而不是列还在、里面空着——后者会被读成"这个状态一张卡都没有"。
// 藏掉了几列必须写出来，否则整版少一列没有任何痕迹。
function kanbanView(columns) {
  const shown = columns.filter((c) => !statusHidden(c.key));
  const hidden = columns.filter((c) => statusHidden(c.key));
  const wrap = h('div', {});
  if (shown.length) {
    const k = h('div', { class: 'kanban' });
    for (const c of shown) k.append(kanbanColumn(c));
    wrap.append(k);
  } else {
    wrap.append(h('div', { class: 'kcol-empty', text: '所有列都被状态筛选隐藏了。' }));
  }
  if (hidden.length) {
    wrap.append(h('p', {
      class: 'more-hint',
      text: `已隐藏 ${hidden.map((c) => `「${c.label}」${c.total} 张`).join('、')}——`
        + '计数与进度仍按全部卡算。',
    }));
  }
  return wrap;
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
    // 工作性质与它的判定来源必须同行显示：结构信号是盘上事实、标题关键词是猜的，
    // 只写"设计"而不写靠什么判的，读者无从判断这条分类值不值得信。
    if (t.kind) {
      row('工作性质', `${KIND_ZH[t.kind] || t.kind}（判定来源：${KIND_SOURCE_ZH[t.kind_source] || t.kind_source || '未知'}）`);
    }
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
  const d = await apiGet(`/api/burn?range=${encodeURIComponent(state.ui.spendRange)}`);
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
      risky.map((x) => {
        const rem = remainPct(x);
        return `${x.account_label}／${x.window_label}（剩余 ${rem === null ? '未知' : Math.round(rem) + '%'}）`;
      }).join('、'))));
  } else if (fresh.length) {
    frag.append(callout('good', '✓', '当前没有窗口按现有速率会在重置前烧完。'));
  } else {
    frag.append(callout('warning', '!', '所有额度样本都已过期，下面的百分比不代表现状。'));
  }

  frag.append(spendSection(d.task_spend || {}));
  frag.append(tokenSection(d.token_series || {}, d.queue_spend || {}));

  const shown = state.ui.burnAll ? sources : fresh;
  const sec = h('section', { class: 'section' },
    h('div', { class: 'section-head' },
      h('h2', { class: 'section-title', text: '各账号窗口' }),
      h('span', {
        class: 'section-note',
        text: '主读数是剩余额度（源数据给的是已用 %，剩余 = 100 − 已用）；速率只在当前窗口周期内拟合',
      }),
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
  const rem = remainPct(src);
  const head = h('div', { class: 'chart-head' },
    h('div', {},
      h('h3', { class: 'chart-title' }, `${src.account_label}　`,
        h('span', { style: 'font-weight:500;color:var(--ink-mute)', text: src.window_label })),
      h('p', {
        class: 'chart-sub',
        text: `采样于 ${fmtTime(src.captured_at)}（${relTime(src.captured_at)}）・${src.series.length} 个样本点`
          + (isNum(src.used_percent) ? `・已用 ${Math.round(src.used_percent)}%` : ''),
      })),
    h('div', { class: 'chart-actions' },
      src.stale
        ? h('span', { class: 'meta-chip', title: '样本已过期，不代表当前窗口的现状', text: '⚠ 已过期' })
        : null,
      h('span', {
        class: 'meta-chip',
        style: rem === null ? null : `color:${remainColor(rem)};font-weight:640`,
        text: rem === null ? '剩余未知' : `剩余 ${Math.round(rem)}%`,
      })));

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
 * 单个额度窗口的**燃尽**曲线：纵轴是剩余额度，线往下走、触底即耗尽。
 *
 * 为什么画剩余而不是已用：这一屏的所有文字读数都改成了「剩余 8%」，
 * 曲线若还往上爬到 92%，同一张卡上就出现一个小数字配一条高线——
 * 读者会在两种方向之间反复换算，最容易读反的正是这种组合。
 * 剩余口径下"线越低越危险"与文字同向，也才对得上「燃尽」这个名字。
 *
 * 只有 exhaust_at 非 null（后端已过速率下限、地平线上限、不给过去时刻三道闸）
 * 且 verdict 不是「数据不足」时，才画那条虚线外推——否则一条预测线都不画。
 */
function burnChart(src) {
  const W = 560, H = 170, PAD = { t: 12, r: 16, b: 26, l: 34 };
  const pts = src.series
    .map((p) => ({ t: parseTime(p.t), v: remainPct(p), used: p.used_percent }))
    .filter((p) => p.t && isNum(p.v));
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

  const remNow = remainPct(src);
  const g = sv('svg', {
    viewBox: `0 0 ${W} ${H}`, role: 'img',
    'aria-label': `${src.account_label} ${src.window_label} 剩余额度曲线，当前剩余 `
      + `${remNow === null ? '未知' : Math.round(remNow) + '%'}，线触底即额度耗尽`,
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
  // 纵轴换了口径就必须写出来：同一张图上 80 从"烧了八成"变成"还剩八成"，
  // 不标注等于让老读者照旧读法读出正好相反的结论。
  g.append(sv('text', {
    x: PAD.l - 6, y: PAD.t - 4, 'text-anchor': 'end',
    style: 'font-size:8.5px;fill:var(--ink-mute)', 'aria-hidden': 'true',
  }, '剩余%'));

  const line = pts.map((p, i) => `${i ? 'L' : 'M'}${x(p.t.getTime()).toFixed(1)},${y(p.v).toFixed(1)}`).join(' ');
  g.append(sv('path', {
    d: `${line} L${x(pts[pts.length - 1].t.getTime()).toFixed(1)},${y(0)} L${x(tMin).toFixed(1)},${y(0)} Z`,
    style: 'fill:var(--s1);opacity:.13',
  }));
  g.append(sv('path', { d: line, style: 'fill:none;stroke:var(--s1);stroke-width:2;stroke-linejoin:round' }));
  for (const p of pts) {
    g.append(sv('circle', { cx: x(p.t.getTime()), cy: y(p.v), r: 2.4, style: 'fill:var(--s1)' },
      sv('title', {}, `${fmtTime(p.t.toISOString())}　剩余 ${p.v}%`
        + (isNum(p.used) ? `（已用 ${p.used}%）` : ''))));
  }

  if (exhaust) {
    // 外推线的终点是**剩余 0**，不是 100——纵轴口径翻过来了，这里跟着翻。
    const last = pts[pts.length - 1];
    g.append(sv('path', {
      d: `M${x(last.t.getTime()).toFixed(1)},${y(last.v).toFixed(1)} L${x(exhaust.getTime()).toFixed(1)},${y(0).toFixed(1)}`,
      style: 'fill:none;stroke:var(--st-critical);stroke-width:1.6;stroke-dasharray:4 3',
    }, sv('title', {}, `按当前速率预计 ${fmtTime(src.exhaust_at)} 剩余归零`)));
    g.append(sv('circle', { cx: x(exhaust.getTime()), cy: y(0), r: 3, style: 'fill:var(--st-critical)' }));
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
      h('span', { class: 'cl-line', style: 'border-top-color:var(--s1)' }), '实测剩余额度'),
    exhaust ? h('span', { class: 'cl-item' },
      h('span', { class: 'cl-line', style: 'border-top-color:var(--st-critical);border-top-style:dashed' }),
      '按当前速率外推至归零') : null,
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
  // 两列并排：剩余是主读数，已用是源数据原值。图上只能画一条，表里两个都给全，
  // 免得有人想核对 CodexBar 的原始百分比时还要自己做减法。
  det.append(
    h('div', { class: 'tbl-wrap' },
      h('table', { class: 'tbl' },
        h('thead', {}, h('tr', {},
          h('th', { text: '采样时刻' }), h('th', { text: '剩余 %' }), h('th', { text: '已用 %（源数据）' }))),
        h('tbody', {}, src.series.map((p) => {
          const rp = remainPct(p);
          return h('tr', {},
            h('td', { text: fmtTime(p.t) || p.t }),
            h('td', { text: rp === null ? '—' : String(rp) }),
            h('td', { text: isNum(p.used_percent) ? String(p.used_percent) : '—' }));
        })))),
    h('p', {
      class: 'chart-sub',
      text: `账号键 ${src.account_key}・窗口 ${src.window}（${src.window_minutes} 分钟）`,
    }));
  return det;
}

/* ---- 队列任务消耗（按时间窗口）---- */

const SPEND_RANGES = [
  ['24h', '24 小时'], ['7d', '7 天'], ['30d', '30 天'], ['all', '全部'],
];

/** 桶大小人话化：窗口一长桶就从 15 分钟涨到 12 小时，写"720 分钟"没人换算得过来。 */
function fmtBucket(min) {
  if (!isNum(min) || min <= 0) return '15 分钟';
  if (min < 60) return `${min} 分钟`;
  const hrs = min / 60;
  return hrs >= 24 ? `${hrs / 24} 天` : `${hrs} 小时`;
}

/** 金额。两位小数不省——花费是钱，抹掉分位会让 $0.04 和 $0.05 看着一样。 */
function usd(v) {
  if (!isNum(v)) return '—';
  return '$' + v.toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

/**
 * 队列任务消耗。与上面的 transcript 曲线**不是同一个源、也不是同一个口径**，
 * 所以单独成节而不是并进那张图：
 *   - 这里一行是一张卡，只含 claudego 派发的活（交互会话不在内）；
 *   - 曲线扫的是 ~/.claude/projects，里面混着人手敲的会话，且只能回看 24 小时
 *     （30 天的 transcript 有 1 GB，扫描字节闸必然截断）。
 * 两者并排放着不说清楚，就会被读成"同一个数怎么对不上"。
 */
function spendSection(sp) {
  const head = h('div', { class: 'section-head' },
    h('h2', { class: 'section-title', text: '队列任务消耗' }),
    h('div', { class: 'seg', role: 'group', 'aria-label': '统计窗口' },
      SPEND_RANGES.map(([key, label]) => h('button', {
        type: 'button',
        'aria-pressed': String(state.ui.spendRange === key),
        onclick: () => {
          if (state.ui.spendRange === key) return;
          state.ui.spendRange = key;
          load({ silent: true });
        },
        text: label,
      }))),
    h('span', {
      class: 'section-note',
      text: sp.since ? `窗口自 ${fmtTime(sp.since) || sp.since} 起（按卡跑完的时刻归档）` : '全部历史',
    }));

  const sec = h('section', { class: 'section' }, head);

  if (!sp.tasks) {
    sec.append(emptyState('这个窗口里没有任务卡。'));
    return sec;
  }

  sec.append(h('div', { class: 'tiles' },
    tile('合计花费', usd(sp.cost_usd), '',
      '订阅制下这是 API 等价成本，不是实际扣款'),
    tile('计入的卡', num(sp.priced || 0), '张',
      `窗口内共 ${num(sp.tasks || 0)} 张卡`),
    tile('无花费数据', num(sp.unpriced || 0), '张',
      'codex / 远端 codex 不回报花费，未跑或已取消的卡同样为空——未计入合计'),
    tile('合计轮数', num(sp.turns_used || 0), '轮', '只统计有花费数据的卡')));

  const models = sp.by_model || [];
  if (models.length) {
    const maxCost = Math.max(...models.map((m) => m.cost_usd || 0), 0.01);
    const rows = h('div', { class: 'spend-list' });
    for (const m of models) {
      rows.append(h('div', { class: 'spend-row' },
        h('span', { class: 'sr-model' }, tierBadge(m.model, m.tier)),
        h('span', { class: 'sr-bar', 'aria-hidden': 'true' },
          h('span', { class: 'sr-fill', style: `width:${((m.cost_usd || 0) / maxCost) * 100}%` })),
        h('span', { class: 'sr-cost', text: usd(m.cost_usd) }),
        h('span', { class: 'sr-meta', text: `${num(m.tasks || 0)} 张・${num(m.turns_used || 0)} 轮` })));
    }
    sec.append(h('section', { class: 'card chart-card', style: 'margin-top:14px' },
      h('h3', { class: 'chart-title', text: '按模型分' }),
      h('p', {
        class: 'chart-sub',
        text: '模型取每张卡**实际生效**的那个（codex 侧经 resolveCodexModel 解析，claude 侧回落类型默认）。',
      }),
      rows));
  }

  const projects = sp.by_project || [];
  if (projects.length) {
    const maxProj = Math.max(...projects.map((p) => p.cost_usd || 0), 0.01);
    const body = h('tbody', {}, projects.map((p) => h('tr', {},
      h('td', {}, h('span', { class: 'sp-title', title: p.name, text: p.name })),
      h('td', {},
        h('span', { class: 'sr-bar sr-bar-inline', 'aria-hidden': 'true' },
          h('span', { class: 'sr-fill', style: `width:${((p.cost_usd || 0) / maxProj) * 100}%` }))),
      h('td', { class: 'is-num', text: usd(p.cost_usd) }),
      // 卡数给两个：有花费的 / 窗口内全部。只给一个的话，"这条线只花了 $3"
      // 分不清是活少还是花费没记上（codex 侧不回报）。
      h('td', { text: `${num(p.priced || 0)} / ${num(p.tasks || 0)}` }),
      h('td', { class: 'is-num', text: p.turns_used ? num(p.turns_used) : '—' }),
      h('td', {}, p.top_model ? tierBadge(p.top_model, p.top_model_tier) : '—'))));
    const card = h('section', { class: 'card chart-card', style: 'margin-top:14px' },
      h('h3', { class: 'chart-title', text: `按项目分（${num(sp.projects_n || projects.length)} 个）` }),
      h('p', {
        class: 'chart-sub',
        text: '「计入 / 全部」两个卡数分开给：只看金额分不清"这条线活少"还是"花费没记上"（codex 侧不回报花费）。',
      }),
      h('div', { class: 'tbl-wrap' }, h('table', { class: 'tbl' },
        h('thead', {}, h('tr', {},
          h('th', { text: '项目' }), h('th', { text: '占比' }), h('th', { text: '花费' }),
          h('th', { text: '计入 / 全部' }), h('th', { text: '轮数' }), h('th', { text: '主力模型' }))),
        body)));
    if (sp.top_truncated) {
      card.append(h('p', {
        class: 'more-hint',
        text: `显示花费最高的 ${projects.length} 个 / 窗口内共 ${num(sp.projects_n || 0)} 个项目。`,
      }));
    }
    sec.append(card);
  }

  if (sp.basis) sec.append(h('div', { style: 'margin-top:12px' }, callout('warning', 'ⓘ', sp.basis)));
  return sec;
}

/* ---- token 曲线 ---- */

function tokenSection(ts, spend) {
  const sec = h('section', { class: 'section' },
    h('div', { class: 'section-head' },
      h('h2', { class: 'section-title', text: 'Token 用量曲线' }),
      h('span', {
        class: 'section-note',
        text: `跟随上方窗口・${fmtBucket(ts.bucket_minutes)}一桶・来自 transcript，不分账号`
          + (ts.since ? `・自 ${fmtTime(ts.since) || ts.since} 起` : ''),
      })));
  // 这一节最容易被误读成"看板只认识一个模型"。口径不是队列这件事必须写明。
  sec.append(callout('warning', 'ⓘ', h('div', {},
    h('strong', { text: '这条曲线与上面的「队列任务消耗」不是同一份账：' }),
    '它扫的是 ~/.claude/projects，里面**混着你在 Claude Code 里手敲的交互会话**，'
    + '不只是队列派发的卡；纵轴是绝对 token 吞吐（等权口径），不是花费。'
    + '窗口短时常常只看得到一两个模型，那是"这段时间只跑过这些"，不是看板漏了数据——'
    + '把窗口拉到 30 天就能看到完整的模型分布。')));
  // 扫描撞闸 = 曲线不完整。这条必须比图更显眼，否则少了半截的曲线
  // 与"那段时间没跑活"在图上长得一模一样。
  if (ts.truncated) {
    sec.append(callout('critical', '⚠', h('div', {},
      h('strong', { text: '本次扫描不完整：' }),
      `匹配到 ${num(ts.files_matched || 0)} 个 transcript 文件，撞上字节预算上限后只读了 `
      + `${num(ts.files_scanned || 0)} 个（${((ts.bytes_scanned || 0) / 1073741824).toFixed(2)} GB）。`,
      h('p', {
        class: 'callout-sub',
        text: '下面的曲线与磁贴数字只覆盖读到的那部分，缩短窗口可得到完整读数。',
      }))));
  }

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
      class: 'nodata', style: 'margin-top:12px', text: '这个窗口内没有 transcript 用量样本。',
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
    'aria-label': `所选窗口内按模型分的 token 用量堆叠面积图，峰值 ${num(vMax)}`,
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
window.addEventListener('resize', syncRailHeight);
setInterval(tickFreshness, 15000);

navigate();
scheduleAuto();
