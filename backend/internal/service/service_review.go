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

// Review action conflicts surfaced to handlers with errors.Is.
var (
	// ErrReviewNotFound means the review item id does not exist.
	ErrReviewNotFound = errors.New("review item not found")
	// ErrReviewConflict means the item has already been handled or its target
	// is not in the expected state; the operation had no effect.
	ErrReviewConflict = errors.New("review action conflict")
	// ErrReviewTargetMissing means the reviewed content was deleted before the
	// action could take effect.
	ErrReviewTargetMissing = errors.New("review target missing")
	// ErrReviewTargetType means the queue item carries an unsupported type.
	ErrReviewTargetType = errors.New("unknown review target type")
)

type ReviewService interface {
	Enqueue(targetType string, targetID uint, content string, hitWords []string) error
	List(page, pageSize int, status int) ([]model.ReviewQueue, int64, error)
	Approve(queueID uint, adminID uint, note string) error
	Reject(queueID uint, adminID uint, note string) error
}

type reviewService struct {
	queue    repository.ReviewQueueRepository
	posts    repository.PostRepository
	comments repository.CommentRepository
	tx       repository.TxManager
	logger   *slog.Logger
}

func NewReviewService(
	queue repository.ReviewQueueRepository,
	posts repository.PostRepository,
	comments repository.CommentRepository,
	tx repository.TxManager,
	logger *slog.Logger,
) ReviewService {
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
		return fmt.Errorf("enqueue review item: %w", err)
	}
	return nil
}

func (s *reviewService) List(page, pageSize int, status int) ([]model.ReviewQueue, int64, error) {
	return s.queue.List(page, pageSize, status)
}

// Approve releases a pending review item. For comments the owning post's
// comment_count is restored in the same transaction as the visibility change,
// so the counter and the visible content can never diverge. The whole flow is
// driven by an atomic conditional claim: duplicate or concurrent calls lose
// the claim and receive ErrReviewConflict without any side effect.
func (s *reviewService) Approve(queueID uint, adminID uint, note string) error {
	return s.process(queueID, adminID, note, constants.ReviewStatusApproved, true)
}

// Reject permanently shields a pending review item. Rejected content stays
// invisible on detail pages and is never counted. As with Approve, only one
// concurrent/duplicate caller can win the claim.
func (s *reviewService) Reject(queueID uint, adminID uint, note string) error {
	return s.process(queueID, adminID, note, constants.ReviewStatusRejected, false)
}

func (s *reviewService) process(queueID, adminID uint, note string, reviewStatus int, publish bool) error {
	if err := s.tx.WithinTransaction(func(tx *gorm.DB) error {
		// 1. Atomic claim. Either this call wins ownership of the pending item
		//    or nothing at all happens (including counter changes).
		item, err := s.queue.ClaimPending(tx, queueID, uint(reviewStatus), adminID, note)
		if err != nil {
			return err
		}

		// 2. Apply the content-state transition and, for an approved comment,
		//    restore the owning post's comment count. Any failure rolls the
		//    claim back so the queue and target content stay consistent.
		switch item.TargetType {
		case "post":
			return s.transitionPost(tx, item.TargetID, publish)
		case "comment":
			var comment model.Comment
			if err := tx.Select("id", "post_id", "status").First(&comment, item.TargetID).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return repository.ErrTargetMissing
				}
				return fmt.Errorf("load comment %d for review: %w", item.TargetID, err)
			}
			if err := s.transitionComment(tx, comment.ID, publish); err != nil {
				return err
			}
			if publish {
				if err := s.posts.IncrementCommentCount(tx, comment.PostID, 1); err != nil {
					return err
				}
			}
			return nil
		default:
			return fmt.Errorf("%w: %s", ErrReviewTargetType, item.TargetType)
		}
	}); err != nil {
		return s.mapProcessError(err)
	}
	return nil
}

func (s *reviewService) transitionPost(tx *gorm.DB, postID uint, publish bool) error {
	wantStatus := constants.PostStatusPending
	toStatus := constants.PostStatusRejected
	if publish {
		toStatus = constants.PostStatusPublished
	}
	return s.posts.TransitionStatus(tx, postID, wantStatus, toStatus)
}

func (s *reviewService) transitionComment(tx *gorm.DB, commentID uint, publish bool) error {
	wantStatus := constants.CommentStatusPending
	toStatus := constants.CommentStatusRejected
	if publish {
		toStatus = constants.CommentStatusPublished
	}
	return s.comments.TransitionStatus(tx, commentID, wantStatus, toStatus)
}

// mapProcessError converts repository sentinel errors into the stable service
// errors handlers branch on; unexpected errors are wrapped and propagated.
func (s *reviewService) mapProcessError(err error) error {
	switch {
	case errors.Is(err, repository.ErrReviewAlreadyProcessed):
		return fmt.Errorf("%w: %w", ErrReviewConflict, err)
	case errors.Is(err, repository.ErrReviewNotFound):
		return fmt.Errorf("%w: %w", ErrReviewNotFound, err)
	case errors.Is(err, repository.ErrTargetMissing):
		return fmt.Errorf("%w: %w", ErrReviewTargetMissing, err)
	case errors.Is(err, repository.ErrTargetStateConflict):
		return fmt.Errorf("%w: %w", ErrReviewConflict, err)
	case errors.Is(err, ErrReviewTargetType):
		return err
	default:
		return fmt.Errorf("process review item: %w", err)
	}
}
