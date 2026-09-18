package repository

import (
	"fmt"

	"gorm.io/gorm"
)

// TxFunc is a unit of work executed inside a single database transaction.
// All repositories used inside fn must be built from the tx handle passed in,
// so that every write participates in the same transaction.
type TxFunc func(tx *gorm.DB) error

// TxManager runs a function atomically inside a database transaction.
type TxManager interface {
	WithinTransaction(fn TxFunc) error
}

type txManager struct {
	db *gorm.DB
}

// NewTxManager builds a TxManager bound to the application database handle.
func NewTxManager(db *gorm.DB) TxManager {
	return &txManager{db: db}
}

func (m *txManager) WithinTransaction(fn TxFunc) (err error) {
	if fn == nil {
		return fmt.Errorf("transaction: nil function")
	}
	tx := m.db.Begin()
	if tx.Error != nil {
		return fmt.Errorf("begin transaction: %w", tx.Error)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback().Error
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback().Error
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit().Error; err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}
