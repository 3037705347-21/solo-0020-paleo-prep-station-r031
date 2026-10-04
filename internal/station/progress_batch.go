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
	if existing, ok := m.GetProgressBatch(submission.SubmissionID); ok {
		result.Entries = append(result.Entries[:0], existing.Entries...)
		return nil
	}
	result.Entries = result.Entries[:0]
	for _, item := range submission.Items {
		entry, plan, specimen, err := prepareBatchProgress(s, m, item)
		if err != nil {
			return err
		}
		m.AppendProgress(entry)
		m.PutPlan(plan)
		m.PutSpecimen(specimen)
		result.Entries = append(result.Entries, entry)
	}
	m.PutProgressBatch(domain.ProgressBatch{
		SubmissionID: submission.SubmissionID,
		Entries:      append([]domain.ProgressEntry(nil), result.Entries...),
	})
	return nil
}

func prepareBatchProgress(
	s *Service,
	m *repository.Mutation,
	item domain.ProgressBatchItem,
) (domain.ProgressEntry, domain.PrepPlan, domain.Specimen, error) {
	plan, ok := m.GetPlan(item.PlanID)
	if !ok {
		return domain.ProgressEntry{}, domain.PrepPlan{}, domain.Specimen{}, notFoundPlan(item.PlanID)
	}
	specimen, ok := m.GetSpecimen(plan.SpecimenID)
	if !ok {
		return domain.ProgressEntry{}, domain.PrepPlan{}, domain.Specimen{}, notFoundSpecimen(plan.SpecimenID)
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
		return domain.ProgressEntry{}, domain.PrepPlan{}, domain.Specimen{}, err
	}
	if plan.Status == domain.PlanApproved {
		if err := plan.Begin(s.now()); err != nil {
			return domain.ProgressEntry{}, domain.PrepPlan{}, domain.Specimen{}, err
		}
	}
	if item.Percent == 100 {
		if err := plan.Complete(s.now()); err != nil {
			return domain.ProgressEntry{}, domain.PrepPlan{}, domain.Specimen{}, err
		}
		if err := specimen.MarkStabilized(s.now()); err != nil {
			return domain.ProgressEntry{}, domain.PrepPlan{}, domain.Specimen{}, err
		}
	}
	return entry, plan, specimen, nil
}
