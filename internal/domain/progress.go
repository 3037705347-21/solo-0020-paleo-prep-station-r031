package domain

import (
	"fmt"
	"strings"
	"time"

	"example.com/paleo-prep-station/internal/contracts"
)

const MaxProgressBatchEntries = 24

type ProgressEntry struct {
	ID         string    `json:"id"`
	PlanID     string    `json:"plan_id"`
	Sequence   int       `json:"sequence"`
	Stage      string    `json:"stage"`
	Percent    int       `json:"percent"`
	Note       string    `json:"note"`
	RecordedBy string    `json:"recorded_by"`
	CreatedAt  time.Time `json:"created_at"`
}

type ProgressBatchItem struct {
	PlanID     string
	Stage      string
	Percent    int
	Note       string
	RecordedBy string
}

type ProgressBatchSubmission struct {
	SubmissionID string
	Items        []ProgressBatchItem
}

type ProgressBatch struct {
	SubmissionID string          `json:"submission_id"`
	Entries      []ProgressEntry `json:"entries"`
}

func NewProgressBatchSubmission(
	submissionID string,
	items []ProgressBatchItem,
) (ProgressBatchSubmission, error) {
	id := strings.TrimSpace(submissionID)
	if id == "" {
		return ProgressBatchSubmission{}, contracts.Invalid(
			"missing_submission_id",
			"合并录入必须携带提交标识",
		)
	}
	if len(items) == 0 {
		return ProgressBatchSubmission{}, contracts.Invalid(
			"empty_progress_batch",
			"合并录入至少需要一条进度",
		)
	}
	if len(items) > MaxProgressBatchEntries {
		return ProgressBatchSubmission{}, contracts.Invalid(
			"progress_batch_too_large",
			"合并录入一次最多包含24条进度",
		)
	}
	seen := make(map[string]struct{}, len(items))
	normalized := make([]ProgressBatchItem, 0, len(items))
	for _, item := range items {
		planID := strings.TrimSpace(item.PlanID)
		if planID == "" {
			return ProgressBatchSubmission{}, contracts.Invalid(
				"missing_plan_id",
				"合并录入的每条进度都必须指向修复方案",
			)
		}
		if _, exists := seen[planID]; exists {
			return ProgressBatchSubmission{}, contracts.Invalid(
				"duplicate_batch_plan",
				"同一合并录入不能重复包含同一份修复方案",
			)
		}
		seen[planID] = struct{}{}
		item.PlanID = planID
		normalized = append(normalized, item)
	}
	return ProgressBatchSubmission{
		SubmissionID: id,
		Items:        normalized,
	}, nil
}

func NewProgressEntry(
	id string,
	plan PrepPlan,
	previous []ProgressEntry,
	stage string,
	percent int,
	note string,
	recordedBy string,
	now time.Time,
) (ProgressEntry, error) {
	if id == "" {
		return ProgressEntry{}, contracts.Internal("missing_generated_id", "进度标识未生成", nil)
	}
	if plan.Status != PlanApproved && plan.Status != PlanActive {
		return ProgressEntry{}, contracts.Conflict("plan_not_recordable", "当前方案状态不能追加制备进度")
	}
	if err := validatePercent(percent); err != nil {
		return ProgressEntry{}, err
	}
	stageValue, err := requiredText("阶段名称", stage, 80)
	if err != nil {
		return ProgressEntry{}, err
	}
	noteValue, err := optionalText("进度说明", note, 500)
	if err != nil {
		return ProgressEntry{}, err
	}
	recorder, err := requiredText("记录人", recordedBy, 80)
	if err != nil {
		return ProgressEntry{}, err
	}
	if len(previous) > 0 && percent < previous[len(previous)-1].Percent {
		return ProgressEntry{}, contracts.Conflict("progress_regressed", "新的进度比例不能低于上一条记录")
	}
	return ProgressEntry{
		ID:         id,
		PlanID:     plan.ID,
		Sequence:   len(previous) + 1,
		Stage:      stageValue,
		Percent:    percent,
		Note:       noteValue,
		RecordedBy: recorder,
		CreatedAt:  now.UTC(),
	}, nil
}

func (p ProgressEntry) String() string {
	return fmt.Sprintf("%s#%d(%d%%)", p.PlanID, p.Sequence, p.Percent)
}
