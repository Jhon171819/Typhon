package typhon

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	valueSlotBytes = 160 // logical weight, independent of Go's layout
	defaultWork    = 10_000_000
	defaultSQLRows = 100_000
	maxQueuedHTTP  = 256
)

type quotaLimits struct {
	engineBytes, requestBytes int64
	work, sqlRows, queue      int64
	liveness                  string
}

func configuredQuotas() (quotaLimits, error) {
	limits := quotaLimits{engineBytes: 256 << 20, requestBytes: 64 << 20, work: defaultWork, sqlRows: defaultSQLRows, queue: maxQueuedHTTP}
	for _, option := range []struct {
		name     string
		target   *int64
		min, max int64
	}{
		{"TYPHON_LIMIT_ENGINE_BYTES", &limits.engineBytes, 1024, 512 << 20},
		{"TYPHON_LIMIT_REQUEST_BYTES", &limits.requestBytes, 1024, 256 << 20},
		{"TYPHON_LIMIT_INSTRUCTIONS", &limits.work, 1, 50_000_000},
		{"TYPHON_LIMIT_SQL_ROWS", &limits.sqlRows, 1, defaultSQLRows},
		{"TYPHON_LIMIT_HTTP_QUEUE", &limits.queue, 0, maxQueuedHTTP},
	} {
		text, exists := os.LookupEnv(option.name)
		if !exists {
			continue
		}
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil || n < option.min || n > option.max {
			return limits, fmt.Errorf("%s must be between %d and %d", option.name, option.min, option.max)
		}
		*option.target = n
	}
	limits.liveness = os.Getenv("TYPHON_LIVENESS_PATH")
	if limits.liveness != "" && (!strings.HasPrefix(limits.liveness, "/") || strings.ContainsAny(limits.liveness, "\t\r\n ?#")) {
		return limits, fmt.Errorf("TYPHON_LIVENESS_PATH must be an absolute HTTP path")
	}
	return limits, nil
}

// QuotaError is the structured cause of a stopped execution or request.
type QuotaError struct {
	Resource string
	Limit    int64
}

func (e *QuotaError) Error() string {
	return fmt.Sprintf("%s budget exceeded (limit %d)", e.Resource, e.Limit)
}

func quotaStop(resource string, limit int64) error {
	return &executionStop{cause: &QuotaError{Resource: resource, Limit: limit}}
}

type executionBudget struct {
	bytes, work, rows              int64
	byteLimit, workLimit, rowLimit int64
}

func (vm *VM) executionBudget() *executionBudget {
	if vm.budget == nil {
		limits := vm.engine.quotas
		byteLimit := limits.engineBytes
		if vm.request != nil {
			byteLimit = limits.requestBytes
		}
		vm.budget = &executionBudget{work: int64(vm.instructions), byteLimit: byteLimit, workLimit: limits.work, rowLimit: limits.sqlRows}
	}
	return vm.budget
}

func (vm *VM) chargeWork() error {
	if vm.cleanupCtx != nil {
		if vm.cleanupSteps <= 0 {
			return &executionStop{cause: errors.New("cancellation finally instruction budget exceeded")}
		}
		vm.cleanupSteps--
		return nil
	}
	b := vm.executionBudget()
	if b.work >= b.workLimit {
		return quotaStop("instruction", b.workLimit)
	}
	b.work++
	return nil
}

func (vm *VM) chargeBytes(n int64) error {
	if vm == nil {
		return nil
	}
	if err := vm.executionError(); err != nil {
		return err
	}
	if vm.cleanupCtx != nil {
		if n > vm.cleanupBytes {
			return quotaStop("cancellation cleanup byte", 1<<20)
		}
		vm.cleanupBytes -= n
		return nil
	}
	return vm.executionBudget().chargeBytes(n)
}

func (b *executionBudget) chargeBytes(n int64) error {
	if n > b.byteLimit-b.bytes {
		return quotaStop("byte", b.byteLimit)
	}
	b.bytes += n
	return nil
}

func (vm *VM) chargeSQLRow() error {
	b := vm.executionBudget()
	if b.rows >= b.rowLimit {
		return quotaStop("SQL row", b.rowLimit)
	}
	b.rows++
	return vm.chargeWork()
}

func (vm *VM) chargeValue(value Value, depth int) error {
	return vm.chargeValuePath(value, depth, nil)
}

func (vm *VM) chargeValuePath(value Value, depth int, active map[any]bool) error {
	if depth > maxJSONDepth {
		return quotaStop("value depth", maxJSONDepth)
	}
	if err := vm.chargeBytes(valueSlotBytes + int64(len(value.Str))); err != nil {
		return err
	}
	switch value.Kind {
	case ValueList:
		if value.List != nil && !active[value.List] {
			if active == nil {
				active = make(map[any]bool)
			}
			active[value.List] = true
			defer delete(active, value.List)
			for _, item := range *value.List {
				if err := vm.chargeValuePath(item, depth+1, active); err != nil {
					return err
				}
			}
		}
	case ValueDict:
		if value.Dict != nil && !active[value.Dict] {
			if active == nil {
				active = make(map[any]bool)
			}
			active[value.Dict] = true
			defer delete(active, value.Dict)
			for _, entry := range value.Dict.Entries {
				if err := vm.chargeValuePath(entry.Key, depth+1, active); err != nil {
					return err
				}
				if err := vm.chargeValuePath(entry.Value, depth+1, active); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
