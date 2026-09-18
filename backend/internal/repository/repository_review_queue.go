package repository

import (
	"errors"
	"fmt"
	"time"

	"github.com/gbtreehole/backend/internal/constants"
	"github.com/gbtreehole/backend/internal/model"
	"gorm.io/gorm"
)

// Review queue persistence errors. Callers distinguish them with errors.Is.
var (
	// ErrReviewNotFound indicates the review item does not exist.
	ErrReviewNotFound = errors.New("review item not found")
	// ErrReviewAlreadyProcessed indicates the item is no longer pending and the
	// requested action cannot take effect (duplicate or concurrent processing).
	ErrReviewAlreadyProcessed = errors.New("review item already processed")
)

type ReviewQueueRepository interface {
	Create(item *model.ReviewQueue) error
	Update(item *model.ReviewQueue) error
	FindByID(id uint) (*model.ReviewQueue, error)
	List(page, pageSize int, status int) ([]model.ReviewQueue, int64, error)
	// ClaimPending atomically transitions a pending item to targetStatus and
	// stamps reviewer/note. Exactly one concurrent caller can win: if the item
	// is missing ErrReviewNotFound is returned; if it has already left the
	// pending state ErrReviewAlreadyProcessed is returned.
	ClaimPending(tx *gorm.DB, id uint, targetStatus, adminID uint, note string) (*model.ReviewQueue, error)
}

type reviewQueueRepository struct {
	db *gorm.DB
}

func NewReviewQueueRepository(db *gorm.DB) ReviewQueueRepository {
	return &reviewQueueRepository{db: db}
}

func (r *reviewQueueRepository) Create(item *model.ReviewQueue) error {
	if err := r.db.Create(item).Error; err != nil {
		return fmt.Errorf("create review queue: %w", err)
	}
	return nil
}

func (r *reviewQueueRepository) Update(item *model.ReviewQueue) error {
	if err := r.db.Save(item).Error; err != nil {
		return fmt.Errorf("update review queue: %w", err)
	}
	return nil
}

func (r *reviewQueueRepository) FindByID(id uint) (*model.ReviewQueue, error) {
	var item model.ReviewQueue
	if err := r.db.First(&item, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrReviewNotFound
		}
		return nil, fmt.Errorf("find review queue by id: %w", err)
	}
	return &item, nil
}

func (r *reviewQueueRepository) List(page, pageSize int, status int) ([]model.ReviewQueue, int64, error) {
	var items []model.ReviewQueue
	var total int64
	q := r.db.Model(&model.ReviewQueue{})
	if status > 0 {
		q = q.Where("status = ?", status)
	}
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count review queue: %w", err)
	}
	if err := q.Order("id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&items).Error; err != nil {
		return nil, 0, fmt.Errorf("list review queue: %w", err)
	}
	return items, total, nil
}

func (r *reviewQueueRepository) ClaimPending(tx *gorm.DB, id uint, targetStatus, adminID uint, note string) (*model.ReviewQueue, error) {
	handle := r.db
	if tx != nil {
		handle = tx
	}
	// A single conditional UPDATE is the atomic claim. The
	// `status = pending` predicate guarantees that only one of any
	// duplicate/concurrent callers can affect the row, so downstream side
	// effects (e.g. restoring the post comment count) never fire twice.
	result := handle.Model(&model.ReviewQueue{}).
		Where("id = ? AND status = ?", id, constants.ReviewStatusPending).
		Updates(map[string]any{
			"status":      targetStatus,
			"reviewed_by": adminID,
			"review_note": note,
			"updated_at":  time.Now(),
		})
	if result.Error != nil {
		return nil, fmt.Errorf("claim review item: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		// Distinguish a missing item from one that has already been processed.
		var existing model.ReviewQueue
		if err := handle.Select("id", "status").First(&existing, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrReviewNotFound
			}
			return nil, fmt.Errorf("reload review item after claim: %w", err)
		}
		return nil, fmt.Errorf("%w: current status %d", ErrReviewAlreadyProcessed, existing.Status)
	}
	var item model.ReviewQueue
	if err := handle.First(&item, id).Error; err != nil {
		return nil, fmt.Errorf("load claimed review item: %w", err)
	}
	return &item, nil
}
