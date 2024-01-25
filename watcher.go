//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly
// +build linux darwin netbsd freebsd openbsd dragonfly

// Package gaio is an Async-IO library for Golang.
//
// gaio acts in proactor mode, https://en.wikipedia.org/wiki/Proactor_pattern.
// User submit async IO operations and waits for IO-completion signal.
package gaio

import (
	"container/heap"
	"container/list"
	"io"
	"net"
	"reflect"
	"runtime"
	"sync"
	"syscall"
	"time"
)

var (
	aiocbPool sync.Pool
	emptycb   aiocb
)

func init() {
	aiocbPool.New = func() interface{} {
		return new(aiocb)
	}
}

// fdDesc contains all data structures associated to fd
type fdDesc struct {
	readers list.List // all read/write requests
	writers list.List
	ptr     uintptr // pointer to net.Conn
}

// watcher will monitor events and process async-io request(s),
type watcher struct {
	// poll fd
	pfd *poller

	// netpoll events
	chEventNotify chan pollerEvents

	// events from user
	chPendingNotify chan struct{}

	// IO-completion events to user
	chNotifyCompletion chan struct{}
	hangups            []chan struct{} // blocking delivery will hangup on this
	results            [][]OpResult    // swap buffer for result delivery
	resultsIdx         int
	resultsMutex       sync.Mutex

	// lock for pending io operations
	// aiocb is associated to fd
	pendingCreate     []*aiocb
	pendingProcessing []*aiocb // swaped pending
	pendingMutex      sync.Mutex

	// internal buffer for reading
	swapSize      int // swap buffer capacity
	swapBuffer    [][]byte
	swapBufferIdx int
	bufferOffset  int // bufferOffset for current using one

	// loop related data structure
	descs      map[int]*fdDesc // all descriptors
	connIdents map[uintptr]int // we must not hold net.Conn as key, for GC purpose
	// for timeout operations which
	// aiocb has non-zero deadline, either exists
	// in timeouts & queue at any time
	// or in neither of them.
	timeouts timedHeap
	timer    *time.Timer
	// for garbage collector
	gc       []net.Conn
	gcMutex  sync.Mutex
	gcNotify chan struct{}

	die     chan struct{}
	dieOnce sync.Once
}

// NewWatcher creates a management object for monitoring file descriptors
// with default internal buffer size - 64KB
func NewWatcher() (*Watcher, error) {
	return NewWatcherSize(defaultInternalBufferSize)
}

// NewWatcherSize creates a management object for monitoring file descriptors.
// 'bufsize' sets the internal swap buffer size for Read() with nil, 2 slices with'bufsize'
// will be allocated for performance.
func NewWatcherSize(bufsize int) (*Watcher, error) {
	w := new(watcher)
	pfd, err := openPoll()
	if err != nil {
		return nil, err
	}
	w.pfd = pfd

	// loop related chan
	w.pendingCreate = make([]*aiocb, 0, maxEvents)
	w.pendingProcessing = make([]*aiocb, 0, maxEvents)
	w.chEventNotify = make(chan pollerEvents)
	w.chPendingNotify = make(chan struct{}, 1)
	w.chNotifyCompletion = make(chan struct{}, 1)
	w.die = make(chan struct{})

	// swapBuffer for shared reading
	w.swapSize = bufsize
	w.swapBuffer = make([][]byte, 2)
	for i := 0; i < len(w.swapBuffer); i++ {
		w.swapBuffer[i] = make([]byte, bufsize)
	}

	// results swap for WaitIO
	w.results = make([][]OpResult, 2)

	// init loop related data structures
	w.descs = make(map[int]*fdDesc)
	w.connIdents = make(map[uintptr]int)
	w.gcNotify = make(chan struct{}, 1)
	w.timer = time.NewTimer(0)

	go w.pfd.Wait(w.chEventNotify)
	go w.loop()

	// watcher finalizer for system resources
	wrapper := &Watcher{watcher: w}
	runtime.SetFinalizer(wrapper, func(wrapper *Watcher) {
		wrapper.Close()
	})

	return wrapper, nil
}

// Close stops monitoring on events for all connections
func (w *watcher) Close() (err error) {
	w.dieOnce.Do(func() {
		close(w.die)
		err = w.pfd.Close()
	})
	return err
}

// notify new operations pending
func (w *watcher) notifyPending() {
	select {
	case w.chPendingNotify <- struct{}{}:
	default:
	}
}

// WaitIO blocks until any read/write completion, or error.
// An internal 'buf' returned or 'r []OpResult' are safe to use BEFORE next call to WaitIO().
func (w *watcher) WaitIO() (r []OpResult, err error) {
	for {
		w.resultsMutex.Lock()
		if len(w.results[w.resultsIdx]) > 0 {
			r = w.results[w.resultsIdx]
			w.switchResults()
			for _, hangup := range w.hangups {
				close(hangup)
			}
			w.hangups = nil
			w.resultsMutex.Unlock()
			return r, nil
		}
		w.resultsMutex.Unlock()

		// 0 results, wait for completion signal
		select {
		case <-w.chNotifyCompletion:
		case <-w.die:
			return r, ErrWatcherClosed
		}
	}
}

// changeResult 外部锁
func (w *watcher) switchResults() {
	w.resultsIdx = (w.resultsIdx + 1) % len(w.results)
	results := w.results[w.resultsIdx]
	for i := 0; i < len(results); i++ {
		// avoid memory leak
		results[i].Context = nil
	}
	w.results[w.resultsIdx] = results[:0]
}

// Read submits an async read request on 'fd' with context 'ctx', using buffer 'buf'.
// 'buf' can be set to nil to use internal buffer.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func (w *watcher) Read(ctx interface{}, conn net.Conn, buf []byte) error {
	return w.aioCreate(ctx, OpRead, conn, buf, zeroTime, false)
}

// ReadTimeout submits an async read request on 'fd' with context 'ctx', using buffer 'buf', and
// expects to read some bytes into the buffer before 'deadline'.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func (w *watcher) ReadTimeout(ctx interface{}, conn net.Conn, buf []byte, deadline time.Time) error {
	return w.aioCreate(ctx, OpRead, conn, buf, deadline, false)
}

// ReadFull submits an async read request on 'fd' with context 'ctx', using buffer 'buf', and
// expects to fill the buffer before 'deadline'.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
// 'buf' can't be nil in ReadFull.
func (w *watcher) ReadFull(ctx interface{}, conn net.Conn, buf []byte, deadline time.Time) error {
	if len(buf) == 0 {
		return ErrEmptyBuffer
	}
	return w.aioCreate(ctx, OpRead, conn, buf, deadline, true)
}

// Write submits an async write request on 'fd' with context 'ctx', using buffer 'buf'.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func (w *watcher) Write(ctx interface{}, conn net.Conn, buf []byte) error {
	if len(buf) == 0 {
		return ErrEmptyBuffer
	}
	return w.aioCreate(ctx, OpWrite, conn, buf, zeroTime, false)
}

// WriteTimeout submits an async write request on 'fd' with context 'ctx', using buffer 'buf', and
// expects to complete writing the buffer before 'deadline', 'buf' can be set to nil to use internal buffer.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func (w *watcher) WriteTimeout(ctx interface{}, conn net.Conn, buf []byte, deadline time.Time) error {
	if len(buf) == 0 {
		return ErrEmptyBuffer
	}
	return w.aioCreate(ctx, OpWrite, conn, buf, deadline, false)
}

// Free let the watcher to release resources related to this conn immediately,
// like socket file descriptors.
func (w *watcher) Free(conn net.Conn) error {
	return w.aioCreate(nil, opDelete, conn, nil, zeroTime, false)
}

// core async-io creation
func (w *watcher) aioCreate(ctx interface{}, op OpType, conn net.Conn, buf []byte, deadline time.Time, readfull bool) error {
	select {
	case <-w.die:
		return ErrWatcherClosed
	default:
		var ptr uintptr
		if conn != nil && reflect.TypeOf(conn).Kind() == reflect.Ptr {
			ptr = reflect.ValueOf(conn).Pointer()
		} else {
			return ErrUnsupported
		}

		cb := aiocbPool.Get().(*aiocb)
		*cb = aiocb{op: op, ptr: ptr, ctx: ctx, conn: conn, buffer: buf, deadline: deadline, readFull: readfull}
		w.pendingMutex.Lock()
		w.pendingCreate = append(w.pendingCreate, cb)
		w.pendingMutex.Unlock()

		w.notifyPending()
		return nil
	}
}

// tryRead will try to read data on aiocb and notify
func (w *watcher) tryRead(fd int, pcb *aiocb) bool {
	buf := pcb.buffer

	var useSwap bool
	if buf == nil { // internal buffer
		buf = w.swapBuffer[w.swapBufferIdx][w.bufferOffset:]
		useSwap = true
	}

	for {
		// return values are stored in pcb
		nr, er := syscall.Read(fd, buf[pcb.size:])
		pcb.err = er
		if er == syscall.EAGAIN {
			return false
		}

		// On MacOS we can see EINTR here if the user
		// pressed ^Z.
		if er == syscall.EINTR {
			continue
		}

		// if er is nil, accumulate bytes read
		if er == nil {
			pcb.size += nr
		}

		// proper setting of EOF
		if nr == 0 && er == nil {
			pcb.err = io.EOF
		}

		break
	}

	completed := false
	if pcb.err != nil {
		completed = true
	} else if pcb.size == len(pcb.buffer) {
		completed = true
	} else if !pcb.readFull {
		completed = true
	}

	if completed {
		// IO completed successfully with internal buffer
		if useSwap && pcb.err == nil {
			pcb.buffer = buf[:pcb.size] // set len to pcb.size
			pcb.useSwap = true
			w.bufferOffset += pcb.size

			// current buffer exhausted, notify caller and swap buffer
			if w.bufferOffset == w.swapSize {
				w.swapBufferIdx = (w.swapBufferIdx + 1) % len(w.swapBuffer)
				w.bufferOffset = 0
				pcb.notifyCaller = true
			}
		}
		return true
	}
	return false
}

func (w *watcher) tryWrite(fd int, pcb *aiocb) bool {
	var nw int
	var ew error

	if pcb.buffer != nil {
		for {
			nw, ew = syscall.Write(fd, pcb.buffer[pcb.size:])
			pcb.err = ew
			if ew == syscall.EAGAIN {
				return false
			}

			if ew == syscall.EINTR {
				continue
			}

			// if ew is nil, accumulate bytes written
			if ew == nil {
				pcb.size += nw
			}
			break
		}
	}

	// all bytes written or has error
	// nil buffer still returns
	if pcb.size == len(pcb.buffer) || ew != nil {
		return true
	}
	return false
}

// release connection related resources
func (w *watcher) releaseConn(ident int) {
	if desc, ok := w.descs[ident]; ok {
		// delete from heap
		for e := desc.readers.Front(); e != nil; e = e.Next() {
			tcb := e.Value.(*aiocb)
			if !tcb.deadline.IsZero() {
				heap.Remove(&w.timeouts, tcb.idx)
			}
		}

		for e := desc.writers.Front(); e != nil; e = e.Next() {
			tcb := e.Value.(*aiocb)
			if !tcb.deadline.IsZero() {
				heap.Remove(&w.timeouts, tcb.idx)
			}
		}

		delete(w.descs, ident)
		delete(w.connIdents, desc.ptr)
		// close socket file descriptor duplicated from net.Conn
		syscall.Close(ident)
	}
}

// deliver function will try best to aggregate results for batch delivery
func (w *watcher) deliver(pcb *aiocb) {
	var hangup chan struct{}

	w.resultsMutex.Lock()
	w.results[w.resultsIdx] = append(w.results[w.resultsIdx], OpResult{Operation: pcb.op, Conn: pcb.conn, IsSwapBuffer: pcb.useSwap, Buffer: pcb.buffer, Size: pcb.size, Error: pcb.err, Context: pcb.ctx})
	if pcb.notifyCaller {
		if hangup == nil {
			hangup = make(chan struct{})
			w.hangups = append(w.hangups, hangup)
		}
	}
	w.resultsMutex.Unlock()

	// avoid memory leak
	pcb.ctx = nil

	// trigger event notification
	select {
	case w.chNotifyCompletion <- struct{}{}:
	default:
	}

	// wait on hangup if this is an blocking operation
	if hangup != nil {
		select {
		case <-hangup:
			return
		case <-w.die:
			return
		}
	}
}

// the core event loop of this watcher
func (w *watcher) loop() {
	// defer function to release all resources
	defer func() {
		for ident := range w.descs {
			w.releaseConn(ident)
		}
	}()

	for {
		select {
		case <-w.chPendingNotify:
			// swap w.pending with w.pending2
			w.pendingMutex.Lock()
			w.pendingCreate, w.pendingProcessing = w.pendingProcessing, w.pendingCreate
			w.pendingCreate = dereferenceSliceElem(w.pendingCreate)
			w.pendingMutex.Unlock()
			w.handlePending(w.pendingProcessing)

		case pe := <-w.chEventNotify: // poller events
			w.handleEvents(pe)

		case <-w.timer.C: // timeout heap
			for w.timeouts.Len() > 0 {
				now := time.Now()
				pcb := w.timeouts[0]
				if now.After(pcb.deadline) {
					// remove from list & heap together
					pcb.l.Remove(pcb.elem)
					heap.Pop(&w.timeouts)
					// ErrDeadline
					pcb.err = ErrDeadline
					w.deliver(pcb)
				} else {
					w.timer.Reset(pcb.deadline.Sub(now))
					break
				}
			}

		case <-w.gcNotify: // gc recycled net.Conn
			w.gcMutex.Lock()
			for i, c := range w.gc {
				ptr := reflect.ValueOf(c).Pointer()
				if ident, ok := w.connIdents[ptr]; ok {
					// since it's gc-ed, queue is impossible to hold net.Conn
					// we don't have to send to chIOCompletion,just release here
					w.releaseConn(ident)
				}
				w.gc[i] = nil
			}
			w.gc = w.gc[:0]
			w.gcMutex.Unlock()

		case <-w.die:
			return
		}
	}
}

// for loop handling pending requests
func (w *watcher) handlePending(pending []*aiocb) {
	for _, pcb := range pending {
		ident, ok := w.connIdents[pcb.ptr]
		// resource releasing operation
		if pcb.op == opDelete && ok {
			w.releaseConn(ident)
			continue
		}

		// handling new connection
		var desc *fdDesc
		if ok {
			desc = w.descs[ident]
		} else {
			if dupfd, err := dupconn(pcb.conn); err != nil {
				pcb.err = err
				w.deliver(pcb)
				continue
			} else {
				// as we duplicated successfully, we're safe to
				// close the original connection
				pcb.conn.Close()
				// assign idents
				ident = dupfd

				// unexpected situation, should notify caller if we cannot dup(2)
				werr := w.pfd.Watch(ident)
				if werr != nil {
					pcb.err = werr
					w.deliver(pcb)
					continue
				}

				// file description bindings
				desc = &fdDesc{ptr: pcb.ptr}
				w.descs[ident] = desc
				w.connIdents[pcb.ptr] = ident

				// the conn is still useful for GC finalizer.
				// note finalizer function cannot hold reference to net.Conn,
				// if not it will never be GC-ed.
				runtime.SetFinalizer(pcb.conn, func(c net.Conn) {
					w.gcMutex.Lock()
					w.gc = append(w.gc, c)
					w.gcMutex.Unlock()

					// notify gc processor
					select {
					case w.gcNotify <- struct{}{}:
					default:
					}
				})
			}
		}

		// operations splitted into different buckets
		switch pcb.op {
		case OpRead:
			// try immediately queue is empty
			if desc.readers.Len() == 0 {
				if w.tryRead(ident, pcb) {
					w.deliver(pcb)
					continue
				}
			}
			// enqueue for poller events
			pcb.l = &desc.readers
			pcb.elem = pcb.l.PushBack(pcb)
		case OpWrite:
			if desc.writers.Len() == 0 {
				if w.tryWrite(ident, pcb) {
					w.deliver(pcb)
					continue
				}
			}
			pcb.l = &desc.writers
			pcb.elem = pcb.l.PushBack(pcb)
		}

		// push to heap for timeout operation
		if !pcb.deadline.IsZero() {
			heap.Push(&w.timeouts, pcb)
			if w.timeouts.Len() == 1 {
				w.timer.Reset(pcb.deadline.Sub(time.Now()))
			}
		}
	}
}

// handle poller events
func (w *watcher) handleEvents(pe pollerEvents) {
	// suppose fd(s) being polled is closed by conn.Close() from outside after chanrecv,
	// and a new conn has re-opened with the same handler number(fd). The read and write
	// on this fd is fatal.
	//
	// Note poller will remove closed fd automatically epoll(7), kqueue(2) and silently.
	// To solve this problem watcher will dup() a new fd from net.Conn, which uniquely
	// identified by 'e.ident', all library operation will be based on 'e.ident',
	// then IO operation is impossible to misread or miswrite on re-created fd.
	//log.Println(e)
	for _, e := range pe {
		if desc, ok := w.descs[e.ident]; ok {
			if e.ev&EV_READ != 0 {
				var next *list.Element
				for elem := desc.readers.Front(); elem != nil; elem = next {
					next = elem.Next()
					pcb := elem.Value.(*aiocb)
					if w.tryRead(e.ident, pcb) {
						w.deliver(pcb)
						desc.readers.Remove(elem)
						if !pcb.deadline.IsZero() {
							heap.Remove(&w.timeouts, pcb.idx)
						}
						// recycle
						aiocbPool.Put(pcb)
					} else {
						break
					}
				}
			}

			if e.ev&EV_WRITE != 0 {
				var next *list.Element
				for elem := desc.writers.Front(); elem != nil; elem = next {
					next = elem.Next()
					pcb := elem.Value.(*aiocb)
					if w.tryWrite(e.ident, pcb) {
						w.deliver(pcb)
						desc.writers.Remove(elem)
						if !pcb.deadline.IsZero() {
							heap.Remove(&w.timeouts, pcb.idx)
						}
						// recycle
						aiocbPool.Put(pcb)
					} else {
						break
					}
				}
			}
		}
	}
}

func dereferenceSliceElem[T any](sliceList []*T) []*T {
	for i := 0; i < len(sliceList); i++ {
		sliceList[i] = nil
	}
	return sliceList[:0]
}
