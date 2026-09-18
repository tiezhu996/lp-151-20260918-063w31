package repository

import "gorm.io/gorm"

// TxManager 提供跨仓储的数据库事务能力，保证审核状态与目标内容、
// 互动计数等多张表的修改在同一事务内原子提交。
type TxManager interface {
	WithinTransaction(fn func(tx *gorm.DB) error) error
}

type txManager struct {
	db *gorm.DB
}

func NewTxManager(db *gorm.DB) TxManager {
	return &txManager{db: db}
}

func (m *txManager) WithinTransaction(fn func(tx *gorm.DB) error) error {
	return m.db.Transaction(fn)
}
