package repository

import (
	"errors"
	"fmt"

	"github.com/gbtreehole/backend/internal/constants"
	"github.com/gbtreehole/backend/internal/model"
	"gorm.io/gorm"
)

// ErrReviewAlreadyProcessed 审核项已被处理（重复提交或并发竞争失败）。
var ErrReviewAlreadyProcessed = errors.New("review item already processed")

type ReviewQueueRepository interface {
	Create(item *model.ReviewQueue) error
	Update(item *model.ReviewQueue) error
	FindByID(id uint) (*model.ReviewQueue, error)
	List(page, pageSize int, status int) ([]model.ReviewQueue, int64, error)
	// MarkProcessed 仅在审核项仍处于待审状态时原子写入终态并返回加锁后的审核项。
	// 审核项不存在返回 ErrNotFound；已被处理（重复/并发）返回 ErrReviewAlreadyProcessed。
	MarkProcessed(id uint, adminID uint, status int, note string) (*model.ReviewQueue, error)
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
			return nil, ErrNotFound
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

// MarkProcessed 使用条件更新（compare-and-swap）保证同一审核项只生效一次：
// UPDATE 仅命中 status=待审 的行，数据库行锁会让并发事务在该行上排队，
// 只有第一个事务能把待审改写为终态，其余事务 RowsAffected=0 判定为冲突。
func (r *reviewQueueRepository) MarkProcessed(id uint, adminID uint, status int, note string) (*model.ReviewQueue, error) {
	var item model.ReviewQueue
	if err := r.db.First(&item, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("find review queue by id: %w", err)
	}
	result := r.db.Model(&model.ReviewQueue{}).
		Where("id = ? AND status = ?", id, constants.ReviewStatusPending).
		Updates(map[string]any{
			"status":      status,
			"reviewed_by": adminID,
			"review_note": note,
		})
	if result.Error != nil {
		return nil, fmt.Errorf("mark review processed: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil, ErrReviewAlreadyProcessed
	}
	item.Status = status
	item.ReviewedBy = &adminID
	item.ReviewNote = note
	return &item, nil
}
