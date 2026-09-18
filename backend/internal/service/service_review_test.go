package service

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/gbtreehole/backend/internal/constants"
	"github.com/gbtreehole/backend/internal/model"
	"github.com/gbtreehole/backend/internal/repository"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newReviewTestDB opens an in-memory SQLite database shared across connections
// (so transactions see the same schema/data). A single pooled connection makes
// BEGIN IMMEDIATE behavior deterministic for the concurrency test; busy_timeout
// lets goroutines wait their turn instead of failing on lock contention.
func newReviewTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:review-%d?mode=memory&cache=shared&busy_timeout=5000", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get sql db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&model.UserIdentity{}, &model.Post{}, &model.Comment{}, &model.ReviewQueue{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

type reviewFixture struct {
	db       *gorm.DB
	svc      ReviewService
	postRepo repository.PostRepository
	commRepo repository.CommentRepository
	queue    repository.ReviewQueueRepository
}

func newReviewFixture(t *testing.T) *reviewFixture {
	t.Helper()
	db := newReviewTestDB(t)
	postRepo := repository.NewPostRepository(db)
	commRepo := repository.NewCommentRepository(db)
	queueRepo := repository.NewReviewQueueRepository(db)
	txMgr := repository.NewTxManager(db)
	svc := NewReviewService(queueRepo, postRepo, commRepo, txMgr, slog.Default())
	return &reviewFixture{db: db, svc: svc, postRepo: postRepo, commRepo: commRepo, queue: queueRepo}
}

func (f *reviewFixture) seedIdentity(t *testing.T) *model.UserIdentity {
	t.Helper()
	identity := &model.UserIdentity{
		IdentityKey: fmt.Sprintf("key-%d", time.Now().UnixNano()),
		Nickname:    "树洞居民",
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := f.db.Create(identity).Error; err != nil {
		t.Fatalf("create identity: %v", err)
	}
	return identity
}

// seedPendingComment creates a published post with one already-published
// comment and a second pending comment with a pending review item, and
// returns the post and the pending comment.
func (f *reviewFixture) seedPendingComment(t *testing.T) (*model.Post, *model.Comment, *model.ReviewQueue) {
	t.Helper()
	identity := f.seedIdentity(t)
	now := time.Now()
	post := &model.Post{
		IdentityID:   identity.ID,
		Content:      "一条正常帖子",
		Status:       constants.PostStatusPublished,
		CommentCount: 1,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := f.postRepo.Create(post); err != nil {
		t.Fatalf("create post: %v", err)
	}
	visible := &model.Comment{
		PostID:     post.ID,
		IdentityID: identity.ID,
		Content:    "已发布的评论",
		Status:     constants.CommentStatusPublished,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := f.commRepo.Create(visible); err != nil {
		t.Fatalf("create visible comment: %v", err)
	}
	pending := &model.Comment{
		PostID:     post.ID,
		IdentityID: identity.ID,
		Content:    "命中敏感词待审的评论",
		Status:     constants.CommentStatusPending,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := f.commRepo.Create(pending); err != nil {
		t.Fatalf("create pending comment: %v", err)
	}
	item := &model.ReviewQueue{
		TargetType: "comment",
		TargetID:   pending.ID,
		Content:    pending.Content,
		Status:     constants.ReviewStatusPending,
		HitWords:   "赌博",
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := f.queue.Create(item); err != nil {
		t.Fatalf("create review item: %v", err)
	}
	return post, pending, item
}

func (f *reviewFixture) seedPendingPost(t *testing.T) (*model.Post, *model.ReviewQueue) {
	t.Helper()
	identity := f.seedIdentity(t)
	now := time.Now()
	post := &model.Post{
		IdentityID: identity.ID,
		Content:    "命中敏感词待审的帖子",
		Status:     constants.PostStatusPending,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := f.postRepo.Create(post); err != nil {
		t.Fatalf("create post: %v", err)
	}
	item := &model.ReviewQueue{
		TargetType: "post",
		TargetID:   post.ID,
		Content:    post.Content,
		Status:     constants.ReviewStatusPending,
		HitWords:   "诈骗",
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := f.queue.Create(item); err != nil {
		t.Fatalf("create review item: %v", err)
	}
	return post, item
}

// 放行待审评论：评论变为已发布、帖子评论数补回 +1、评论在详情页可见，
// 且审核项只生效一次。
func TestReviewService_ApproveCommentRestoresCountAndVisibility(t *testing.T) {
	f := newReviewFixture(t)
	post, pending, item := f.seedPendingComment(t)

	if err := f.svc.Approve(item.ID, 7, "ok"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	updated, err := f.postRepo.FindByID(post.ID)
	if err != nil {
		t.Fatalf("reload post: %v", err)
	}
	if updated.CommentCount != 2 {
		t.Fatalf("comment count = %d, want 2", updated.CommentCount)
	}

	comment, err := f.commRepo.FindByID(pending.ID)
	if err != nil {
		t.Fatalf("reload comment: %v", err)
	}
	if comment.Status != constants.CommentStatusPublished {
		t.Fatalf("comment status = %d, want published %d", comment.Status, constants.CommentStatusPublished)
	}

	// 详情页评论接口只返回已发布评论，放行后该评论必须出现。
	comments, total, err := f.commRepo.ListByPostID(post.ID, 1, 50, constants.CommentStatusPublished)
	if err != nil {
		t.Fatalf("list published comments: %v", err)
	}
	if total != 2 || len(comments) != 2 {
		t.Fatalf("visible comments = %d (total %d), want 2", len(comments), total)
	}

	queueItem, err := f.queue.FindByID(item.ID)
	if err != nil {
		t.Fatalf("reload review item: %v", err)
	}
	if queueItem.Status != constants.ReviewStatusApproved {
		t.Fatalf("review status = %d, want approved", queueItem.Status)
	}
	if queueItem.ReviewedBy == nil || *queueItem.ReviewedBy != 7 {
		t.Fatalf("reviewed by = %v, want 7", queueItem.ReviewedBy)
	}
}

// 屏蔽待审评论：保持不可见、不计入评论数、审核项变为已屏蔽。
func TestReviewService_RejectCommentStaysInvisible(t *testing.T) {
	f := newReviewFixture(t)
	post, pending, item := f.seedPendingComment(t)

	if err := f.svc.Reject(item.ID, 9, "violation"); err != nil {
		t.Fatalf("reject: %v", err)
	}

	updated, err := f.postRepo.FindByID(post.ID)
	if err != nil {
		t.Fatalf("reload post: %v", err)
	}
	if updated.CommentCount != 1 {
		t.Fatalf("comment count = %d, want 1 (rejected must not count)", updated.CommentCount)
	}

	comment, err := f.commRepo.FindByID(pending.ID)
	if err != nil {
		t.Fatalf("reload comment: %v", err)
	}
	if comment.Status != constants.CommentStatusRejected {
		t.Fatalf("comment status = %d, want rejected", comment.Status)
	}

	_, total, err := f.commRepo.ListByPostID(post.ID, 1, 50, constants.CommentStatusPublished)
	if err != nil {
		t.Fatalf("list comments: %v", err)
	}
	if total != 1 {
		t.Fatalf("visible comments total = %d, want 1", total)
	}
}

// 同一审核项重复处理：只有第一次成功，后续全部返回冲突错误且无副作用。
func TestReviewService_DuplicateApproveOnlyAppliesOnce(t *testing.T) {
	f := newReviewFixture(t)
	post, _, item := f.seedPendingComment(t)

	if err := f.svc.Approve(item.ID, 1, "first"); err != nil {
		t.Fatalf("first approve: %v", err)
	}

	// 重复放行
	err := f.svc.Approve(item.ID, 2, "second")
	if !errors.Is(err, ErrReviewConflict) {
		t.Fatalf("duplicate approve error = %v, want ErrReviewConflict", err)
	}

	// 放行后再屏蔽也必须失败，不能把内容改回不可见
	err = f.svc.Reject(item.ID, 2, "late reject")
	if !errors.Is(err, ErrReviewConflict) {
		t.Fatalf("approve then reject error = %v, want ErrReviewConflict", err)
	}

	updated, _ := f.postRepo.FindByID(post.ID)
	if updated.CommentCount != 2 {
		t.Fatalf("comment count after duplicate actions = %d, want 2 (no double increment)", updated.CommentCount)
	}
	comment, _ := f.commRepo.FindByID(item.TargetID)
	if comment.Status != constants.CommentStatusPublished {
		t.Fatalf("comment status = %d, must stay published after late reject", comment.Status)
	}
}

// 屏蔽后再放行也必须冲突。
func TestReviewService_RejectThenApproveConflict(t *testing.T) {
	f := newReviewFixture(t)
	_, _, item := f.seedPendingComment(t)

	if err := f.svc.Reject(item.ID, 1, ""); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if err := f.svc.Approve(item.ID, 1, ""); !errors.Is(err, ErrReviewConflict) {
		t.Fatalf("reject then approve error = %v, want ErrReviewConflict", err)
	}
}

// 并发处理同一审核项：无论多少个请求，只能一个成功，评论数只增加一次。
func TestReviewService_ConcurrentApproveExactlyOneWins(t *testing.T) {
	f := newReviewFixture(t)
	post, _, item := f.seedPendingComment(t)

	const workers = 16
	var wg sync.WaitGroup
	var success, conflict int64
	var mu sync.Mutex
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			err := f.svc.Approve(item.ID, uint(i+1), "concurrent")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				success++
			case errors.Is(err, ErrReviewConflict):
				conflict++
			default:
				t.Errorf("unexpected approve error: %v", err)
			}
		}()
	}
	wg.Wait()

	if success != 1 {
		t.Fatalf("success count = %d, want exactly 1", success)
	}
	if conflict != workers-1 {
		t.Fatalf("conflict count = %d, want %d", conflict, workers-1)
	}

	updated, err := f.postRepo.FindByID(post.ID)
	if err != nil {
		t.Fatalf("reload post: %v", err)
	}
	if updated.CommentCount != 2 {
		t.Fatalf("comment count after concurrent approves = %d, want 2", updated.CommentCount)
	}
}

// 不存在的审核项返回 not found。
func TestReviewService_UnknownItemNotFound(t *testing.T) {
	f := newReviewFixture(t)
	if err := f.svc.Approve(99999, 1, ""); !errors.Is(err, ErrReviewNotFound) {
		t.Fatalf("error = %v, want ErrReviewNotFound", err)
	}
	if err := f.svc.Reject(99999, 1, ""); !errors.Is(err, ErrReviewNotFound) {
		t.Fatalf("error = %v, want ErrReviewNotFound", err)
	}
}

// 被审评论在处理前被删除：返回目标缺失冲突，审核项不应残留为已放行状态。
func TestReviewService_ApproveCommentTargetDeleted(t *testing.T) {
	f := newReviewFixture(t)
	_, pending, item := f.seedPendingComment(t)
	if err := f.db.Delete(&model.Comment{}, pending.ID).Error; err != nil {
		t.Fatalf("delete comment: %v", err)
	}

	err := f.svc.Approve(item.ID, 1, "")
	if !errors.Is(err, ErrReviewTargetMissing) {
		t.Fatalf("error = %v, want ErrReviewTargetMissing", err)
	}

	// 事务回滚：审核项仍为待审，等待人工处理
	queueItem, _ := f.queue.FindByID(item.ID)
	if queueItem.Status != constants.ReviewStatusPending {
		t.Fatalf("review status = %d, want pending after rollback", queueItem.Status)
	}
}

// 帖子审核放行/屏蔽：仅状态变更，不影响评论数。
func TestReviewService_PostApproveAndReject(t *testing.T) {
	t.Run("approve post", func(t *testing.T) {
		f := newReviewFixture(t)
		post, item := f.seedPendingPost(t)
		if err := f.svc.Approve(item.ID, 1, ""); err != nil {
			t.Fatalf("approve post: %v", err)
		}
		got, _ := f.postRepo.FindByID(post.ID)
		if got.Status != constants.PostStatusPublished {
			t.Fatalf("post status = %d, want published", got.Status)
		}
	})
	t.Run("reject post", func(t *testing.T) {
		f := newReviewFixture(t)
		post, item := f.seedPendingPost(t)
		if err := f.svc.Reject(item.ID, 1, ""); err != nil {
			t.Fatalf("reject post: %v", err)
		}
		got, _ := f.postRepo.FindByID(post.ID)
		if got.Status != constants.PostStatusRejected {
			t.Fatalf("post status = %d, want rejected", got.Status)
		}
	})
}

// 评论数原子调整不会变为负数。
func TestPostRepository_CommentCountCannotGoNegative(t *testing.T) {
	db := newReviewTestDB(t)
	postRepo := repository.NewPostRepository(db)
	identity := &model.UserIdentity{IdentityKey: "neg-key", Nickname: "n", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := db.Create(identity).Error; err != nil {
		t.Fatalf("create identity: %v", err)
	}
	post := &model.Post{IdentityID: identity.ID, Content: "c", Status: constants.PostStatusPublished, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := postRepo.Create(post); err != nil {
		t.Fatalf("create post: %v", err)
	}
	if err := postRepo.IncrementCommentCount(nil, post.ID, 1); err != nil {
		t.Fatalf("increment: %v", err)
	}
	if err := postRepo.IncrementCommentCount(nil, post.ID, -1); err != nil {
		t.Fatalf("decrement to zero: %v", err)
	}
	if err := postRepo.IncrementCommentCount(nil, post.ID, -1); !errors.Is(err, repository.ErrTargetStateConflict) {
		t.Fatalf("error = %v, want ErrTargetStateConflict", err)
	}
	got, _ := postRepo.FindByID(post.ID)
	if got.CommentCount != 0 {
		t.Fatalf("comment count = %d, want 0 (never negative)", got.CommentCount)
	}
}
