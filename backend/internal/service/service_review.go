package service

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gbtreehole/backend/internal/constants"
	"github.com/gbtreehole/backend/internal/model"
	"github.com/gbtreehole/backend/internal/repository"
	"gorm.io/gorm"
)

var (
	// ErrReviewConflict 审核项已被处理（重复提交或并发竞争），本次操作未生效。
	ErrReviewConflict = errors.New("review item already processed")
	// ErrReviewNotFound 审核项不存在。
	ErrReviewNotFound = errors.New("review item not found")
	// ErrReviewTargetMissing 审核项指向的内容已不存在，无法放行/屏蔽。
	ErrReviewTargetMissing = errors.New("review target missing")
)

type ReviewService interface {
	Enqueue(targetType string, targetID uint, content string, hitWords []string) error
	List(page, pageSize int, status int) ([]model.ReviewQueue, int64, error)
	// Approve 放行审核项。评论放行后所属帖子评论数补回、详情页可见。
	// 同一审核项重复或并发处理时仅一个成功，其余返回 ErrReviewConflict。
	Approve(queueID uint, adminID uint, note string) error
	// Reject 屏蔽审核项。评论屏蔽后保持不可见且不计入评论数。
	// 同一审核项重复或并发处理时仅一个成功，其余返回 ErrReviewConflict。
	Reject(queueID uint, adminID uint, note string) error
}

type reviewService struct {
	queue    repository.ReviewQueueRepository
	posts    repository.PostRepository
	comments repository.CommentRepository
	tx       repository.TxManager
	logger   *slog.Logger
}

func NewReviewService(queue repository.ReviewQueueRepository, posts repository.PostRepository, comments repository.CommentRepository, tx repository.TxManager, logger *slog.Logger) ReviewService {
	return &reviewService{queue: queue, posts: posts, comments: comments, tx: tx, logger: logger}
}

func (s *reviewService) Enqueue(targetType string, targetID uint, content string, hitWords []string) error {
	item := &model.ReviewQueue{
		TargetType: targetType,
		TargetID:   targetID,
		Content:    content,
		Status:     constants.ReviewStatusPending,
		HitWords:   strings.Join(hitWords, ","),
	}
	if err := s.queue.Create(item); err != nil {
		return err
	}
	return nil
}

func (s *reviewService) List(page, pageSize int, status int) ([]model.ReviewQueue, int64, error) {
	return s.queue.List(page, pageSize, status)
}

func (s *reviewService) Approve(queueID uint, adminID uint, note string) error {
	return s.process(queueID, adminID, note, constants.ReviewStatusApproved)
}

func (s *reviewService) Reject(queueID uint, adminID uint, note string) error {
	return s.process(queueID, adminID, note, constants.ReviewStatusRejected)
}

// process 在单个数据库事务内完成「抢占审核项 -> 修改目标状态 -> 校正互动计数」，
// 保证审核状态与帖子/评论状态、评论数三者原子一致：
//   - 条件 UPDATE 抢占终态，重复/并发只有一个事务成功；
//   - 评论放行时在事务内把帖子评论数补回（+1，原子表达式），屏蔽不补；
//   - 任一步骤失败整体回滚，评论数不可能重复增加或被扣减成负数。
func (s *reviewService) process(queueID uint, adminID uint, note string, finalStatus int) error {
	txErr := s.tx.WithinTransaction(func(tx *gorm.DB) error {
		queueRepo := repository.NewReviewQueueRepository(tx)
		postRepo := repository.NewPostRepository(tx)
		commentRepo := repository.NewCommentRepository(tx)

		item, err := queueRepo.MarkProcessed(queueID, adminID, finalStatus, note)
		if err != nil {
			return err
		}

		switch item.TargetType {
		case "post":
			if _, err := postRepo.FindByID(item.TargetID); err != nil {
				if errors.Is(err, repository.ErrNotFound) {
					return ErrReviewTargetMissing
				}
				return err
			}
			targetStatus := constants.PostStatusPublished
			if finalStatus == constants.ReviewStatusRejected {
				targetStatus = constants.PostStatusRejected
			}
			updated, err := postRepo.UpdateStatusIf(item.TargetID, constants.PostStatusPending, targetStatus)
			if err != nil {
				return err
			}
			if !updated {
				// 帖子状态已被改动（非待审），与审核项不一致，整体回滚避免脏数据。
				return ErrReviewConflict
			}
		case "comment":
			comment, err := commentRepo.FindByID(item.TargetID)
			if err != nil {
				if errors.Is(err, repository.ErrNotFound) {
					return ErrReviewTargetMissing
				}
				return err
			}
			targetStatus := constants.CommentStatusPublished
			if finalStatus == constants.ReviewStatusRejected {
				targetStatus = constants.CommentStatusRejected
			}
			updated, err := commentRepo.UpdateStatusIf(item.TargetID, constants.CommentStatusPending, targetStatus)
			if err != nil {
				return err
			}
			if !updated {
				// 评论状态已被改动（非待审），说明目标与审核项不一致，整体回滚。
				return ErrReviewConflict
			}
			// 评论发表命中敏感词时未计入评论数；放行时在同一事务内补回一次。
			// 屏蔽则保持不可见、不计入。
			if finalStatus == constants.ReviewStatusApproved {
				if err := postRepo.IncrementCommentCount(comment.PostID); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unknown review target type: %s", item.TargetType)
		}
		return nil
	})
	if txErr != nil {
		return s.mapProcessError(txErr)
	}
	return nil
}

func (s *reviewService) mapProcessError(err error) error {
	switch {
	case errors.Is(err, repository.ErrReviewAlreadyProcessed):
		return ErrReviewConflict
	case errors.Is(err, repository.ErrNotFound):
		return ErrReviewNotFound
	default:
		return fmt.Errorf("process review: %w", err)
	}
}
