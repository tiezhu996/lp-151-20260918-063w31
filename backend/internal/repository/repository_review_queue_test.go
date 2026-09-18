package repository

import (
	"errors"
	"testing"
	"time"

	"github.com/gbtreehole/backend/internal/constants"
	"github.com/gbtreehole/backend/internal/model"
)

// TestReviewQueueMarkProcessedCAS 条件更新只能把待审改写一次，重复处理拿到冲突错误。
func TestReviewQueueMarkProcessedCAS(t *testing.T) {
	db := newTestDB(t)
	repo := NewReviewQueueRepository(db)
	item := &model.ReviewQueue{TargetType: "comment", TargetID: 1, Status: constants.ReviewStatusPending, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := repo.Create(item); err != nil {
		t.Fatalf("create: %v", err)
	}

	processed, err := repo.MarkProcessed(item.ID, 7, constants.ReviewStatusApproved, "ok")
	if err != nil {
		t.Fatalf("first mark: %v", err)
	}
	if processed.Status != constants.ReviewStatusApproved || processed.ReviewedBy == nil || *processed.ReviewedBy != 7 {
		t.Fatalf("processed item mismatch: %+v", processed)
	}

	// 再次处理（无论目标终态是什么）都应冲突
	if _, err := repo.MarkProcessed(item.ID, 8, constants.ReviewStatusRejected, "again"); !errors.Is(err, ErrReviewAlreadyProcessed) {
		t.Fatalf("second mark err = %v, want ErrReviewAlreadyProcessed", err)
	}

	got, _ := repo.FindByID(item.ID)
	if got.Status != constants.ReviewStatusApproved {
		t.Fatalf("status mutated by losing request: %d", got.Status)
	}
	if got.ReviewNote != "ok" {
		t.Fatalf("note mutated by losing request: %q", got.ReviewNote)
	}

	// 不存在的审核项返回未找到
	if _, err := repo.MarkProcessed(9999, 1, constants.ReviewStatusApproved, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing mark err = %v, want ErrNotFound", err)
	}
}

// TestPostIncrementCommentCount 原子自增评论数且对不存在的帖子报错。
func TestPostIncrementCommentCount(t *testing.T) {
	db := newTestDB(t)
	repo := NewPostRepository(db)
	post := &model.Post{Content: "c", Status: constants.PostStatusPublished, CommentCount: 0, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := repo.Create(post); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.IncrementCommentCount(post.ID); err != nil {
		t.Fatalf("increment: %v", err)
	}
	if err := repo.IncrementCommentCount(post.ID); err != nil {
		t.Fatalf("increment again: %v", err)
	}
	got, _ := repo.FindByID(post.ID)
	if got.CommentCount != 2 {
		t.Fatalf("comment count = %d, want 2", got.CommentCount)
	}
	if err := repo.IncrementCommentCount(9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("increment missing err = %v, want ErrNotFound", err)
	}
}

// TestCommentAndPostUpdateStatusIf 状态条件更新只在当前状态匹配时生效。
func TestCommentAndPostUpdateStatusIf(t *testing.T) {
	db := newTestDB(t)
	postRepo := NewPostRepository(db)
	commentRepo := NewCommentRepository(db)

	post := &model.Post{Content: "p", Status: constants.PostStatusPending, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := postRepo.Create(post); err != nil {
		t.Fatalf("create post: %v", err)
	}
	comment := &model.Comment{PostID: post.ID, Content: "cm", Status: constants.CommentStatusPending, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := commentRepo.Create(comment); err != nil {
		t.Fatalf("create comment: %v", err)
	}

	ok, err := postRepo.UpdateStatusIf(post.ID, constants.PostStatusPending, constants.PostStatusPublished)
	if err != nil || !ok {
		t.Fatalf("post status update: ok=%v err=%v", ok, err)
	}
	// 已放行的帖子再按待审条件更新应不命中
	ok, err = postRepo.UpdateStatusIf(post.ID, constants.PostStatusPending, constants.PostStatusRejected)
	if err != nil || ok {
		t.Fatalf("post second update should miss: ok=%v err=%v", ok, err)
	}

	ok, err = commentRepo.UpdateStatusIf(comment.ID, constants.CommentStatusPending, constants.CommentStatusRejected)
	if err != nil || !ok {
		t.Fatalf("comment status update: ok=%v err=%v", ok, err)
	}
	ok, err = commentRepo.UpdateStatusIf(comment.ID, constants.CommentStatusPending, constants.CommentStatusPublished)
	if err != nil || ok {
		t.Fatalf("comment second update should miss: ok=%v err=%v", ok, err)
	}
}
