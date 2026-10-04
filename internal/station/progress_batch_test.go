package station

import (
	"context"
	"testing"

	"example.com/paleo-prep-station/internal/contracts"
	"example.com/paleo-prep-station/internal/domain"
	"example.com/paleo-prep-station/internal/repository"
)

type batchFixture struct {
	service *Service
	ctx     context.Context
	t       *testing.T
}

func newBatchFixture(t *testing.T) batchFixture {
	t.Helper()
	return batchFixture{service: New(repository.NewMemory()), ctx: context.Background(), t: t}
}

func (f batchFixture) specimen(n int) domain.Specimen {
	s, err := f.service.RegisterSpecimen(f.ctx, contracts.RegisterSpecimenRequest{
		CatalogNumber: "BATCH-" + string(rune('A'+n)) + "-01",
		FieldLabel:    "field label",
		MatrixKind:    "mudstone",
	})
	if err != nil {
		f.t.Fatalf("register specimen: %v", err)
	}
	return s
}

func (f batchFixture) approvedPlan(t *testing.T, specimenID string) domain.PrepPlan {
	t.Helper()
	plan, err := f.service.CreatePlan(f.ctx, specimenID, contracts.CreatePlanRequest{
		Objective:      "objective text",
		Method:         "method text",
		TargetExposure: 80,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	plan, err = f.service.ApprovePlan(f.ctx, plan.ID, contracts.ApprovalRequest{ApprovedBy: "curator-lin"})
	if err != nil {
		t.Fatalf("approve plan: %v", err)
	}
	return plan
}

func batchItem(planID string, percent int) contracts.BatchProgressItem {
	return contracts.BatchProgressItem{
		PlanID:     planID,
		Stage:      "stage name",
		Percent:    percent,
		Note:       "",
		RecordedBy: "preparator-chen",
	}
}

// assertPlanProgress checks the recorded entries for a plan.
func assertPlanProgress(t *testing.T, f batchFixture, planID string, wantPercents ...int) {
	t.Helper()
	snap, err := f.service.GetPlan(f.ctx, planID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Progress) != len(wantPercents) {
		t.Fatalf("plan %s progress = %v, want %v", planID, percents(snap.Progress), wantPercents)
	}
	for i, want := range wantPercents {
		if snap.Progress[i].Percent != want {
			t.Fatalf("plan %s progress[%d] = %d, want %d (full: %v)",
				planID, i, snap.Progress[i].Percent, want, percents(snap.Progress))
		}
		if snap.Progress[i].Sequence != i+1 {
			t.Fatalf("plan %s progress[%d] sequence = %d, want %d",
				planID, i, snap.Progress[i].Sequence, i+1)
		}
	}
}

func percents(entries []domain.ProgressEntry) []int {
	out := make([]int, len(entries))
	for i, e := range entries {
		out[i] = e.Percent
	}
	return out
}

func TestBatchAppliesEveryEntryTogether(t *testing.T) {
	f := newBatchFixture(t)
	s1 := f.specimen(0)
	s2 := f.specimen(1)
	p1 := f.approvedPlan(t, s1.ID)
	p2 := f.approvedPlan(t, s2.ID)

	res, err := f.service.LogProgressBatch(f.ctx, contracts.BatchProgressRequest{
		SubmissionID: "SUB-OK",
		Entries:      []contracts.BatchProgressItem{batchItem(p1.ID, 30), batchItem(p2.ID, 60)},
	})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Entries) != 2 {
		t.Fatalf("result entries = %d, want 2", len(res.Entries))
	}
	assertPlanProgress(t, f, p1.ID, 30)
	assertPlanProgress(t, f, p2.ID, 60)
	snap1, _ := f.service.GetPlan(f.ctx, p1.ID)
	snap2, _ := f.service.GetPlan(f.ctx, p2.ID)
	if snap1.Plan.Status != domain.PlanActive || snap2.Plan.Status != domain.PlanActive {
		t.Fatalf("statuses = %s/%s, want active/active", snap1.Plan.Status, snap2.Plan.Status)
	}
}

func TestBatchRollsBackWhenAnyEntryRejected(t *testing.T) {
	f := newBatchFixture(t)
	s1 := f.specimen(0)
	s2 := f.specimen(1)
	p1 := f.approvedPlan(t, s1.ID)
	p2 := f.approvedPlan(t, s2.ID)

	// Second entry invalid percent; first entry must not survive.
	_, err := f.service.LogProgressBatch(f.ctx, contracts.BatchProgressRequest{
		SubmissionID: "SUB-BAD-PERCENT",
		Entries:      []contracts.BatchProgressItem{batchItem(p1.ID, 30), batchItem(p2.ID, 150)},
	})
	if err == nil {
		t.Fatal("expected rejection")
	}
	assertPlanProgress(t, f, p1.ID)
	assertPlanProgress(t, f, p2.ID)
	snap1, _ := f.service.GetPlan(f.ctx, p1.ID)
	if snap1.Plan.Status != domain.PlanApproved {
		t.Fatalf("p1 status = %s, want approved", snap1.Plan.Status)
	}
	spec1, _ := f.service.GetSpecimen(f.ctx, s1.ID)
	if spec1.Status != domain.SpecimenPreparing {
		t.Fatalf("s1 status = %s, want preparing", spec1.Status)
	}

	// Rejected entry in first position rolls back the later entry too.
	_, err = f.service.LogProgressBatch(f.ctx, contracts.BatchProgressRequest{
		SubmissionID: "SUB-BAD-FIRST",
		Entries:      []contracts.BatchProgressItem{batchItem(p2.ID, 250), batchItem(p1.ID, 40)},
	})
	if err == nil {
		t.Fatal("expected rejection")
	}
	assertPlanProgress(t, f, p1.ID)
	assertPlanProgress(t, f, p2.ID)
}

func TestBatchRollsBackCompletionAndStabilization(t *testing.T) {
	f := newBatchFixture(t)
	s1 := f.specimen(0)
	s2 := f.specimen(1)
	p1 := f.approvedPlan(t, s1.ID)
	p2 := f.approvedPlan(t, s2.ID)

	// First entry completes p1 and stabilizes s1; second entry regresses
	// percentage for p2 (it has no prior record, use an already completed plan
	// variant instead): complete p2 first, then a batch that completes p1 and
	// illegally appends to completed p2.
	if _, err := f.service.LogProgress(f.ctx, p2.ID, contracts.ProgressRequest{
		Stage: "finish", Percent: 100, RecordedBy: "preparator-chen",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := f.service.LogProgressBatch(f.ctx, contracts.BatchProgressRequest{
		SubmissionID: "SUB-TERMINAL-ROLLBACK",
		Entries:      []contracts.BatchProgressItem{batchItem(p1.ID, 100), batchItem(p2.ID, 100)},
	})
	if err == nil {
		t.Fatal("expected rejection of entry against completed plan")
	}
	assertPlanProgress(t, f, p1.ID)
	snap1, _ := f.service.GetPlan(f.ctx, p1.ID)
	if snap1.Plan.Status != domain.PlanApproved {
		t.Fatalf("p1 status = %s, want approved after rollback", snap1.Plan.Status)
	}
	spec1, _ := f.service.GetSpecimen(f.ctx, s1.ID)
	if spec1.Status != domain.SpecimenPreparing {
		t.Fatalf("s1 status = %s, want preparing after rollback", spec1.Status)
	}
	// p2 and s2 stay exactly as they were (completed / stabilized, one entry).
	assertPlanProgress(t, f, p2.ID, 100)
	spec2, _ := f.service.GetSpecimen(f.ctx, s2.ID)
	if spec2.Status != domain.SpecimenStabilized {
		t.Fatalf("s2 status = %s, want stabilized", spec2.Status)
	}
}

func TestBatchIdempotentReplay(t *testing.T) {
	f := newBatchFixture(t)
	s1 := f.specimen(0)
	p1 := f.approvedPlan(t, s1.ID)

	req := contracts.BatchProgressRequest{
		SubmissionID: "SUB-IDEM",
		Entries:      []contracts.BatchProgressItem{batchItem(p1.ID, 40)},
	}
	first, err := f.service.LogProgressBatch(f.ctx, req)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Immediate network retry.
	replay, err := f.service.LogProgressBatch(f.ctx, req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(replay.Entries) != 1 || replay.Entries[0].ID != first.Entries[0].ID {
		t.Fatalf("replay = %+v, want original entry %+v", replay.Entries, first.Entries)
	}
	assertPlanProgress(t, f, p1.ID, 40)

	// Retry after later single-entry progress advances the plan must still
	// return the original outcome without renumbering or duplication.
	if _, err := f.service.LogProgress(f.ctx, p1.ID, contracts.ProgressRequest{
		Stage: "later stage", Percent: 80, RecordedBy: "preparator-chen",
	}); err != nil {
		t.Fatalf("single progress: %v", err)
	}
	replay2, err := f.service.LogProgressBatch(f.ctx, req)
	if err != nil {
		t.Fatalf("late replay: %v", err)
	}
	if len(replay2.Entries) != 1 || replay2.Entries[0].Sequence != 1 || replay2.Entries[0].Percent != 40 {
		t.Fatalf("late replay = %+v", replay2.Entries)
	}
	assertPlanProgress(t, f, p1.ID, 40, 80)
}

func TestBatchCompletionFollowedByReplayStaysTerminal(t *testing.T) {
	f := newBatchFixture(t)
	s1 := f.specimen(0)
	p1 := f.approvedPlan(t, s1.ID)

	req := contracts.BatchProgressRequest{
		SubmissionID: "SUB-DONE",
		Entries:      []contracts.BatchProgressItem{batchItem(p1.ID, 100)},
	}
	if _, err := f.service.LogProgressBatch(f.ctx, req); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := f.service.LogProgressBatch(f.ctx, req); err != nil {
		t.Fatalf("replay completed: %v", err)
	}
	assertPlanProgress(t, f, p1.ID, 100)
	snap, _ := f.service.GetPlan(f.ctx, p1.ID)
	if snap.Plan.Status != domain.PlanCompleted {
		t.Fatalf("status = %s, want completed", snap.Plan.Status)
	}
	spec1, _ := f.service.GetSpecimen(f.ctx, s1.ID)
	if spec1.Status != domain.SpecimenStabilized {
		t.Fatalf("specimen = %s, want stabilized", spec1.Status)
	}
}

func TestFailedBatchCanBeResumedBySingleEntries(t *testing.T) {
	f := newBatchFixture(t)
	s1 := f.specimen(0)
	s2 := f.specimen(1)
	p1 := f.approvedPlan(t, s1.ID)
	p2 := f.approvedPlan(t, s2.ID)

	_, err := f.service.LogProgressBatch(f.ctx, contracts.BatchProgressRequest{
		SubmissionID: "SUB-FAILED",
		Entries:      []contracts.BatchProgressItem{batchItem(p1.ID, 50), batchItem(p2.ID, 999)},
	})
	if err == nil {
		t.Fatal("expected failure")
	}
	// Continue both plans through the normal single-entry path; sequences and
	// state transitions must line up as if the batch never happened.
	if _, err := f.service.LogProgress(f.ctx, p1.ID, contracts.ProgressRequest{
		Stage: "single stage", Percent: 100, RecordedBy: "preparator-chen",
	}); err != nil {
		t.Fatalf("complete p1: %v", err)
	}
	if _, err := f.service.LogProgress(f.ctx, p2.ID, contracts.ProgressRequest{
		Stage: "single stage", Percent: 70, RecordedBy: "preparator-chen",
	}); err != nil {
		t.Fatalf("progress p2: %v", err)
	}
	assertPlanProgress(t, f, p1.ID, 100)
	assertPlanProgress(t, f, p2.ID, 70)
	spec1, _ := f.service.GetSpecimen(f.ctx, s1.ID)
	if spec1.Status != domain.SpecimenStabilized {
		t.Fatalf("s1 = %s", spec1.Status)
	}
	spec2, _ := f.service.GetSpecimen(f.ctx, s2.ID)
	if spec2.Status != domain.SpecimenPreparing {
		t.Fatalf("s2 = %s", spec2.Status)
	}
}

func TestBatchRejectsUnknownPlanWithoutSideEffects(t *testing.T) {
	f := newBatchFixture(t)
	s1 := f.specimen(0)
	p1 := f.approvedPlan(t, s1.ID)

	_, err := f.service.LogProgressBatch(f.ctx, contracts.BatchProgressRequest{
		SubmissionID: "SUB-MISSING",
		Entries:      []contracts.BatchProgressItem{batchItem(p1.ID, 20), batchItem("plan-missing", 30)},
	})
	if err == nil {
		t.Fatal("expected not found error")
	}
	assertPlanProgress(t, f, p1.ID)
}

func TestBatchValidationDoesNotOpenTransactionWrites(t *testing.T) {
	// A missing submission id or empty entries is rejected before mutation.
	f := newBatchFixture(t)
	if _, err := f.service.LogProgressBatch(f.ctx, contracts.BatchProgressRequest{
		Entries: []contracts.BatchProgressItem{batchItem("p", 10)},
	}); err == nil {
		t.Fatal("expected missing submission id error")
	}
	if _, err := f.service.LogProgressBatch(f.ctx, contracts.BatchProgressRequest{
		SubmissionID: "SUB-EMPTY",
	}); err == nil {
		t.Fatal("expected empty batch error")
	}
}
