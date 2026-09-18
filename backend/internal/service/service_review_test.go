package service

import (
	"errors"
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

func newReviewTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// 单连接：并发事务在同一连接上串行执行，模拟行锁竞争语义。
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get sql db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&model.UserIdentity{}, &model.Post{}, &model.Tag{}, &model.PostTag{}, &model.Comment{}, &model.Like{}, &model.ReviewQueue{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		_ = sqlDB.Close()
	})
	return db
}

type reviewFixture struct {
	db       *gorm.DB
	svc      ReviewService
	posts    repository.PostRepository
	comments repository.CommentRepository
	queues   repository.ReviewQueueRepository
}

func newReviewFixture(t *testing.T) *reviewFixture {
	t.Helper()
	db := newReviewTestDB(t)
	posts := repository.NewPostRepository(db)
	comments := repository.NewCommentRepository(db)
	queues := repository.NewReviewQueueRepository(db)
	tx := repository.NewTxManager(db)
	svc := NewReviewService(queues, posts, comments, tx, slog.Default())
	return &reviewFixture{db: db, svc: svc, posts: posts, comments: comments, queues: queues}
}

func seedPendingComment(t *testing.T, f *reviewFixture) (*model.Post, *model.Comment, *model.ReviewQueue) {
	t.Helper()
	identity := &model.UserIdentity{IdentityKey: "key-review", Nickname: "树洞用户", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := f.db.Create(identity).Error; err != nil {
		t.Fatalf("create identity: %v", err)
	}
	post := &model.Post{
		IdentityID: identity.ID, Content: "帖子正文", Status: constants.PostStatusPublished,
		CommentCount: 0, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := f.posts.Create(post); err != nil {
		t.Fatalf("create post: %v", err)
	}
	comment := &model.Comment{
		PostID: post.ID, IdentityID: identity.ID, Content: "命中赌博的评论",
		Status: constants.CommentStatusPending, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := f.comments.Create(comment); err != nil {
		t.Fatalf("create comment: %v", err)
	}
	queue := &model.ReviewQueue{
		TargetType: "comment", TargetID: comment.ID, Content: comment.Content,
		Status: constants.ReviewStatusPending, HitWords: "赌博", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := f.queues.Create(queue); err != nil {
		t.Fatalf("create review queue: %v", err)
	}
	return post, comment, queue
}

// TestReviewApproveCommentRestoresCount 放行待审评论后：评论可见、帖子评论数补回。
func TestReviewApproveCommentRestoresCount(t *testing.T) {
	f := newReviewFixture(t)
	post, comment, queue := seedPendingComment(t, f)

	if err := f.svc.Approve(queue.ID, 1, "ok"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	updatedComment, err := f.comments.FindByID(comment.ID)
	if err != nil {
		t.Fatalf("find comment: %v", err)
	}
	if updatedComment.Status != constants.CommentStatusPublished {
		t.Fatalf("comment status = %d, want published(%d)", updatedComment.Status, constants.CommentStatusPublished)
	}
	updatedPost, err := f.posts.FindByID(post.ID)
	if err != nil {
		t.Fatalf("find post: %v", err)
	}
	if updatedPost.CommentCount != 1 {
		t.Fatalf("comment count = %d, want 1", updatedPost.CommentCount)
	}
	// 详情页评论列表只返回已放行评论
	list, total, err := f.comments.ListByPostID(post.ID, 1, 10, constants.CommentStatusPublished)
	if err != nil {
		t.Fatalf("list comments: %v", err)
	}
	if total != 1 || len(list) != 1 || list[0].ID != comment.ID {
		t.Fatalf("published comment not visible on detail: total=%d len=%d", total, len(list))
	}
}

// TestReviewRejectCommentKeepsInvisibleAndNotCounted 屏蔽后评论不可见且不计入评论数。
func TestReviewRejectCommentKeepsInvisibleAndNotCounted(t *testing.T) {
	f := newReviewFixture(t)
	post, comment, queue := seedPendingComment(t, f)

	if err := f.svc.Reject(queue.ID, 1, "block"); err != nil {
		t.Fatalf("reject: %v", err)
	}

	updatedComment, err := f.comments.FindByID(comment.ID)
	if err != nil {
		t.Fatalf("find comment: %v", err)
	}
	if updatedComment.Status != constants.CommentStatusRejected {
		t.Fatalf("comment status = %d, want rejected(%d)", updatedComment.Status, constants.CommentStatusRejected)
	}
	updatedPost, err := f.posts.FindByID(post.ID)
	if err != nil {
		t.Fatalf("find post: %v", err)
	}
	if updatedPost.CommentCount != 0 {
		t.Fatalf("comment count = %d, want 0", updatedPost.CommentCount)
	}
	_, total, err := f.comments.ListByPostID(post.ID, 1, 10, constants.CommentStatusPublished)
	if err != nil {
		t.Fatalf("list comments: %v", err)
	}
	if total != 0 {
		t.Fatalf("rejected comment must be invisible, got total=%d", total)
	}
}

// TestReviewDuplicateApproveConflict 同一审核项放行后重复放行返回冲突，评论数不重复增加。
func TestReviewDuplicateApproveConflict(t *testing.T) {
	f := newReviewFixture(t)
	post, _, queue := seedPendingComment(t, f)

	if err := f.svc.Approve(queue.ID, 1, "ok"); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	err := f.svc.Approve(queue.ID, 1, "again")
	if !errors.Is(err, ErrReviewConflict) {
		t.Fatalf("second approve err = %v, want ErrReviewConflict", err)
	}
	// 换屏蔽动作同样冲突
	if err := f.svc.Reject(queue.ID, 1, "flip"); !errors.Is(err, ErrReviewConflict) {
		t.Fatalf("reject after approve err = %v, want ErrReviewConflict", err)
	}

	updatedPost, _ := f.posts.FindByID(post.ID)
	if updatedPost.CommentCount != 1 {
		t.Fatalf("comment count = %d, want exactly 1", updatedPost.CommentCount)
	}
	updatedQueue, _ := f.queues.FindByID(queue.ID)
	if updatedQueue.Status != constants.ReviewStatusApproved {
		t.Fatalf("queue status changed by losing request: %d", updatedQueue.Status)
	}
}

// TestReviewDuplicateRejectConflict 屏蔽后重复处理返回冲突，评论仍不可见、计数为 0（不会变负）。
func TestReviewDuplicateRejectConflict(t *testing.T) {
	f := newReviewFixture(t)
	post, _, queue := seedPendingComment(t, f)

	if err := f.svc.Reject(queue.ID, 1, "block"); err != nil {
		t.Fatalf("first reject: %v", err)
	}
	if err := f.svc.Reject(queue.ID, 1, "again"); !errors.Is(err, ErrReviewConflict) {
		t.Fatalf("second reject err = %v, want ErrReviewConflict", err)
	}
	if err := f.svc.Approve(queue.ID, 1, "flip"); !errors.Is(err, ErrReviewConflict) {
		t.Fatalf("approve after reject err = %v, want ErrReviewConflict", err)
	}

	updatedPost, _ := f.posts.FindByID(post.ID)
	if updatedPost.CommentCount < 0 {
		t.Fatalf("comment count negative: %d", updatedPost.CommentCount)
	}
	if updatedPost.CommentCount != 0 {
		t.Fatalf("comment count = %d, want 0", updatedPost.CommentCount)
	}
}

// TestReviewNotFound 处理不存在的审核项返回明确的未找到错误。
func TestReviewNotFound(t *testing.T) {
	f := newReviewFixture(t)
	if err := f.svc.Approve(9999, 1, ""); !errors.Is(err, ErrReviewNotFound) {
		t.Fatalf("approve missing err = %v, want ErrReviewNotFound", err)
	}
	if err := f.svc.Reject(9999, 1, ""); !errors.Is(err, ErrReviewNotFound) {
		t.Fatalf("reject missing err = %v, want ErrReviewNotFound", err)
	}
}

// TestReviewConcurrentApproveOnlyOneWins 并发放行同一审核项：仅一个成功，
// 其余全部冲突，帖子评论数恰好 +1。
func TestReviewConcurrentApproveOnlyOneWins(t *testing.T) {
	f := newReviewFixture(t)

	post, _, queue := seedPendingComment(t, f)

	const n = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var wins, conflicts, others int
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			err := f.svc.Approve(queue.ID, 1, "concurrent")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrReviewConflict):
				conflicts++
			default:
				others++
			}
		}()
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1 (conflicts=%d others=%d)", wins, conflicts, others)
	}
	if conflicts != n-1 {
		t.Fatalf("conflicts = %d, want %d (others=%d)", conflicts, n-1, others)
	}
	updatedPost, _ := f.posts.FindByID(post.ID)
	if updatedPost.CommentCount != 1 {
		t.Fatalf("comment count = %d, want exactly 1", updatedPost.CommentCount)
	}
}

// TestReviewApprovePostDoesNotTouchCommentCount 放行帖子不影响帖子的评论计数。
func TestReviewApprovePostDoesNotTouchCommentCount(t *testing.T) {
	f := newReviewFixture(t)
	identity := &model.UserIdentity{IdentityKey: "key-post-review", Nickname: "楼主", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := f.db.Create(identity).Error; err != nil {
		t.Fatalf("create identity: %v", err)
	}
	post := &model.Post{
		IdentityID: identity.ID, Content: "命中暴力的帖子", Status: constants.PostStatusPending,
		CommentCount: 2, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := f.posts.Create(post); err != nil {
		t.Fatalf("create post: %v", err)
	}
	queue := &model.ReviewQueue{
		TargetType: "post", TargetID: post.ID, Content: post.Content,
		Status: constants.ReviewStatusPending, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := f.queues.Create(queue); err != nil {
		t.Fatalf("create queue: %v", err)
	}

	if err := f.svc.Approve(queue.ID, 1, "ok"); err != nil {
		t.Fatalf("approve post: %v", err)
	}
	updatedPost, _ := f.posts.FindByID(post.ID)
	if updatedPost.Status != constants.PostStatusPublished {
		t.Fatalf("post status = %d, want published", updatedPost.Status)
	}
	if updatedPost.CommentCount != 2 {
		t.Fatalf("comment count = %d, want unchanged 2", updatedPost.CommentCount)
	}
}
