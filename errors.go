package compliance

import "errors"

var (
	ErrNotImplemented   = errors.New("compliance: not implemented")
	ErrInvalidArgument  = errors.New("compliance: invalid argument")
	ErrPermissionDenied = errors.New("compliance: permission denied")
	ErrPeriodClosed     = errors.New("compliance: period is closed")
	ErrPeriodNotClosed  = errors.New("compliance: period is not closed")
	ErrPeriodClosing    = errors.New("compliance: period close is in progress")
	ErrNotReopened      = errors.New("compliance: period is not reopened")
	ErrDuplicateEntry   = errors.New("compliance: duplicate entry idempotency key")
	ErrNoEffectiveRule  = errors.New("compliance: no effective rule")
	ErrRuleExists       = errors.New("compliance: rule version already exists")
	ErrNotFound         = errors.New("compliance: not found")
	ErrClosed           = errors.New("compliance: service is closed")
)
