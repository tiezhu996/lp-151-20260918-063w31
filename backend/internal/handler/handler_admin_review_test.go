package handler

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/gbtreehole/backend/internal/constants"
	"github.com/gbtreehole/backend/internal/model"
	"github.com/gbtreehole/backend/internal/repository"
	"github.com/gbtreehole/backend/internal/service"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newReviewAdminRouter(t *testing.T) (*gin.Engine, *gorm.DB, ReviewSeeds) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.UserIdentity{}, &model.Post{}, &model.Tag{}, &model.PostTag{}, &model.Comment{}, &model.Like{}, &model.ReviewQueue{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	posts := repository.NewPostRepository(db)
	comments := repository.NewCommentRepository(db)
	queues := repository.NewReviewQueueRepository(db)
	tx := repository.NewTxManager(db)
	reviewSvc := service.NewReviewService(queues, posts, comments, tx, slog.Default())

	h := NewAdminHandler(reviewSvc, nil, nil, nil, slog.Default())

	r := gin.New()
	secured := r.Group("/api/v1/admin", func(c *gin.Context) {
		c.Set("identityId", uint(1)) // 管理入口沿用身份鉴权，管理员身份固定写入
		c.Next()
	})
	secured.POST("/reviews/action", h.ReviewItem)

	identity := &model.UserIdentity{IdentityKey: "k", Nickname: "n", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := db.Create(identity).Error; err != nil {
		t.Fatalf("identity: %v", err)
	}
	post := &model.Post{IdentityID: identity.ID, Content: "post", Status: constants.PostStatusPublished, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := posts.Create(post); err != nil {
		t.Fatalf("post: %v", err)
	}
	comment := &model.Comment{PostID: post.ID, IdentityID: identity.ID, Content: "赌博", Status: constants.CommentStatusPending, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := comments.Create(comment); err != nil {
		t.Fatalf("comment: %v", err)
	}
	queue := &model.ReviewQueue{TargetType: "comment", TargetID: comment.ID, Content: "赌博", Status: constants.ReviewStatusPending, HitWords: "赌博", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := queues.Create(queue); err != nil {
		t.Fatalf("queue: %v", err)
	}
	return r, db, ReviewSeeds{PostID: post.ID, CommentID: comment.ID, QueueID: queue.ID}
}

type ReviewSeeds struct {
	PostID    uint
	CommentID uint
	QueueID   uint
}

func postReviewAction(t *testing.T, r *gin.Engine, body map[string]any) (int, Response) {
	t.Helper()
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/reviews/action", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	var resp Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", w.Body.String(), err)
	}
	return w.Code, resp
}

// TestReviewActionHTTPConflict 放行成功后重复处理返回 409 冲突；屏蔽路径同理。
func TestReviewActionHTTPConflict(t *testing.T) {
	r, db, seeds := newReviewAdminRouter(t)

	status, resp := postReviewAction(t, r, map[string]any{"queueId": seeds.QueueID, "action": "approve"})
	if status != http.StatusOK || resp.Code != constants.CodeOK {
		t.Fatalf("first approve: http=%d code=%d msg=%s", status, resp.Code, resp.Message)
	}

	// 重复放行 -> 409 明确冲突
	status, resp = postReviewAction(t, r, map[string]any{"queueId": seeds.QueueID, "action": "approve"})
	if status != http.StatusConflict || resp.Code != constants.CodeConflict {
		t.Fatalf("duplicate approve: http=%d code=%d msg=%s", status, resp.Code, resp.Message)
	}
	// 改为屏蔽也必须冲突
	status, _ = postReviewAction(t, r, map[string]any{"queueId": seeds.QueueID, "action": "reject"})
	if status != http.StatusConflict {
		t.Fatalf("reject after approve: http=%d, want 409", status)
	}

	// 不存在的审核项 -> 404
	status, resp = postReviewAction(t, r, map[string]any{"queueId": 9999, "action": "approve"})
	if status != http.StatusNotFound || resp.Code != constants.CodeNotFound {
		t.Fatalf("missing: http=%d code=%d", status, resp.Code)
	}

	// 终态一致：评论已放行、评论数恰好补回 1
	var post model.Post
	if err := db.First(&post, seeds.PostID).Error; err != nil {
		t.Fatalf("load post: %v", err)
	}
	if post.CommentCount != 1 {
		t.Fatalf("comment count = %d, want 1", post.CommentCount)
	}
	var comment model.Comment
	if err := db.First(&comment, seeds.CommentID).Error; err != nil {
		t.Fatalf("load comment: %v", err)
	}
	if comment.Status != constants.CommentStatusPublished {
		t.Fatalf("comment status = %d, want published", comment.Status)
	}
}

// TestReviewActionHTTPRejectKeepsZero 屏蔽：200 成功，重复屏蔽 409，计数保持 0。
func TestReviewActionHTTPRejectKeepsZero(t *testing.T) {
	r, db, seeds := newReviewAdminRouter(t)

	status, _ := postReviewAction(t, r, map[string]any{"queueId": seeds.QueueID, "action": "reject"})
	if status != http.StatusOK {
		t.Fatalf("reject http=%d, want 200", status)
	}
	status, resp := postReviewAction(t, r, map[string]any{"queueId": seeds.QueueID, "action": "reject"})
	if status != http.StatusConflict || resp.Code != constants.CodeConflict {
		t.Fatalf("duplicate reject: http=%d code=%d", status, resp.Code)
	}
	var post model.Post
	if err := db.First(&post, seeds.PostID).Error; err != nil {
		t.Fatalf("load post: %v", err)
	}
	if post.CommentCount != 0 {
		t.Fatalf("comment count = %d, want 0", post.CommentCount)
	}
}
