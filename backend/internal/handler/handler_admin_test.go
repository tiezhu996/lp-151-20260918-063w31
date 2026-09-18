package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/gbtreehole/backend/internal/model"
	"github.com/gbtreehole/backend/internal/service"
)

// fakeReviewService returns a programmed error for approve/reject.
type fakeReviewService struct {
	approveErr error
	rejectErr  error
}

func (f *fakeReviewService) Enqueue(_ string, _ uint, _ string, _ []string) error {
	return nil
}

func (f *fakeReviewService) List(_, _ int, _ int) ([]model.ReviewQueue, int64, error) {
	return nil, 0, nil
}

func (f *fakeReviewService) Approve(_ uint, _ uint, _ string) error { return f.approveErr }

func (f *fakeReviewService) Reject(_ uint, _ uint, _ string) error { return f.rejectErr }

// Compile-time assertion that the fake satisfies the service interface.
var _ service.ReviewService = (*fakeReviewService)(nil)

func newReviewActionRouter(fake service.ReviewService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewAdminHandler(fake, nil, nil, nil, slog.Default())
	r.POST("/api/v1/admin/reviews/action", func(c *gin.Context) {
		c.Set("identityId", uint(1))
		h.ReviewItem(c)
	})
	return r
}

func TestReviewActionResponses(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   int
		approveErr error
		rejectErr  error
	}{
		{
			name:       "success",
			body:       `{"queueId":1,"action":"approve"}`,
			wantStatus: http.StatusOK,
			wantCode:   0,
		},
		{
			name:       "duplicate conflict",
			body:       `{"queueId":2,"action":"approve"}`,
			wantStatus: http.StatusConflict,
			wantCode:   40900,
			approveErr: service.ErrReviewConflict,
		},
		{
			name:       "target missing conflict",
			body:       `{"queueId":3,"action":"reject"}`,
			wantStatus: http.StatusConflict,
			wantCode:   40900,
			rejectErr:  service.ErrReviewTargetMissing,
		},
		{
			name:       "not found",
			body:       `{"queueId":4,"action":"approve"}`,
			wantStatus: http.StatusNotFound,
			wantCode:   40400,
			approveErr: service.ErrReviewNotFound,
		},
		{
			name:       "invalid action",
			body:       `{"queueId":5,"action":"delete"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   40000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeReviewService{approveErr: tt.approveErr, rejectErr: tt.rejectErr}
			r := newReviewActionRouter(fake)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/reviews/action", bytes.NewBufferString(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("http status = %d, want %d (body=%s)", w.Code, tt.wantStatus, w.Body.String())
			}
			var resp struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.Code != tt.wantCode {
				t.Fatalf("business code = %d, want %d, message=%s", resp.Code, tt.wantCode, resp.Message)
			}
			if tt.wantStatus >= 400 && resp.Message == "" {
				t.Fatal("expected non-empty error message for failed operation")
			}
		})
	}

	// Wrapped sentinel errors must still classify as conflict via errors.Is.
	fake := &fakeReviewService{approveErr: errors.Join(service.ErrReviewConflict, errors.New("detail"))}
	r := newReviewActionRouter(fake)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/reviews/action", bytes.NewBufferString(`{"queueId":9,"action":"approve"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("wrapped conflict status = %d, want 409", w.Code)
	}
}
