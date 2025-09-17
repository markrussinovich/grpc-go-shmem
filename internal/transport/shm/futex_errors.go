package shm

import "errors"

// ErrFutexTimeout is returned by futexWaitTimeout when the wait times out.
var ErrFutexTimeout = errors.New("futex timeout")

