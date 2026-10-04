package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"example.com/paleo-prep-station/internal/contracts"
	"example.com/paleo-prep-station/internal/domain"
	"example.com/paleo-prep-station/internal/httpapi"
	"example.com/paleo-prep-station/internal/repository"
	"example.com/paleo-prep-station/internal/station"
)

type batchHarness struct {
	server *httptest.Server
	client *http.Client
	serial int
}

func newBatchHarness(t *testing.T) *batchHarness {
	t.Helper()
	h := &batchHarness{client: &http.Client{}}
	h.server = httptest.NewServer(httpapi.NewRouter(station.New(repository.NewMemory())))
	t.Cleanup(h.server.Close)
	return h
}

func (h *batchHarness) do(method, path string, body any, out any) int {
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, h.server.URL+path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func (h *batchHarness) approvedPlan(t *testing.T) (string, string) {
	t.Helper()
	h.serial++
	var specimen struct {
		ID string `json:"id"`
	}
	if status := h.do(http.MethodPost, "/specimens", contracts.RegisterSpecimenRequest{
		CatalogNumber: fmt.Sprintf("PPS-HTTP-%d", h.serial),
		FieldLabel:    "北坡",
		MatrixKind:    "粉砂岩",
	}, &specimen); status != http.StatusCreated {
		t.Fatalf("register specimen status = %d", status)
	}
	var plan struct {
		ID string `json:"id"`
	}
	if status := h.do(http.MethodPost, "/specimens/"+specimen.ID+"/plans", contracts.CreatePlanRequest{
		Objective: "暴露主要关节面", Method: "手工显微剥离", TargetExposure: 80,
	}, &plan); status != http.StatusCreated {
		t.Fatalf("create plan status = %d", status)
	}
	if status := h.do(http.MethodPost, "/plans/"+plan.ID+"/approval",
		contracts.ApprovalRequest{ApprovedBy: "curator-lin"}, &plan); status != http.StatusOK {
		t.Fatalf("approve plan status = %d", status)
	}
	return specimen.ID, plan.ID
}

type planSnapshot struct {
	Plan struct {
		Status string `json:"status"`
	} `json:"plan"`
	Progress []domain.ProgressEntry `json:"progress"`
}

func (h *batchHarness) snapshot(t *testing.T, planID string) planSnapshot {
	t.Helper()
	var snap planSnapshot
	if status := h.do(http.MethodGet, "/plans/"+planID, nil, &snap); status != http.StatusOK {
		t.Fatalf("get plan status = %d", status)
	}
	return snap
}

func (h *batchHarness) specimenStatus(t *testing.T, specimenID string) string {
	t.Helper()
	var spec struct {
		Status string `json:"status"`
	}
	h.do(http.MethodGet, "/specimens/"+specimenID, nil, &spec)
	return spec.Status
}

// 合并录入中有一份方案不可接受时，HTTP 返回 4xx，且快照中不留任何进度或完成/稳定变化。
func TestHTTPRejectsPartialBatch(t *testing.T) {
	h := newBatchHarness(t)
	specimenA, planA := h.approvedPlan(t)
	_, planB := h.approvedPlan(t)

	status := h.do(http.MethodPost, "/plans/progress-batches", contracts.BatchProgressRequest{
		SubmissionID: "http-rollback",
		Entries: []contracts.BatchProgressItem{
			{PlanID: planA, Stage: "表面加固", Percent: 100, RecordedBy: "preparator-chen"},
			{PlanID: planB, Stage: "坏记录", Percent: 150, RecordedBy: "preparator-chen"},
		},
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}

	snap := h.snapshot(t, planA)
	if snap.Plan.Status != "approved" || len(snap.Progress) != 0 {
		t.Fatalf("plan A not rolled back: status=%s progress=%+v", snap.Plan.Status, snap.Progress)
	}
	if got := h.specimenStatus(t, specimenA); got != "preparing" {
		t.Fatalf("specimen A = %s, want preparing", got)
	}
}

// 同一份提交经网络重试并发到达时，每次都成功返回，但持久化结果只有一份。
func TestHTTPConcurrentReplayIsIdempotent(t *testing.T) {
	h := newBatchHarness(t)
	specimen, plan := h.approvedPlan(t)
	request := contracts.BatchProgressRequest{
		SubmissionID: "http-replay",
		Entries: []contracts.BatchProgressItem{
			{PlanID: plan, Stage: "表面加固", Percent: 100, RecordedBy: "preparator-chen"},
		},
	}

	const n = 32
	var wg sync.WaitGroup
	statuses := make([]int, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			statuses[i] = h.do(http.MethodPost, "/plans/progress-batches", request, nil)
		}(i)
	}
	wg.Wait()
	for i, status := range statuses {
		if status != http.StatusCreated {
			t.Fatalf("replay %d status = %d, want 201", i, status)
		}
	}

	snap := h.snapshot(t, plan)
	if len(snap.Progress) != 1 {
		t.Fatalf("progress entries = %d, want exactly 1: %+v", len(snap.Progress), snap.Progress)
	}
	if snap.Progress[0].Sequence != 1 || snap.Progress[0].Percent != 100 {
		t.Fatalf("single retained entry wrong: %+v", snap.Progress[0])
	}
	if snap.Plan.Status != "completed" {
		t.Fatalf("plan status = %s, want completed", snap.Plan.Status)
	}
	if got := h.specimenStatus(t, specimen); got != "stabilized" {
		t.Fatalf("specimen = %s, want stabilized", got)
	}
}

// 合并录入稳定后，正常的研究放行链路结果保持不变。
func TestHTTPBatchCompletionStillReleases(t *testing.T) {
	h := newBatchHarness(t)
	specimen, plan := h.approvedPlan(t)

	if status := h.do(http.MethodPost, "/plans/progress-batches", contracts.BatchProgressRequest{
		SubmissionID: "http-complete",
		Entries: []contracts.BatchProgressItem{
			{PlanID: plan, Stage: "表面加固", Percent: 100, RecordedBy: "preparator-chen"},
		},
	}, nil); status != http.StatusCreated {
		t.Fatalf("complete via batch status = %d", status)
	}
	if status := h.do(http.MethodPost, "/specimens/"+specimen+"/release", contracts.ReleaseRequest{
		Purpose: "比较解剖研究", Recipient: "地层研究组", ApprovedBy: "keeper-zhou",
	}, nil); status != http.StatusCreated {
		t.Fatalf("release status = %d, want 201", status)
	}
	if got := h.specimenStatus(t, specimen); got != "available" {
		t.Fatalf("specimen = %s, want available", got)
	}
}
