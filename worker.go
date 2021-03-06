package v8worker

/*
#cgo CXXFLAGS: -std=c++11
#cgo pkg-config: v8.pc
#include <stdlib.h>
#include "binding.h"
*/
import "C"
import (
	"errors"
	"runtime"
	"strconv"
	"sync"
	"unsafe"
)

var (
	// Don't init V8 more than once.
	initV8Once sync.Once

	scriptSequence         int
	scriptSequenceLocker   sync.Mutex
	workerIdSequence       int
	workerIdSequenceLocker sync.Mutex
	callbacksMapLocker     sync.RWMutex
	callbacksMap           = make(map[int]*callbacks)
)

// To receive messages from javascript...
type ReceiveMessageCallback func(msg string)

// To send a message from javascript and synchronously return a string.
type ReceiveSyncMessageCallback func(msg string) string

// This is a golang wrapper around a single V8 Isolate.
type Worker struct {
	cWorker *C.worker
}

// This is a wrapper for worker callbacks
type callbacks struct {
	cb     ReceiveMessageCallback
	syncCB ReceiveSyncMessageCallback
}

// ScriptOrigin represents V8 class – see http://v8.paulfryzel.com/docs/master/classv8_1_1_script_origin.html
type ScriptOrigin struct {
	ScriptName            string
	LineOffset            int32
	ColumnOffset          int32
	IsSharedCrossOrigin   bool
	ScriptId              int32
	IsEmbedderDebugScript bool
	SourceMapURL          string
	IsOpaque              bool
}

// HeapStatistics represents V8 class - see http://v8.paulfryzel.com/docs/master/classv8_1_1_heap_statistics.html
type HeapStatistics struct {
	TotalHeapSize           int
	TotalHeapSizeExecutable int
	TotalPhysicalSize       int
	TotalAvailableSize      int
	UsedHeapSize            int
	HeapSizeLimit           int
	MallocedMemory          int
	DoesZapGarbage          int
}

// Version return the V8 version E.G. "4.3.59"
func Version() string {
	return C.GoString(C.worker_version())
}

//export recvCb
func recvCb(msg_s *C.char, workerId int) {
	msg := C.GoString(msg_s)
	callbacksMapLocker.RLock()
	fn := callbacksMap[workerId].cb
	callbacksMapLocker.RUnlock()
	fn(msg)
}

//export recvSyncCb
func recvSyncCb(msg_s *C.char, workerId int) *C.char {
	msg := C.GoString(msg_s)
	callbacksMapLocker.RLock()
	fn := callbacksMap[workerId].syncCB
	callbacksMapLocker.RUnlock()
	res := fn(msg)
	return C.CString(res)
}

// New creates a new worker, which corresponds to a V8 isolate. A single threaded
// standalone execution context.
func New(cb ReceiveMessageCallback, syncCB ReceiveSyncMessageCallback) *Worker {
	id := nextWorkerId()

	cbWrapper := &callbacks{
		cb:     cb,
		syncCB: syncCB,
	}
	callbacksMapLocker.Lock()
	callbacksMap[id] = cbWrapper
	callbacksMapLocker.Unlock()

	initV8Once.Do(func() {
		C.v8_init()
	})

	worker := &Worker{}
	worker.cWorker = C.worker_new(C.int(id))
	runtime.SetFinalizer(worker, func(final_worker *Worker) {
		C.worker_dispose(final_worker.cWorker)
		callbacksMapLocker.Lock()
		delete(callbacksMap, id)
		callbacksMapLocker.Unlock()
	})
	return worker
}

// Optional notification that the embedder is idle.
// http://v8.paulfryzel.com/docs/master/classv8_1_1_isolate.html#aba794ed25d4fa8780b3a07c66a5e5d4a
func (w *Worker) IdleNotificationDeadline(deadLineInSeconds float64) bool {
	return bool(C.worker_idle_notification_deadline(w.cWorker, C.double(deadLineInSeconds)))
}

// Load loads and executes a javascript file with the filename specified by
// scriptName and the contents of the file specified by the param code.
func (w *Worker) Load(scriptName string, code string) error {
	return w.LoadWithOptions(&ScriptOrigin{ScriptName: scriptName}, code)
}

// GetHeapStatistics returns statistics about the V8 isolate heap memory usage
func (w *Worker) GetHeapStatistics() *HeapStatistics {
	hs := C.struct_heap_statistics_s{}
	C.worker_get_heap_statistics(w.cWorker, &hs)
	return &HeapStatistics{
		TotalHeapSize:           int(hs.total_heap_size),
		TotalHeapSizeExecutable: int(hs.total_heap_size_executable),
		TotalPhysicalSize:       int(hs.total_physical_size),
		TotalAvailableSize:      int(hs.total_available_size),
		UsedHeapSize:            int(hs.used_heap_size),
		HeapSizeLimit:           int(hs.heap_size_limit),
		MallocedMemory:          int(hs.malloced_memory),
		DoesZapGarbage:          int(hs.does_zap_garbage),
	}
}

// LoadWithOptions loads and executes a javascript file with the ScriptOrigin specified by
// origin and the contents of the file specified by the param code.
func (w *Worker) LoadWithOptions(origin *ScriptOrigin, code string) error {
	cCode := C.CString(code)

	if origin == nil {
		origin = new(ScriptOrigin)
	}
	if origin.ScriptName == "" {
		origin.ScriptName = nextScriptName()
	}
	cScriptName := C.CString(origin.ScriptName)
	cLineOffset := C.int(origin.LineOffset)
	cColumnOffset := C.int(origin.ColumnOffset)
	cIsSharedCrossOrigin := C.bool(origin.IsSharedCrossOrigin)
	cScriptId := C.int(origin.ScriptId)
	cIsEmbedderDebugScript := C.bool(origin.IsEmbedderDebugScript)
	cSourceMapURL := C.CString(origin.SourceMapURL)
	cIsOpaque := C.bool(origin.IsOpaque)

	defer C.free(unsafe.Pointer(cScriptName))
	defer C.free(unsafe.Pointer(cCode))
	defer C.free(unsafe.Pointer(cSourceMapURL))

	r := C.worker_load(w.cWorker, cCode, cScriptName, cLineOffset, cColumnOffset, cIsSharedCrossOrigin, cScriptId, cIsEmbedderDebugScript, cSourceMapURL, cIsOpaque)
	if r != 0 {
		errStr := C.worker_last_exception(w.cWorker)
		return errors.New(C.GoString(errStr))
	}
	return nil
}

// LowMemoryNotification for optional notification that the system is running low on memory.
// V8 uses these notifications to attempt to free memory.
// http://v8.paulfryzel.com/docs/master/classv8_1_1_isolate.html#aaf446f4877e4707a93d2c406fffd9fd6
func (w *Worker) LowMemoryNotification() {
	C.worker_low_memory_notification(w.cWorker)
}

// Send sends a message to a worker. The $recv callback in js will be called.
func (w *Worker) Send(msg string) error {
	msg_s := C.CString(string(msg))
	defer C.free(unsafe.Pointer(msg_s))

	r := C.worker_send(w.cWorker, msg_s)
	if r != 0 {
		errStr := C.worker_last_exception(w.cWorker)
		return errors.New(C.GoString(errStr))
	}

	return nil
}

// SendSync sends a message to a worker. The $recvSync callback in js will be called.
// That callback will return a string which is passed to golang and used as the return value of SendSync.
func (w *Worker) SendSync(msg string) string {
	msg_s := C.CString(string(msg))
	defer C.free(unsafe.Pointer(msg_s))

	svalue := C.worker_send_sync(w.cWorker, msg_s)
	return C.GoString(svalue)
}

// TerminateExecution terminates execution of javascript
func (w *Worker) TerminateExecution() {
	C.worker_terminate_execution(w.cWorker)
}

func nextWorkerId() int {
	workerIdSequenceLocker.Lock()
	seq := workerIdSequence
	workerIdSequence++
	workerIdSequenceLocker.Unlock()
	return seq
}

func nextScriptName() string {
	scriptSequenceLocker.Lock()
	seq := scriptSequence
	scriptSequence++
	scriptSequenceLocker.Unlock()
	return "VM" + strconv.Itoa(seq)
}
