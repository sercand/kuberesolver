package kuberesolver

import (
	"fmt"
	"runtime"
	"time"

	"github.com/golang/glog"
)

func Until(f func(), period time.Duration, stopCh <-chan struct{}) {
	select {
	case <-stopCh:
		return
	default:
	}
	for {
		func() {
			defer HandleCrash()
			f()
		}()
		select {
		case <-stopCh:
			return
		case <-time.After(period):
		}
	}
}

// HandleCrash simply catches a crash and logs an error. Meant to be called via defer.
// Additional context-specific handlers can be provided, and will be called in case of panic
func HandleCrash(additionalHandlers ...func(interface{})) {
	if r := recover(); r != nil {
		logPanic(r)
	}
}

// logPanic logs the caller tree when a panic occurs.
func logPanic(r interface{}) {
	callers := ""
	for i := 0; true; i++ {
		_, file, line, ok := runtime.Caller(i)
		if !ok {
			break
		}
		callers = callers + fmt.Sprintf("%v:%v\n", file, line)
	}
	glog.Errorf("Recovered from panic: %#v (%v)\n%v", r, r, callers)
}
