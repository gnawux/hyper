package hlog

import (
	"fmt"

	"github.com/golang/glog"
)

type LogLevel int32
type LogOwner interface {
	LogPrefix() string
}

const (
	TRACE LogLevel = iota
	DEBUG
	INFO
	WARNING
	ERROR
)

func Log(level LogLevel, format string, args ...interface{}) {
	HLog(level, nil, 1, format, args...)
}

func HLog(level LogLevel, owner interface{}, depth int, args ...interface{}) {
	l := getLogger(level)
	if l == nil {
		return
	}
	prefix := getPrefix(owner)
	if len(args) > 1 {
		format, ok := args[0].(string)
		if ok {
			format = fmt.Sprintf(format, args[1:]...)
			l(depth+1, prefix, format)
			return
		}
	}
	l(depth+1, prefix, args...)
}

type logFunc func(int, ...interface{})

func getLogger(level LogLevel) logFunc {
	switch level {
	case ERROR:
		return glog.ErrorDepth
	case WARNING:
		return glog.WarningDepth
	case INFO:
		return glog.InfoDepth
	case DEBUG:
		if glog.V(1) {
			return glog.InfoDepth
		}
		return nil
	case TRACE:
		if glog.V(3) {
			return glog.InfoDepth
		}
		return nil
	default:
		return nil
	}
}

func getPrefix(o interface{}) string {
	if o == nil {
		return ""
	}
	if lo, ok := o.(LogOwner); ok {
		return lo.LogPrefix()
	}
	return ""
}
