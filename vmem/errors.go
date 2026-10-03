package vmem

import "errors"

var (
	// ErrOutOfBounds 表示访问的虚拟地址区间越界。
	ErrOutOfBounds = errors.New("vmem: access out of bounds")
	// ErrRegionNotFound 表示逻辑区域不存在。
	ErrRegionNotFound = errors.New("vmem: region not found")
	// ErrInvalidConfig 表示配置非法。
	ErrInvalidConfig = errors.New("vmem: invalid config")
	// ErrClosed 表示实例已关闭。
	ErrClosed = errors.New("vmem: closed")
	// ErrBackingUnavailable 表示后备区不可用。
	ErrBackingUnavailable = errors.New("vmem: backing store unavailable")
)
