package main

// boardeta.go — 完成时间估算。
//
// 诚实性是这个文件的第一原则。任务卡上**没有** ETA 字段，也没有「开始执行时间」，
// 只有 created_at / updated_at 两个时间戳。因此任何「这张卡还要跑多久」的说法
// 都只能是推断，而推断必须交代依据与样本量，样本不足时必须说「数据不足」。
//
// 为什么用**吞吐节奏**而不是「单卡历时」：
//   - updated_at - created_at 里绝大部分是排队等待（卡可能在队列里躺好几天），
//     拿它当「执行耗时」会离谱地高估；
//   - 队列是 max_parallel 路并行 + 限额冷却 + 红线时段的复杂系统，没法解析建模；
//   - 但「这个项目最近平均每 μ 分钟结掉一张卡」是可直接观测、可验证的量，
//     且天然把并行度、冷却、红线全都吸收进去了。
// 于是：剩余 k 张可调度卡的总耗时 ≈ k 个完成间隔之和。
//
// 怎么把「k 个间隔之和」变成 p50/p80，取决于 k：
//   - 完成间隔是重尾右偏分布（实测某项目中位数 24 分钟、均值 66 分钟）。
//     中心极限定理只保证 k **足够大**时和才近似正态、均值才≈中位数。
//   - k 小的时候 k·μ 远大于真中位数（实测 k=1 时高 2.7 倍，32 个间隔里 78% 低于它），
//     把这个数命名成 p50 就是编造。所以小 k 直接对经验分布做自助重采样取真分位数，
//     大 k 才用 CLT 闭式解 p50≈k·μ、p80≈k·μ+0.84·√k·σ（实测 k≥30 两者相差 3% 以内）。

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// z80 是标准正态分布 80% 分位点。用于把「和的标准差」换算成 p80 上界。
const z80 = 0.8416

const (
	pacePrimaryLookbackH  = 7 * 24  // 首选样本窗口：最近 7 天
	paceFallbackLookbackH = 30 * 24 // 样本不够时放宽到 30 天（并在 basis 里注明）
	paceMinGaps           = 3       // 少于这么多间隔就直接判「数据不足」

	// bootstrapMaxK 是走自助重采样的 k 上限；超过它 CLT 已经足够准，闭式解更省。
	bootstrapMaxK = 64
	// bootstrapDraws 是重采样次数。2000 次对 p50/p80 的抽样误差在 1% 量级，
	// 而单次估算最多 64×2000 次加法，且按 k 记忆化，整份响应只算一遍。
	bootstrapDraws = 2000
)

// paceModel 是一个项目的完成节奏样本。
type paceModel struct {
	cfg *Config
	now time.Time
	// scope 是**样本的来源范围**（当前恒为「项目」）。它与 estimate 的目标范围是两回事：
	// 阶段 ETA 用的是项目级样本，basis 必须照实说「该项目」，
	// 否则一个零完成卡的阶段会声称自己有 12 张完成样本。
	scope string
	// gaps 是相邻两张卡完成时间之差（分钟）。
	gaps []float64
	// mean/sd 是 gaps 的均值与总体标准差（分钟），med 是中位数。
	mean, sd, med float64
	// n 是完成卡数量，spanH 是样本覆盖的时间跨度（小时）。
	n         int
	spanH     float64
	lookbackH int
	// qcache 按 k 记忆化分位数：一份响应里几百张卡会反复问同一个 k。
	qcache map[int][2]float64
}

// newPaceModel 从项目的历史完成卡里提取节奏样本。
func newPaceModel(cfg *Config, ts []*Task, now time.Time) *paceModel {
	p := &paceModel{cfg: cfg, now: now, scope: "项目", qcache: map[int][2]float64{}}
	build := func(lookbackH int) bool {
		var done []time.Time
		cut := now.Add(-time.Duration(lookbackH) * time.Hour)
		for _, t := range ts {
			if t.Status != statusDone {
				continue
			}
			// done 卡的 updated_at 就是它被判完成的时刻——这是盘上唯一的完成时间证据。
			at, ok := parseRFC3339(t.UpdatedAt)
			if !ok || at.Before(cut) || at.After(now.Add(time.Hour)) {
				continue
			}
			done = append(done, at)
		}
		if len(done) < paceMinGaps+1 {
			return false
		}
		sort.Slice(done, func(i, j int) bool { return done[i].Before(done[j]) })
		var gaps []float64
		for i := 1; i < len(done); i++ {
			g := done[i].Sub(done[i-1]).Minutes()
			if g >= 0 {
				gaps = append(gaps, g)
			}
		}
		if len(gaps) < paceMinGaps {
			return false
		}
		sum := 0.0
		for _, g := range gaps {
			sum += g
		}
		mean := sum / float64(len(gaps))
		varsum := 0.0
		for _, g := range gaps {
			varsum += (g - mean) * (g - mean)
		}
		p.gaps, p.mean, p.sd = gaps, mean, math.Sqrt(varsum/float64(len(gaps)))
		sorted := append([]float64(nil), gaps...)
		sort.Float64s(sorted)
		p.med = quantileSorted(sorted, 0.5)
		p.n, p.spanH = len(done), done[len(done)-1].Sub(done[0]).Hours()
		p.lookbackH = lookbackH
		return true
	}
	if !build(pacePrimaryLookbackH) {
		build(paceFallbackLookbackH)
	}
	return p
}

func (p *paceModel) ok() bool { return len(p.gaps) >= paceMinGaps && p.mean > 0 }

// quantileSorted 对已排序切片做线性插值分位数。
func quantileSorted(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := lo + 1
	if hi >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	return sorted[lo] + (pos-float64(lo))*(sorted[hi]-sorted[lo])
}

// xorshift 是一个确定性伪随机源。**必须**确定性：同一份响应里同一张卡
// 可能被渲染多次（kanban 总列 + 阶段泳道），两次给出不同的 p50 是自相矛盾。
type xorshift uint64

func (x *xorshift) next() uint64 {
	v := uint64(*x)
	v ^= v << 13
	v ^= v >> 7
	v ^= v << 17
	*x = xorshift(v)
	return v
}

// quantilesForK 给「k 个完成间隔之和」取 50/80 分位，并返回所用方法的人话名称。
//
// 小 k 走自助重采样：从经验间隔里有放回地抽 k 个求和，重复多次后直接取分位数。
// 这是对**真分位数**的估计，不依赖任何分布假设——而 k·均值在右偏分布上只是均值，
// 把它叫 p50 会系统性高估（实测 k=1 高 2.7 倍）。
// 大 k 走 CLT 闭式解：此时和已充分正态化，闭式解与重采样结果差异在 3% 以内且更省。
func (p *paceModel) quantilesForK(k int) (float64, float64, string) {
	if k <= 0 || !p.ok() {
		return 0, 0, ""
	}
	if k > bootstrapMaxK {
		kf := float64(k)
		return kf * p.mean, kf*p.mean + z80*math.Sqrt(kf)*p.sd,
			fmt.Sprintf("k=%d 已足够大，用中心极限定理闭式解 p50=k·均值、p80=k·均值+0.84·√k·标准差", k)
	}
	method := fmt.Sprintf("k=%d 较小、间隔分布右偏，故对经验分布做 %d 次自助重采样后直接取分位数，"+
		"而非用 k·均值充当 p50", k, bootstrapDraws)
	if v, ok := p.qcache[k]; ok {
		return v[0], v[1], method
	}
	sums := make([]float64, bootstrapDraws)
	rng := xorshift(uint64(k)*0x9E3779B97F4A7C15 + uint64(len(p.gaps))*1000003 + 1)
	n := uint64(len(p.gaps))
	for i := range sums {
		s := 0.0
		for j := 0; j < k; j++ {
			s += p.gaps[rng.next()%n]
		}
		sums[i] = s
	}
	sort.Float64s(sums)
	p50, p80 := quantileSorted(sums, 0.5), quantileSorted(sums, 0.8)
	if p.qcache == nil {
		p.qcache = map[int][2]float64{}
	}
	p.qcache[k] = [2]float64{p50, p80}
	return p50, p80, method
}

// confidence 按样本量与覆盖跨度分级。
func (p *paceModel) confidence() string {
	switch {
	case !p.ok():
		return "数据不足"
	case len(p.gaps) >= 20 && p.spanH >= 6:
		return "high"
	case len(p.gaps) >= 8:
		return "medium"
	default:
		return "low"
	}
}

// confidenceFor 在样本量分级之外再看**这次外推推了多远**。
// 用 35 小时的样本去预测 3 小时后是一回事，预测 40 天后是另一回事——
// 后者早已超出「最近节奏仍然成立」的有效期，不该和前者共用一个 high。
func (p *paceModel) confidenceFor(p50Minutes float64) string {
	c := p.confidence()
	if c == "数据不足" || p.spanH <= 0 {
		return c
	}
	if p50Minutes/60 > 3*p.spanH {
		switch c {
		case "high":
			return "medium"
		case "medium":
			return "low"
		}
	}
	return c
}

func (p *paceModel) lookbackLabel() string {
	if p.lookbackH >= paceFallbackLookbackH {
		return fmt.Sprintf("最近 %d 天（7 天内样本不足，已放宽）", p.lookbackH/24)
	}
	return fmt.Sprintf("最近 %d 天", p.lookbackH/24)
}

// noETA 造一个「明确说不知道」的 ETA：三个数值字段全 null。
func noETA(confidence, basis string) BoardETA {
	return BoardETA{Confidence: confidence, Basis: basis}
}

// sampleNote 交代样本的来源范围。target 是被估算的对象（项目/阶段），
// p.scope 是样本实际取自哪里。两者不一致时**必须**说出来——
// 用项目节奏去估阶段本身站得住，冒充阶段自己的样本则是虚报。
func (p *paceModel) sampleNote(target string) string {
	if target == p.scope {
		return ""
	}
	return fmt.Sprintf("（本%s未单独建节奏样本，采用%s级完成节奏推算）", target, p.scope)
}

// estimate 估算「还剩 k 张可调度卡」需要多久跑完。
// held 单独传入：挂起卡不由调度器推进，不能计入工期，但要在 basis 里说明它的存在，
// 否则用户会以为「1 分钟后全项目就干完了」而忽略 32 张卡正等着他 release。
func (p *paceModel) estimate(schedulable, held int, target string) BoardETA {
	if schedulable == 0 {
		if held > 0 {
			return noETA("数据不足", fmt.Sprintf(
				"%s无可调度任务，但有 %d 张挂起卡等待人工 release——未放行前无法排期，故不给完成时间。",
				target, held))
		}
		zero := 0.0
		return BoardETA{
			P50Minutes: &zero, P80Minutes: &zero, Confidence: "high",
			Basis: target + "已无未终态任务。",
		}
	}
	if !p.ok() {
		return noETA("数据不足", fmt.Sprintf(
			"%s尚有 %d 张待推进卡，但%s内该%s已完成卡不足 %d 张，凑不出可信的完成节奏样本，不做估算。",
			target, schedulable, p.lookbackLabel(), p.scope, paceMinGaps+1))
	}
	p50, p80, method := p.quantilesForK(schedulable)
	finish := p.now.Add(time.Duration(p50 * float64(time.Minute))).Format(time.RFC3339)
	heldNote := ""
	if held > 0 {
		heldNote = fmt.Sprintf("；另有 %d 张挂起卡未计入，需人工 release", held)
	}
	basis := fmt.Sprintf(
		"依据%s该%s完成的 %d 张卡（%d 个完成间隔，中位数 %.1f 分钟、均值 %.1f 分钟、"+
			"标准差 %.1f 分钟，样本跨度 %.1f 小时）%s。剩余 %d 张可调度卡按间隔之和估算：%s%s。"+
			"该节奏已隐含当前并行度与限额冷却的实际影响。",
		p.lookbackLabel(), p.scope, p.n, len(p.gaps), p.med, p.mean, p.sd, p.spanH,
		p.sampleNote(target), schedulable, method, heldNote)
	p50r, p80r := round1(p50), round1(p80)
	return BoardETA{
		FinishAt: &finish, P50Minutes: &p50r, P80Minutes: &p80r,
		Confidence: p.confidenceFor(p50), Basis: basis,
	}
}

// estimateTask 估算单张卡的完成时间：按它在**同项目**可调度队列里的排位算。
// 排位复用 dispatch.go 的派发优先级（resume_first → priority → type_order → FIFO），
// 保证看板给的顺序与调度器真实取卡顺序一致。
//
// siblings 必须是**整个项目**的卡。调度器根本没有阶段概念（grep dispatch/tick/runner 零命中），
// 阶段内排位没有任何调度意义：兄弟阶段的卡照样排在它前面被派发、照样吃掉墙钟时间。
// 传阶段切片进来会让同一张卡在同一个响应里出现两个不同的 finish_at。
func (p *paceModel) estimateTask(t *Task, siblings []*Task, now time.Time) BoardETA {
	switch {
	case t.terminal():
		return noETA("数据不足", "任务已终态，无需估算。")
	case t.Status == statusHeld:
		return noETA("数据不足", "任务处于挂起状态，需人工 release 后才会进入排期，故不给完成时间。")
	}
	if !p.ok() {
		return noETA("数据不足", fmt.Sprintf(
			"%s内本%s已完成卡不足 %d 张，凑不出完成节奏样本，不做估算。",
			p.lookbackLabel(), p.scope, paceMinGaps+1))
	}
	rank := p.rankOf(t, siblings, now)
	if rank <= 0 {
		return noETA("数据不足", "无法确定该任务在派发队列中的位置，不做估算。")
	}
	p50, p80, method := p.quantilesForK(rank)
	finish := now.Add(time.Duration(p50 * float64(time.Minute))).Format(time.RFC3339)
	pos := fmt.Sprintf("排在本%s可调度队列第 %d 位", p.scope, rank)
	if t.Status == statusRunning {
		pos = "正在执行（按队列第 1 位计）"
	}
	basis := fmt.Sprintf(
		"%s；本%s%s完成间隔中位数 %.1f 分钟、均值 %.1f 分钟、标准差 %.1f 分钟（%d 个间隔）。"+
			"按前面 %d 张卡的间隔之和估算：%s。单卡耗时本身无历史记录（任务卡不存执行时长），"+
			"故这是队列排位推断，不是对这张卡本身工作量的估计。",
		pos, p.scope, p.lookbackLabel(), p.med, p.mean, p.sd, len(p.gaps), rank, method)
	p50r, p80r := round1(p50), round1(p80)
	return BoardETA{
		FinishAt: &finish, P50Minutes: &p50r, P80Minutes: &p80r,
		Confidence: p.confidenceFor(p50), Basis: basis,
	}
}

// rankOf 返回 t 在同项目可调度卡中的 1-based 排位（按调度器优先级排序）。
func (p *paceModel) rankOf(t *Task, siblings []*Task, now time.Time) int {
	cfg := p.cfg
	if cfg == nil {
		cfg = defaultConfig("claude")
	}
	var cands []*Task
	for _, s := range siblings {
		switch s.Status {
		case statusQueued, statusRunning, statusLimitPaused:
			cands = append(cands, s)
		}
	}
	interrupted := func(x *Task) bool {
		return x.Status == statusLimitPaused || x.Status == statusRunning
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if cfg.ResumeFirst && interrupted(a) != interrupted(b) {
			return interrupted(a)
		}
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if ra, rb := typeRank(cfg, a.Type), typeRank(cfg, b.Type); ra != rb {
			return ra < rb
		}
		return a.CreatedAt < b.CreatedAt
	})
	for i, c := range cands {
		if c.ID == t.ID {
			return i + 1
		}
	}
	return 0
}

// ---- 自动推导的介绍文案 ----

func zhPhaseStatus(s string) string {
	switch s {
	case "active":
		return "进行中"
	case "blocked":
		return "受阻"
	case "queued":
		return "排队中"
	case "done":
		return "已完成"
	case "canceled":
		return "已取消"
	}
	return s
}

// derivedProjectDesc 生成项目介绍。任务卡没有项目描述字段，只能据统计事实叙述，
// 不编造项目意图——想要一句人话介绍请写 board.json 覆盖。
func derivedProjectDesc(p *Project, dirs []string) string {
	s := &p.Stats
	parts := fmt.Sprintf("共 %d 张任务卡（已完成 %d、待推进 %d）", s.Total, s.Done, s.activeTotal())
	if len(p.Phases) > 0 {
		parts += fmt.Sprintf("，划分 %d 个阶段", len(p.Phases))
	}
	if len(dirs) > 0 {
		parts += fmt.Sprintf("；主目录 %s", dirs[0])
		if len(dirs) > 1 {
			parts += fmt.Sprintf("（另含 %d 个车道/镜像目录）", len(dirs)-1)
		}
	}
	if len(p.Models) > 0 {
		parts += fmt.Sprintf("；主力模型 %s（%s 档）", p.Models[0].Model, p.Models[0].Tier)
	}
	if s.Held > 0 || s.Failed > 0 {
		parts += fmt.Sprintf("；当前 %d 张挂起、%d 张失败需处理", s.Held, s.Failed)
	}
	if s.Canceled > 0 {
		parts += fmt.Sprintf("；另有 %d 张已取消（不计入进度分母）", s.Canceled)
	}
	return parts + "。（自动统计，非人工撰写）"
}

// derivedPhaseDesc 生成阶段介绍。
// 「已完成 + 待推进」不是全部——取消卡两边都不占，不单列出来这句话就算不平
// （实测出现过「共 2 张卡：已完成 0、待推进 0」，两张卡在叙述里凭空消失）。
func derivedPhaseDesc(ph *Phase) string {
	s := &ph.Stats
	d := fmt.Sprintf("阶段「%s」共 %d 张卡：已完成 %d、待推进 %d，状态%s",
		ph.Name, s.Total, s.Done, s.activeTotal(), zhPhaseStatus(ph.Status))
	if s.Held > 0 {
		d += fmt.Sprintf("（%d 张挂起待放行）", s.Held)
	}
	if s.Failed > 0 {
		d += fmt.Sprintf("（%d 张失败）", s.Failed)
	}
	if s.Canceled > 0 {
		d += fmt.Sprintf("（另有 %d 张已取消，不计入进度分母）", s.Canceled)
	}
	if ph.Name == phaseUnsorted {
		d += "。这些卡的标题没有可识别的阶段记号，归到此处而非强行归类"
	}
	return d + "。（自动统计）"
}
