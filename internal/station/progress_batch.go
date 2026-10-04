package station

import (
	"example.com/paleo-prep-station/internal/contracts"
	"example.com/paleo-prep-station/internal/domain"
	"example.com/paleo-prep-station/internal/repository"
)

// ProgressBatchResult is the ordered outcome of one merged progress submission.
type ProgressBatchResult struct {
	SubmissionID string                 `json:"submission_id"`
	Entries      []domain.ProgressEntry `json:"entries"`
}

// preparedBatchItem holds every change produced by one entry of a merged
// submission. The values are derived without touching the transaction so a
// later rejected entry can abort the whole batch cleanly.
type preparedBatchItem struct {
	entry    domain.ProgressEntry
	plan     domain.PrepPlan
	specimen domain.Specimen
}

func batchItems(request contracts.BatchProgressRequest) []domain.ProgressBatchItem {
	items := make([]domain.ProgressBatchItem, 0, len(request.Entries))
	for _, entry := range request.Entries {
		items = append(items, domain.ProgressBatchItem{
			PlanID:     entry.PlanID,
			Stage:      entry.Stage,
			Percent:    entry.Percent,
			Note:       entry.Note,
			RecordedBy: entry.RecordedBy,
		})
	}
	return items
}

func applyProgressBatch(
	s *Service,
	m *repository.Mutation,
	submission domain.ProgressBatchSubmission,
	result *ProgressBatchResult,
) error {
	// A repeated delivery of an already accepted submission replays the
	// original outcome and never applies the payload a second time.
	if existing, ok := m.GetProgressBatch(submission.SubmissionID); ok {
		result.Entries = append(result.Entries[:0], existing.Entries...)
		return nil
	}

	// Phase 1: prepare every entry in working memory. This performs all
	// validation and state-machine transitions without a single transaction
	// write, so one unacceptable entry aborts the whole batch and leaves no
	// progress, completion or stabilization behind.
	prepared := make([]preparedBatchItem, 0, len(submission.Items))
	for _, item := range submission.Items {
		preparedItem, err := prepareBatchProgress(s, m, item)
		if err != nil {
			return err
		}
		prepared = append(prepared, preparedItem)
	}

	// Phase 2: every entry was accepted, so commit the entire batch together.
	entries := make([]domain.ProgressEntry, 0, len(prepared))
	for _, item := range prepared {
		m.AppendProgress(item.entry)
		m.PutPlan(item.plan)
		m.PutSpecimen(item.specimen)
		entries = append(entries, item.entry)
	}
	m.PutProgressBatch(domain.ProgressBatch{
		SubmissionID: submission.SubmissionID,
		Entries:      entries,
	})
	result.Entries = append(result.Entries[:0], entries...)
	return nil
}

func prepareBatchProgress(
	s *Service,
	m *repository.Mutation,
	item domain.ProgressBatchItem,
) (preparedBatchItem, error) {
	plan, ok := m.GetPlan(item.PlanID)
	if !ok {
		return preparedBatchItem{}, notFoundPlan(item.PlanID)
	}
	specimen, ok := m.GetSpecimen(plan.SpecimenID)
	if !ok {
		return preparedBatchItem{}, notFoundSpecimen(plan.SpecimenID)
	}
	previous := m.ProgressForPlan(item.PlanID)
	entry, err := domain.NewProgressEntry(
		s.nextID("progress"),
		plan,
		previous,
		item.Stage,
		item.Percent,
		item.Note,
		item.RecordedBy,
		s.now(),
	)
	if err != nil {
		return preparedBatchItem{}, err
	}
	if plan.Status == domain.PlanApproved {
		if err := plan.Begin(s.now()); err != nil {
			return preparedBatchItem{}, err
		}
	}
	if item.Percent == 100 {
		if err := plan.Complete(s.now()); err != nil {
			return preparedBatchItem{}, err
		}
		if err := specimen.MarkStabilized(s.now()); err != nil {
			return preparedBatchItem{}, err
		}
	}
	return preparedBatchItem{entry: entry, plan: plan, specimen: specimen}, nil
}
