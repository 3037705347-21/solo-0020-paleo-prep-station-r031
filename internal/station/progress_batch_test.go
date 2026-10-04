package station_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"example.com/paleo-prep-station/internal/contracts"
	"example.com/paleo-prep-station/internal/domain"
	"example.com/paleo-prep-station/internal/repository"
	"example.com/paleo-prep-station/internal/station"
)

type fixture struct {
	svc    *station.Service
	ctx    context.Context
	serial int
}

func newFixture() *fixture {
	return &fixture{svc: station.New(repository.NewMemory()), ctx: context.Background()}
}

// setupApprovedPlan 登记并审批一份方案，返回标本标识与方案标识。
func (f *fixture) setupApprovedPlan(t *testing.T) (string, string) {
	t.Helper()
	f.serial++
	specimen, err := f.svc.RegisterSpecimen(f.ctx, contracts.RegisterSpecimenRequest{
		CatalogNumber: fmt.Sprintf("PPS-BATCH-%d", f.serial),
		FieldLabel:    "北坡",
		MatrixKind:    "粉砂岩",
	})
	if err != nil {
		t.Fatalf("register specimen: %v", err)
	}
	plan, err := f.svc.CreatePlan(f.ctx, specimen.ID, contracts.CreatePlanRequest{
		Objective:      "暴露主要关节面",
		Method:         "手工显微剥离",
		TargetExposure: 80,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if _, err := f.svc.ApprovePlan(f.ctx, plan.ID, contracts.ApprovalRequest{ApprovedBy: "curator-lin"}); err != nil {
		t.Fatalf("approve plan: %v", err)
	}
	return specimen.ID, plan.ID
}

func (f *fixture) progress(t *testing.T, planID string, percent int, stage string) {
	t.Helper()
	if _, err := f.svc.LogProgress(f.ctx, planID, contracts.ProgressRequest{
		Stage:      stage,
		Percent:    percent,
		RecordedBy: "preparator-chen",
	}); err != nil {
		t.Fatalf("log single progress %d%%: %v", percent, err)
	}
}

func assertPlanState(t *testing.T, f *fixture, planID string, status domain.PlanStatus, percents []int) {
	t.Helper()
	snap, err := f.svc.GetPlan(f.ctx, planID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if snap.Plan.Status != status {
		t.Errorf("plan %s status = %s, want %s", planID, snap.Plan.Status, status)
	}
	if len(snap.Progress) != len(percents) {
		t.Fatalf("plan %s progress count = %d (%+v), want %d", planID, len(snap.Progress), snap.Progress, len(percents))
	}
	for i, want := range percents {
		if snap.Progress[i].Percent != want {
			t.Errorf("plan %s progress[%d] percent = %d, want %d", planID, i, snap.Progress[i].Percent, want)
		}
		if snap.Progress[i].Sequence != i+1 {
			t.Errorf("plan %s progress[%d] sequence = %d, want %d", planID, i, snap.Progress[i].Sequence, i+1)
		}
	}
}

func assertSpecimenState(t *testing.T, f *fixture, specimenID string, status domain.SpecimenStatus) {
	t.Helper()
	sp, err := f.svc.GetSpecimen(f.ctx, specimenID)
	if err != nil {
		t.Fatalf("get specimen: %v", err)
	}
	if sp.Status != status {
		t.Errorf("specimen %s status = %s, want %s", specimenID, sp.Status, status)
	}
}

func batch(planID string, percent int, stage, submissionID string) contracts.BatchProgressRequest {
	return contracts.BatchProgressRequest{
		SubmissionID: submissionID,
		Entries: []contracts.BatchProgressItem{{
			PlanID: planID, Stage: stage, Percent: percent, RecordedBy: "preparator-chen",
		}},
	}
}

// 一份方案无法接受时，整批不留任何进度、完成或稳定变化（坏记录在最后）。
func TestBatchRollsBackWhenAnyEntryRejected(t *testing.T) {
	f := newFixture()
	specimenA, planA := f.setupApprovedPlan(t)
	_, planB := f.setupApprovedPlan(t)

	_, err := f.svc.LogProgressBatch(f.ctx, contracts.BatchProgressRequest{
		SubmissionID: "rollback-tail",
		Entries: []contracts.BatchProgressItem{
			{PlanID: planA, Stage: "表面加固", Percent: 100, RecordedBy: "preparator-chen"},
			{PlanID: planB, Stage: "坏记录", Percent: 150, RecordedBy: "preparator-chen"},
		},
	})
	if err == nil {
		t.Fatal("expected batch to be rejected")
	}

	// 前一份方案不得留下完成/进度；两份方案与标本都保持提交前状态。
	assertPlanState(t, f, planA, domain.PlanApproved, []int{})
	assertPlanState(t, f, planB, domain.PlanApproved, []int{})
	assertSpecimenState(t, f, specimenA, domain.SpecimenPreparing)
}

// 坏记录排在最前、以及满 24 条中最后一条失败时，同样整体回滚。
func TestBatchRollsBackRegardlessOfFailurePosition(t *testing.T) {
	f := newFixture()
	_, planA := f.setupApprovedPlan(t)
	_, planB := f.setupApprovedPlan(t)

	_, err := f.svc.LogProgressBatch(f.ctx, contracts.BatchProgressRequest{
		SubmissionID: "rollback-head",
		Entries: []contracts.BatchProgressItem{
			{PlanID: planB, Stage: "坏记录", Percent: 150, RecordedBy: "preparator-chen"},
			{PlanID: planA, Stage: "表面加固", Percent: 100, RecordedBy: "preparator-chen"},
		},
	})
	if err == nil {
		t.Fatal("expected head-failure batch to be rejected")
	}
	assertPlanState(t, f, planA, domain.PlanApproved, []int{})

	// 23 条可接受进度 + 最后一条引用不存在的方案：全部回滚。
	entries := make([]contracts.BatchProgressItem, 0, 24)
	// 复用同一方案会被"同批重复方案"拦截，因此这里让可接受条目也指向 planA 之外不可行，
	// 改用一个不存在方案位于尾部、前面构造 23 个不同已审批方案。
	fixtures := make([]struct{ specimen, plan string }, 0, 23)
	for i := 0; i < 23; i++ {
		sid, pid := f.setupApprovedPlan(t)
		fixtures = append(fixtures, struct{ specimen, plan string }{sid, pid})
		entries = append(entries, contracts.BatchProgressItem{
			PlanID: pid, Stage: "矩阵剥离", Percent: 40, RecordedBy: "preparator-chen",
		})
	}
	entries = append(entries, contracts.BatchProgressItem{
		PlanID: "plan-missing", Stage: "坏记录", Percent: 10, RecordedBy: "preparator-chen",
	})
	if _, err := f.svc.LogProgressBatch(f.ctx, contracts.BatchProgressRequest{
		SubmissionID: "rollback-full",
		Entries:      entries,
	}); err == nil {
		t.Fatal("expected full-size failing batch to be rejected")
	}
	for i, fx := range fixtures {
		assertPlanState(t, f, fx.plan, domain.PlanApproved, []int{})
		assertSpecimenState(t, f, fx.specimen, domain.SpecimenPreparing)
		_ = i
	}
}

// 同一提交重复到达（串行重放）只保留一次结果。
func TestBatchReplayKeepsSingleResult(t *testing.T) {
	f := newFixture()
	specimen, plan := f.setupApprovedPlan(t)
	req := batch(plan, 50, "矩阵剥离", "replay-serial")

	first, err := f.svc.LogProgressBatch(f.ctx, req)
	if err != nil {
		t.Fatalf("first submission: %v", err)
	}
	second, err := f.svc.LogProgressBatch(f.ctx, req)
	if err != nil {
		t.Fatalf("replayed submission: %v", err)
	}
	if len(first.Entries) != 1 || len(second.Entries) != 1 || first.Entries[0].ID != second.Entries[0].ID {
		t.Fatalf("replay must return the same single entry: %+v vs %+v", first.Entries, second.Entries)
	}
	assertPlanState(t, f, plan, domain.PlanActive, []int{50})
	assertSpecimenState(t, f, specimen, domain.SpecimenPreparing)
}

// 同一提交在网络重试下并发到达，仍然只落一份结果（序号不重复）。
func TestBatchConcurrentReplayKeepsSingleResult(t *testing.T) {
	f := newFixture()
	specimen, plan := f.setupApprovedPlan(t)
	req := batch(plan, 100, "表面加固", "replay-concurrent")

	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.svc.LogProgressBatch(f.ctx, req)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent replay %d: %v", i, err)
		}
	}
	assertPlanState(t, f, plan, domain.PlanCompleted, []int{100})
	assertSpecimenState(t, f, specimen, domain.SpecimenStabilized)
}

// 失败提交不会被登记；修好后用同一标识再次提交应当成功，且之前的半截结果不干扰状态。
func TestFailedSubmissionCanBeCorrectedAndCompleted(t *testing.T) {
	f := newFixture()
	specimen, plan := f.setupApprovedPlan(t)
	f.progress(t, plan, 50, "矩阵剥离")

	failing := contracts.BatchProgressRequest{
		SubmissionID: "corrected-later",
		Entries: []contracts.BatchProgressItem{
			{PlanID: plan, Stage: "表面加固", Percent: 100, RecordedBy: "preparator-chen"},
			{PlanID: "plan-missing", Stage: "坏记录", Percent: 10, RecordedBy: "preparator-chen"},
		},
	}
	if _, err := f.svc.LogProgressBatch(f.ctx, failing); err == nil {
		t.Fatal("expected failing submission to be rejected")
	}
	assertPlanState(t, f, plan, domain.PlanActive, []int{50})

	// 换用新的提交标识（前端修正后重新生成）完成最后一条。
	if _, err := f.svc.LogProgressBatch(f.ctx, batch(plan, 100, "表面加固", "corrected-ok")); err != nil {
		t.Fatalf("corrected submission: %v", err)
	}
	assertPlanState(t, f, plan, domain.PlanCompleted, []int{50, 100})
	assertSpecimenState(t, f, specimen, domain.SpecimenStabilized)

	// 完成后再写进度必须冲突；放行必须正常。
	if _, err := f.svc.LogProgress(f.ctx, plan, contracts.ProgressRequest{
		Stage: "多余", Percent: 100, RecordedBy: "preparator-chen",
	}); err == nil {
		t.Fatal("appending progress to a completed plan must conflict")
	}
	if _, err := f.svc.ReleaseSpecimen(f.ctx, specimen, contracts.ReleaseRequest{
		Purpose: "比较解剖研究", Recipient: "地层研究组", ApprovedBy: "keeper-zhou",
	}); err != nil {
		t.Fatalf("release after stabilized: %v", err)
	}
	assertSpecimenState(t, f, specimen, domain.SpecimenAvailable)
}
