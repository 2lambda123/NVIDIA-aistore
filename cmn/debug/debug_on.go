// +build debug

// Package provides debug utilities
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package debug

import (
	"bytes"
	"expvar"
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/NVIDIA/aistore/3rdparty/glog"
)

var (
	xmodules map[uint8]*expvar.Map

	smodules = map[string]uint8{
		"ais":       glog.SmoduleAIS,
		"cluster":   glog.SmoduleCluster,
		"fs":        glog.SmoduleFS,
		"memsys":    glog.SmoduleMemsys,
		"mirror":    glog.SmoduleMirror,
		"reb":       glog.SmoduleReb,
		"transport": glog.SmoduleTransport,
		"ec":        glog.SmoduleEC,
		"stats":     glog.SmoduleStats,
		"ios":       glog.SmoduleIOS,
	}
)

func init() {
	xmodules = make(map[uint8]*expvar.Map, 2)
	loadLogLevel()
}

func NewExpvar(smodule uint8) {
	var smod string
	for k, v := range smodules {
		if v == smodule {
			smod = "ais." + k
			break
		}
	}
	if smod == "" {
		fatalMsg("invalid smodule %d - expecting %+v", smodule, smodules)
	}
	xmodules[smodule] = expvar.NewMap(smod)
}

func SetExpvar(smodule uint8, name string, val int64) {
	m := xmodules[smodule]
	v, ok := m.Get(name).(*expvar.Int)
	if !ok {
		v = new(expvar.Int)
		m.Set(name, v)
	}
	v.Set(val)
}

func Errorln(a ...interface{}) {
	if len(a) == 1 {
		glog.ErrorDepth(1, "[DEBUG] ", a[0])
		return
	}
	Errorf("%v", a)
}

func Errorf(f string, a ...interface{}) {
	glog.ErrorDepth(1, fmt.Sprintf("[DEBUG] "+f, a...))
}

func Infof(f string, a ...interface{}) {
	glog.InfoDepth(1, fmt.Sprintf("[DEBUG] "+f, a...))
}

func Func(f func()) { f() }

func _panic(a ...interface{}) {
	var msg = "DEBUG PANIC: "
	if len(a) > 0 {
		msg += fmt.Sprint(a...)
	}
	buffer := bytes.NewBuffer(make([]byte, 0, 1024))
	fmt.Fprint(buffer, msg)
	for i := 2; i < 9; i++ {
		if _, file, line, ok := runtime.Caller(i); !ok {
			break
		} else {
			if !strings.Contains(file, "aistore") {
				break
			}
			f := filepath.Base(file)
			if buffer.Len() > len(msg) {
				buffer.WriteString(" <- ")
			}
			fmt.Fprintf(buffer, "%s:%d", f, line)
		}
	}
	glog.Errorf("%s", buffer.Bytes())
	glog.Flush()
	panic(msg)
}

func Assert(cond bool, a ...interface{}) {
	if !cond {
		_panic(a...)
	}
}

func AssertFunc(f func() bool, a ...interface{}) {
	if !f() {
		_panic(a...)
	}
}

func AssertMsg(cond bool, msg string) {
	if !cond {
		_panic(msg)
	}
}

func AssertNoErr(err error) {
	if err != nil {
		_panic(err)
	}
}

func Assertf(cond bool, f string, a ...interface{}) {
	if !cond {
		msg := fmt.Sprintf(f, a...)
		_panic(msg)
	}
}

func AssertMutexLocked(m *sync.Mutex) {
	state := reflect.ValueOf(m).Elem().FieldByName("state")
	AssertMsg(state.Int()&1 == 1, "Mutex not Locked")
}

func AssertRWMutexLocked(m *sync.RWMutex) {
	state := reflect.ValueOf(m).Elem().FieldByName("w").FieldByName("state")
	AssertMsg(state.Int()&1 == 1, "RWMutex not Locked")
}

func AssertRWMutexRLocked(m *sync.RWMutex) {
	const maxReaders = 1 << 30 // Taken from `sync/rwmutex.go`.
	rc := reflect.ValueOf(m).Elem().FieldByName("readerCount").Int()
	// NOTE: As it's generally true that `rc > 0` the problem arises when writer
	//  tries to lock the mutex. The writer announces it by manipulating `rc`
	//  (to be specific, decreases `rc` by `maxReaders`). Therefore, to check if
	//  there are any readers still holding lock we need to check both cases.
	AssertMsg(rc > 0 || (0 > rc && rc > -maxReaders), "RWMutex not RLocked")
}

func Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/debug/vars":               expvar.Handler().ServeHTTP,
		"/debug/pprof/":             pprof.Index,
		"/debug/pprof/cmdline":      pprof.Cmdline,
		"/debug/pprof/profile":      pprof.Profile,
		"/debug/pprof/symbol":       pprof.Symbol,
		"/debug/pprof/block":        pprof.Handler("block").ServeHTTP,
		"/debug/pprof/heap":         pprof.Handler("heap").ServeHTTP,
		"/debug/pprof/goroutine":    pprof.Handler("goroutine").ServeHTTP,
		"/debug/pprof/threadcreate": pprof.Handler("threadcreate").ServeHTTP,
	}
}

// loadLogLevel sets debug verbosity for different packages based on
// environment variables. It is to help enable asserts that were originally
// used for testing/initial development and to set the verbosity of glog.
func loadLogLevel() {
	var (
		opts []string
	)

	// Input will be in the format of AIS_DEBUG=transport=4,memsys=3 (same as GODEBUG).
	if val := os.Getenv("AIS_DEBUG"); val != "" {
		opts = strings.Split(val, ",")
	}

	for _, ele := range opts {
		pair := strings.Split(ele, "=")
		if len(pair) != 2 {
			fatalMsg("failed to get module=level element: %q", ele)
		}
		module, level := pair[0], pair[1]
		logModule, exists := smodules[module]
		if !exists {
			fatalMsg("unknown module: %s", module)
		}
		logLvl, err := strconv.Atoi(level)
		if err != nil || logLvl <= 0 {
			fatalMsg("invalid verbosity level=%s, err: %s", level, err)
		}
		glog.SetV(logModule, glog.Level(logLvl))
	}
}

func fatalMsg(f string, v ...interface{}) {
	s := fmt.Sprintf(f, v...)
	if s == "" || s[len(s)-1] != '\n' {
		fmt.Fprintln(os.Stderr, s)
	} else {
		fmt.Fprint(os.Stderr, s)
	}
	os.Exit(1)
}
