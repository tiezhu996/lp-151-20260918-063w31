package repository

import (
	"errors"
	"fmt"
	"time"

	"github.com/gbtreehole/backend/internal/model"
	"gorm.io/gorm"
)

type CommentRepository interface {
	Create(comment *model.Comment) error
	Update(comment *model.Comment) error
	FindByID(id uint) (*model.Comment, error)
	ListByPostID(postID uint, page, pageSize int, status int) ([]model.Comment, int64, error)
	ListByIDs(ids []uint) ([]model.Comment, error)
	// TransitionStatus atomically moves a comment from wantStatus to toStatus
	// and bumps updated_at inside the given transaction. Returns
	// ErrTargetMissing when the comment is absent and ErrTargetStateConflict
	// when its current status is not wantStatus.
	TransitionStatus(tx *gorm.DB, id uint, wantStatus, toStatus int) error
}

type commentRepository struct {
	db *gorm.DB
}

func NewCommentRepository(db *gorm.DB) CommentRepository {
	return &commentRepository{db: db}
}

func (r *commentRepository) Create(comment *model.Comment) error {
	if err := r.db.Create(comment).Error; err != nil {
		return fmt.Errorf("create comment: %w", err)
	}
	return nil
}

func (r *commentRepository) Update(comment *model.Comment) error {
	if err := r.db.Save(comment).Error; err != nil {
		return fmt.Errorf("update comment: %w", err)
	}
	return nil
}

func (r *commentRepository) FindByID(id uint) (*model.Comment, error) {
	var comment model.Comment
	if err := r.db.Preload("Identity").First(&comment, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("find comment by id: %w", err)
	}
	return &comment, nil
}

func (r *commentRepository) ListByPostID(postID uint, page, pageSize int, status int) ([]model.Comment, int64, error) {
	var comments []model.Comment
	var total int64
	q := r.db.Model(&model.Comment{}).Preload("Identity").Where("post_id = ?", postID)
	if status > 0 {
		q = q.Where("status = ?", status)
	}
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count comments: %w", err)
	}
	if err := q.Order("id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&comments).Error; err != nil {
		return nil, 0, fmt.Errorf("list comments: %w", err)
	}
	return comments, total, nil
}

func (r *commentRepository) ListByIDs(ids []uint) ([]model.Comment, error) {
	var comments []model.Comment
	if len(ids) == 0 {
		return comments, nil
	}
	if err := r.db.Preload("Identity").Where("id IN ?", ids).Find(&comments).Error; err != nil {
		return nil, fmt.Errorf("list comments by ids: %w", err)
	}
	return comments, nil
}

func (r *commentRepository) TransitionStatus(tx *gorm.DB, id uint, wantStatus, toStatus int) error {
	handle := r.db
	if tx != nil {
		handle = tx
	}
	result := handle.Model(&model.Comment{}).
		Where("id = ? AND status = ?", id, wantStatus).
		Updates(map[string]any{"status": toStatus, "updated_at": time.Now()})
	if result.Error != nil {
		return fmt.Errorf("transition comment status: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := handle.Model(&model.Comment{}).Where("id = ?", id).Count(&count).Error; err != nil {
			return fmt.Errorf("check comment after status transition: %w", err)
		}
		if count == 0 {
			return ErrTargetMissing
		}
		return fmt.Errorf("%w: comment %d is not in status %d", ErrTargetStateConflict, id, wantStatus)
	}
	return nil
}
