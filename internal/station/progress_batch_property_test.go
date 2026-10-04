package station_test

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"testing"

	"example.com/paleo-prep-station/internal/contracts"
	"example.com/paleo-prep-station/internal/domain"
	"example.com/paleo-prep-station/internal/repository"
	"example.com/paleo-prep-station/internal/station"
)

type propWorld struct {
	t          *testing.T
	svc        *station.Service
	ctx        context.Context
	specimenID []string
	planID     []string
	submission int
}

// 随机混合流量下的跨聚合不变量：
//   - 进度序号连续、比例单调；
//   - 方案 completed 当且仅当最后一条进度为 100；
//   - 方案 completed 时标本 stabilized/available，未完成时标本 preparing；
//   - 失败批次不留任何痕迹；同一提交（含并发/历史重放）只生效一次。
func TestBatchPropertyInvariantsUnderMixedTraffic(t *testing.T) {
	for _, seed := range []int64{1, 42, 99, 777, 2026} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			runPropertyWorld(t, seed, 2000)
		})
	}
}

func runPropertyWorld(t *testing.T, seed int64, iterations int) {
	w := &propWorld{t: t, svc: station.New(repository.NewMemory()), ctx: context.Background()}
	const planCount = 8
	for i := 0; i < planCount; i++ {
		sid, pid := registerAndApprove(t, w.svc, w.ctx, seed, i)
		w.specimenID = append(w.specimenID, sid)
		w.planID = append(w.planID, pid)
	}

	rng := rand.New(rand.NewSource(seed))
	for iter := 0; iter < iterations; iter++ {
		// 约 6%：提交一份"只走一次"的批次（不达到 100%），随后立即并发重放，
		// 模拟未被其它写入穿插的纯网络重试；成功的提交标识重放必须幂等。
		if rng.Intn(100) < 6 {
			w.freshBatchThenReplay(rng)
		} else if rng.Intn(10) < 2 {
			w.singleProgress(rng)
		} else {
			w.newBatch(rng, 1+rng.Intn(3))
		}
		w.assertInvariants(iter)
	}
}

// freshBatchThenReplay 在一个专属方案上提交非终态批次，再用同一提交标识并发重放，
// 验证只生效一次、各次返回完全一致，且方案进度恰好增加一条。
func (w *propWorld) freshBatchThenReplay(rng *rand.Rand) {
	recordable, last := w.recordable()
	if len(recordable) == 0 {
		return
	}
	idx := recordable[0]
	before, err := w.svc.GetPlan(w.ctx, w.planID[idx])
	if err != nil {
		w.t.Fatal(err)
	}
	// 选一个严格小于 100 且不低于当前的比例，确保之后还能继续，但这里不再触碰它。
	percent := 50
	if p := last[idx]; p >= 50 {
		percent = 80
		if p >= 80 {
			return
		}
	}
	w.submission++
	req := contracts.BatchProgressRequest{
		SubmissionID: fmt.Sprintf("sub-%d", w.submission),
		Entries: []contracts.BatchProgressItem{{
			PlanID: w.planID[idx], Stage: "矩阵剥离", Percent: percent, RecordedBy: "chen",
		}},
	}
	results, errs := w.runConcurrent(req, 2+rng.Intn(3))
	for r := range errs {
		if errs[r] != nil {
			w.t.Fatalf("fresh batch replay %d: %v", r, errs[r])
		}
		if !sameEntryIDs(results[r], results[0]) {
			w.t.Fatalf("fresh batch replays diverged: %+v vs %+v", results[r], results[0])
		}
	}
	after, err := w.svc.GetPlan(w.ctx, w.planID[idx])
	if err != nil {
		w.t.Fatal(err)
	}
	if len(after.Progress) != len(before.Progress)+1 {
		w.t.Fatalf("idempotent replay added %d entries, want exactly 1", len(after.Progress)-len(before.Progress))
	}
}

func (w *propWorld) newBatch(rng *rand.Rand, replays int) {
	recordable, last := w.recordable()
	if len(recordable) == 0 {
		return
	}
	rng.Shuffle(len(recordable), func(i, j int) {
		recordable[i], recordable[j] = recordable[j], recordable[i]
	})
	k := 1 + rng.Intn(4)
	if k > len(recordable) {
		k = len(recordable)
	}
	w.submission++
	req := contracts.BatchProgressRequest{SubmissionID: fmt.Sprintf("sub-%d", w.submission)}
	for j := 0; j < k; j++ {
		idx := recordable[j]
		req.Entries = append(req.Entries, contracts.BatchProgressItem{
			PlanID: w.planID[idx], Stage: "矩阵剥离",
			Percent: nextPercent(rng, last[idx]), RecordedBy: "chen",
		})
	}
	injectBad := rng.Intn(10) < 3
	if injectBad {
		req.Entries = append(req.Entries, contracts.BatchProgressItem{
			PlanID: "plan-missing", Stage: "坏", Percent: 10, RecordedBy: "chen",
		})
	}

	results, errs := w.runConcurrent(req, replays)
	if injectBad {
		for r, err := range errs {
			if err == nil {
				w.t.Fatalf("injected-failure batch succeeded on replay %d", r)
			}
		}
		return
	}
	// 非坏批次：因与其它提交并发竞争同一方案，可能整体冲突失败；
	// 但凡有一次成功，所有重放都必须成功且返回同一组条目（幂等命中）。
	success := -1
	for r, err := range errs {
		if err == nil {
			success = r
			break
		}
	}
	if success == -1 {
		return
	}
	want := results[success]
	for r := range errs {
		if errs[r] != nil {
			w.t.Fatalf("submission %s replay %d failed while another succeeded: %v", req.SubmissionID, r, errs[r])
		}
		if !sameEntryIDs(results[r], want) {
			w.t.Fatalf("submission %s replays diverged: %+v vs %+v", req.SubmissionID, results[r], want)
		}
	}
}

func (w *propWorld) singleProgress(rng *rand.Rand) {
	recordable, last := w.recordable()
	if len(recordable) == 0 {
		return
	}
	idx := recordable[rng.Intn(len(recordable))]
	_, _ = w.svc.LogProgress(w.ctx, w.planID[idx], contracts.ProgressRequest{
		Stage: "手工清修", Percent: nextPercent(rng, last[idx]), RecordedBy: "chen",
	})
}

func (w *propWorld) runConcurrent(req contracts.BatchProgressRequest, n int) ([][]domain.ProgressEntry, []error) {
	var wg sync.WaitGroup
	results := make([][]domain.ProgressEntry, n)
	errs := make([]error, n)
	wg.Add(n)
	for r := 0; r < n; r++ {
		go func(r int) {
			defer wg.Done()
			res, err := w.svc.LogProgressBatch(w.ctx, req)
			results[r], errs[r] = res.Entries, err
		}(r)
	}
	wg.Wait()
	return results, errs
}

func (w *propWorld) recordable() ([]int, map[int]int) {
	idx := make([]int, 0)
	last := make(map[int]int)
	for i, pid := range w.planID {
		snap, err := w.svc.GetPlan(w.ctx, pid)
		if err != nil {
			w.t.Fatal(err)
		}
		if snap.Plan.Status == domain.PlanApproved || snap.Plan.Status == domain.PlanActive {
			idx = append(idx, i)
			if len(snap.Progress) > 0 {
				last[i] = snap.Progress[len(snap.Progress)-1].Percent
			}
		}
	}
	return idx, last
}

func (w *propWorld) assertInvariants(iter int) {
	for i, pid := range w.planID {
		snap, err := w.svc.GetPlan(w.ctx, pid)
		if err != nil {
			w.t.Fatal(err)
		}
		var last int
		for seq, e := range snap.Progress {
			if e.Sequence != seq+1 {
				w.t.Fatalf("iter %d plan %d: non-contiguous sequence %+v", iter, i, snap.Progress)
			}
			if e.Percent < last {
				w.t.Fatalf("iter %d plan %d: non-monotonic progress %+v", iter, i, snap.Progress)
			}
			last = e.Percent
		}
		completed := snap.Plan.Status == domain.PlanCompleted
		if completed != (last == 100) {
			w.t.Fatalf("iter %d plan %d: completed=%v but last=%d", iter, i, completed, last)
		}
		sp, err := w.svc.GetSpecimen(w.ctx, w.specimenID[i])
		if err != nil {
			w.t.Fatal(err)
		}
		switch {
		case completed && sp.Status != domain.SpecimenStabilized && sp.Status != domain.SpecimenAvailable:
			w.t.Fatalf("iter %d plan %d completed but specimen=%s", iter, i, sp.Status)
		case !completed && sp.Status != domain.SpecimenPreparing:
			w.t.Fatalf("iter %d plan %d open but specimen=%s", iter, i, sp.Status)
		}
	}
}

func sameEntryIDs(a, b []domain.ProgressEntry) bool {
	if len(a) != len(b) {
		return false
	}
	ids := func(in []domain.ProgressEntry) []string {
		out := make([]string, len(in))
		for i := range in {
			out[i] = in[i].ID
		}
		sort.Strings(out)
		return out
	}
	x, y := ids(a), ids(b)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func registerAndApprove(t *testing.T, svc *station.Service, ctx context.Context, seed int64, i int) (string, string) {
	t.Helper()
	specimen, err := svc.RegisterSpecimen(ctx, contracts.RegisterSpecimenRequest{
		CatalogNumber: fmt.Sprintf("PPS-PROP-%d-%d", seed, i), FieldLabel: "北坡", MatrixKind: "粉砂岩",
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := svc.CreatePlan(ctx, specimen.ID, contracts.CreatePlanRequest{
		Objective: "暴露关节面", Method: "手工剥离", TargetExposure: 80,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApprovePlan(ctx, plan.ID, contracts.ApprovalRequest{ApprovedBy: "lin"}); err != nil {
		t.Fatal(err)
	}
	return specimen.ID, plan.ID
}

func nextPercent(rng *rand.Rand, current int) int {
	steps := []int{0, 10, 25, 40, 60, 80, 100}
	candidates := make([]int, 0)
	for _, s := range steps {
		if s >= current {
			candidates = append(candidates, s)
		}
	}
	if len(candidates) == 0 {
		return 100
	}
	return candidates[rng.Intn(len(candidates))]
}
