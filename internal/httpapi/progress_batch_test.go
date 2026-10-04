package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"example.com/paleo-prep-station/internal/contracts"
	"example.com/paleo-prep-station/internal/repository"
	"example.com/paleo-prep-station/internal/station"
)

type batchAPIFixture struct {
	server *httptest.Server
}

func newBatchAPI(t *testing.T) batchAPIFixture {
	t.Helper()
	service := station.New(repository.NewMemory())
	return batchAPIFixture{server: httptest.NewServer(NewRouter(service))}
}

func (f batchAPIFixture) close() { f.server.Close() }

func (f batchAPIFixture) do(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, f.server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

func (f batchAPIFixture) approvedPlan(t *testing.T, catalog string) (specimenID, planID string) {
	t.Helper()
	code, raw := f.do(t, "POST", "/specimens", contracts.RegisterSpecimenRequest{
		CatalogNumber: catalog, FieldLabel: "field label", MatrixKind: "mudstone",
	})
	if code != http.StatusCreated {
		t.Fatalf("register: %d %s", code, raw)
	}
	var specimen struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &specimen); err != nil {
		t.Fatal(err)
	}
	code, raw = f.do(t, "POST", "/specimens/"+specimen.ID+"/plans", contracts.CreatePlanRequest{
		Objective: "objective text", Method: "method text", TargetExposure: 80,
	})
	if code != http.StatusCreated {
		t.Fatalf("create plan: %d %s", code, raw)
	}
	var plan struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	code, raw = f.do(t, "POST", "/plans/"+plan.ID+"/approval", contracts.ApprovalRequest{ApprovedBy: "curator-lin"})
	if code != http.StatusOK {
		t.Fatalf("approve: %d %s", code, raw)
	}
	return specimen.ID, plan.ID
}

type planSnapshotView struct {
	Plan struct {
		Status string `json:"status"`
	} `json:"plan"`
	Progress []struct {
		ID       string `json:"id"`
		Sequence int    `json:"sequence"`
		Percent  int    `json:"percent"`
	} `json:"progress"`
}

func (f batchAPIFixture) snapshot(t *testing.T, planID string) planSnapshotView {
	t.Helper()
	code, raw := f.do(t, "GET", "/plans/"+planID, nil)
	if code != http.StatusOK {
		t.Fatalf("snapshot: %d %s", code, raw)
	}
	var view planSnapshotView
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	return view
}

func batchPayload(id, planID string, percent int) contracts.BatchProgressRequest {
	return contracts.BatchProgressRequest{
		SubmissionID: id,
		Entries: []contracts.BatchProgressItem{{
			PlanID: planID, Stage: "stage name", Percent: percent, RecordedBy: "preparator-chen",
		}},
	}
}

func TestHTTPBatchRejectedLeavesNoProgress(t *testing.T) {
	f := newBatchAPI(t)
	defer f.close()
	_, p1 := f.approvedPlan(t, "HTTP-A")
	_, p2 := f.approvedPlan(t, "HTTP-B")

	payload := contracts.BatchProgressRequest{
		SubmissionID: "HTTP-FAIL",
		Entries: []contracts.BatchProgressItem{
			{PlanID: p1, Stage: "stage name", Percent: 40, RecordedBy: "preparator-chen"},
			{PlanID: p2, Stage: "stage name", Percent: 240, RecordedBy: "preparator-chen"},
		},
	}
	code, raw := f.do(t, "POST", "/plans/progress-batches", payload)
	if code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", code, raw)
	}

	// Retry after the failure returns the same rejection and still leaves nothing.
	code, _ = f.do(t, "POST", "/plans/progress-batches", payload)
	if code != http.StatusBadRequest {
		t.Fatalf("retry want 400, got %d", code)
	}

	snap1 := f.snapshot(t, p1)
	snap2 := f.snapshot(t, p2)
	if snap1.Plan.Status != "approved" || len(snap1.Progress) != 0 {
		t.Fatalf("p1 leaked: %+v", snap1)
	}
	if snap2.Plan.Status != "approved" || len(snap2.Progress) != 0 {
		t.Fatalf("p2 leaked: %+v", snap2)
	}
}

func TestHTTPBatchRetryReturnsSameResultOnce(t *testing.T) {
	f := newBatchAPI(t)
	defer f.close()
	_, p1 := f.approvedPlan(t, "HTTP-C")

	payload := batchPayload("HTTP-IDEM", p1, 55)
	code1, raw1 := f.do(t, "POST", "/plans/progress-batches", payload)
	if code1 != http.StatusCreated {
		t.Fatalf("first: %d %s", code1, raw1)
	}
	code2, raw2 := f.do(t, "POST", "/plans/progress-batches", payload)
	if code2 != http.StatusCreated {
		t.Fatalf("retry: %d %s", code2, raw2)
	}
	if !bytes.Equal(raw1, raw2) {
		t.Fatalf("retry response differs:\nfirst: %s\nretry: %s", raw1, raw2)
	}
	snap := f.snapshot(t, p1)
	if len(snap.Progress) != 1 || snap.Progress[0].Sequence != 1 || snap.Progress[0].Percent != 55 {
		t.Fatalf("snapshot after retry: %+v", snap.Progress)
	}
	if snap.Plan.Status != "active" {
		t.Fatalf("status after retry: %s", snap.Plan.Status)
	}
}

func TestHTTPBatchConcurrentRetriesApplyOnce(t *testing.T) {
	f := newBatchAPI(t)
	defer f.close()
	_, p1 := f.approvedPlan(t, "HTTP-D")

	payload := batchPayload("HTTP-RACE", p1, 65)
	raw, _ := json.Marshal(payload)

	const clients = 16
	var wg sync.WaitGroup
	codes := make([]int, clients)
	responses := make([][]byte, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Post(f.server.URL+"/plans/progress-batches", "application/json", bytes.NewReader(raw))
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			codes[i] = resp.StatusCode
			responses[i] = body
		}(i)
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusCreated {
			t.Fatalf("client %d got %d: %s", i, code, responses[i])
		}
		if !bytes.Equal(responses[i], responses[0]) {
			t.Fatalf("client %d returned a different result:\n%s\nvs\n%s", i, responses[i], responses[0])
		}
	}
	snap := f.snapshot(t, p1)
	if len(snap.Progress) != 1 || snap.Progress[0].Sequence != 1 {
		t.Fatalf("duplicate progress under concurrent retry: %+v", snap.Progress)
	}
}

func TestHTTPBatchCompletionThenSingleReleaseStillWorks(t *testing.T) {
	f := newBatchAPI(t)
	defer f.close()
	s1, p1 := f.approvedPlan(t, "HTTP-E")

	payload := batchPayload("HTTP-DONE", p1, 100)
	if code, raw := f.do(t, "POST", "/plans/progress-batches", payload); code != http.StatusCreated {
		t.Fatalf("complete: %d %s", code, raw)
	}
	snap := f.snapshot(t, p1)
	if snap.Plan.Status != "completed" || len(snap.Progress) != 1 {
		t.Fatalf("completed state wrong: %+v", snap)
	}
	code, raw := f.do(t, "POST", "/specimens/"+s1+"/release", contracts.ReleaseRequest{
		Purpose: "study purpose", Recipient: "research group", ApprovedBy: "keeper-zhou",
	})
	if code != http.StatusCreated {
		t.Fatalf("release: %d %s", code, raw)
	}
	code, raw = f.do(t, "GET", "/specimens/"+s1, nil)
	if code != http.StatusOK || !bytes.Contains(raw, []byte(`"available"`)) {
		t.Fatalf("specimen not available: %d %s", code, raw)
	}
}
