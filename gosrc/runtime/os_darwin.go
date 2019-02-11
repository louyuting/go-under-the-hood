// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

type mOS struct {
	initialized bool
	mutex       pthreadmutex
	cond        pthreadcond
	count       int
}

func unimplemented(name string) {
	println(name, "not implemented")
	*(*int)(unsafe.Pointer(uintptr(1231))) = 1231
}

//go:nosplit
func semacreate(mp *m) {
	if mp.initialized {
		return
	}
	mp.initialized = true
	if err := pthread_mutex_init(&mp.mutex, nil); err != 0 {
		throw("pthread_mutex_init")
	}
	if err := pthread_cond_init(&mp.cond, nil); err != 0 {
		throw("pthread_cond_init")
	}
}

//go:nosplit
func semasleep(ns int64) int32 {
	var start int64
	if ns >= 0 {
		start = nanotime()
	}
	mp := getg().m
	// 在持有 P 的情况下对 M 加锁
	pthread_mutex_lock(&mp.mutex)
	for {
		if mp.count > 0 {
			mp.count--
			pthread_mutex_unlock(&mp.mutex)
			return 0
		}
		if ns >= 0 {
			spent := nanotime() - start
			if spent >= ns {
				pthread_mutex_unlock(&mp.mutex)
				return -1
			}
			var t timespec
			t.set_nsec(ns - spent)
			err := pthread_cond_timedwait_relative_np(&mp.cond, &mp.mutex, &t)
			if err == _ETIMEDOUT {
				pthread_mutex_unlock(&mp.mutex)
				return -1
			}
		} else {
			pthread_cond_wait(&mp.cond, &mp.mutex)
		}
	}
}

//go:nosplit
func semawakeup(mp *m) {
	// 唤醒 M 线程
	pthread_mutex_lock(&mp.mutex)
	mp.count++
	if mp.count > 0 {
		pthread_cond_signal(&mp.cond)
	}
	pthread_mutex_unlock(&mp.mutex)
}

// BSD 接口作线程操作
func osinit() {
	// pthread_create 推迟到 goenvs 末尾，从而可以先检查环境

	ncpu = getncpu()
	physPageSize = getPageSize()
}

const (
	_CTL_HW      = 6
	_HW_NCPU     = 3
	_HW_PAGESIZE = 7
)

func getncpu() int32 {
	// Use sysctl to fetch hw.ncpu.
	mib := [2]uint32{_CTL_HW, _HW_NCPU}
	out := uint32(0)
	nout := unsafe.Sizeof(out)
	ret := sysctl(&mib[0], 2, (*byte)(unsafe.Pointer(&out)), &nout, nil, 0)
	if ret >= 0 && int32(out) > 0 {
		return int32(out)
	}
	return 1
}

func getPageSize() uintptr {
	// 使用 sysctl 来获取 hw.pagesize.
	mib := [2]uint32{_CTL_HW, _HW_PAGESIZE}
	out := uint32(0)
	nout := unsafe.Sizeof(out)
	ret := sysctl(&mib[0], 2, (*byte)(unsafe.Pointer(&out)), &nout, nil, 0)
	if ret >= 0 && int32(out) > 0 {
		return uintptr(out)
	}
	return 0
}

var urandom_dev = []byte("/dev/urandom\x00")

//go:nosplit
func getRandomData(r []byte) {
	fd := open(&urandom_dev[0], 0 /* O_RDONLY */, 0)
	n := read(fd, unsafe.Pointer(&r[0]), int32(len(r)))
	closefd(fd)
	extendRandom(r, int(n))
}

func goenvs() {
	goenvs_unix()
}

// 可能在 m.p==nil 情况下运行，因此不允许 write barrier
//go:nowritebarrierrec
func newosproc(mp *m) {
	stk := unsafe.Pointer(mp.g0.stack.hi)
	if false {
		print("newosproc stk=", stk, " m=", mp, " g=", mp.g0, " id=", mp.id, " ostk=", &mp, "\n")
	}

	// 初始化 attribute 对象
	var attr pthreadattr
	var err int32
	err = pthread_attr_init(&attr)
	if err != 0 {
		write(2, unsafe.Pointer(&failthreadcreate[0]), int32(len(failthreadcreate)))
		exit(1)
	}

	// 设置想要使用的栈大小。目前为 64KB
	// TODO: just use OS default size?
	const stackSize = 1 << 16
	if pthread_attr_setstacksize(&attr, stackSize) != 0 {
		write(2, unsafe.Pointer(&failthreadcreate[0]), int32(len(failthreadcreate)))
		exit(1)
	}
	//mSysStatInc(&memstats.stacks_sys, stackSize) //TODO: do this?

	// 通知 pthread 库不会 join 这个线程。
	if pthread_attr_setdetachstate(&attr, _PTHREAD_CREATE_DETACHED) != 0 {
		write(2, unsafe.Pointer(&failthreadcreate[0]), int32(len(failthreadcreate)))
		exit(1)
	}

	// Finally, create the thread. It starts at mstart_stub, which does some low-level
	// setup and then calls mstart.
	var oset sigset
	sigprocmask(_SIG_SETMASK, &sigset_all, &oset)
	err = pthread_create(&attr, funcPC(mstart_stub), unsafe.Pointer(mp))
	sigprocmask(_SIG_SETMASK, &oset, nil)
	if err != 0 {
		write(2, unsafe.Pointer(&failthreadcreate[0]), int32(len(failthreadcreate)))
		exit(1)
	}
}

// glue code to call mstart from pthread_create.
func mstart_stub()

// newosproc0 is a version of newosproc that can be called before the runtime
// is initialized.
//
// This function is not safe to use after initialization as it does not pass an M as fnarg.
//
//go:nosplit
func newosproc0(stacksize uintptr, fn uintptr) {
	// Initialize an attribute object.
	var attr pthreadattr
	var err int32
	err = pthread_attr_init(&attr)
	if err != 0 {
		write(2, unsafe.Pointer(&failthreadcreate[0]), int32(len(failthreadcreate)))
		exit(1)
	}

	// Set the stack we want to use.
	if pthread_attr_setstacksize(&attr, stacksize) != 0 {
		write(2, unsafe.Pointer(&failthreadcreate[0]), int32(len(failthreadcreate)))
		exit(1)
	}
	mSysStatInc(&memstats.stacks_sys, stacksize)

	// Tell the pthread library we won't join with this thread.
	if pthread_attr_setdetachstate(&attr, _PTHREAD_CREATE_DETACHED) != 0 {
		write(2, unsafe.Pointer(&failthreadcreate[0]), int32(len(failthreadcreate)))
		exit(1)
	}

	// Finally, create the thread. It starts at mstart_stub, which does some low-level
	// setup and then calls mstart.
	var oset sigset
	sigprocmask(_SIG_SETMASK, &sigset_all, &oset)
	err = pthread_create(&attr, fn, nil)
	sigprocmask(_SIG_SETMASK, &oset, nil)
	if err != 0 {
		write(2, unsafe.Pointer(&failthreadcreate[0]), int32(len(failthreadcreate)))
		exit(1)
	}
}

var failallocatestack = []byte("runtime: failed to allocate stack for the new OS thread\n")
var failthreadcreate = []byte("runtime: failed to create new OS thread\n")

// Called to do synchronous initialization of Go code built with
// -buildmode=c-archive or -buildmode=c-shared.
// None of the Go runtime is initialized.
//go:nosplit
//go:nowritebarrierrec
func libpreinit() {
	initsig(true)
}

// 调用此方法来初始化一个新的 m (包含引导 m)
// 从一个父线程上进行调用（引导时为主线程），可以分配内存
func mpreinit(mp *m) {
	mp.gsignal = malg(32 * 1024) // OS X 需要 >= 8K，此处创建处理 singnal 的 g
	mp.gsignal.m = mp            // 指定 gsignal 拥有的 m
}

// 初始化一个新的 m （包括引导阶段的 m）
// 在一个新的线程上调用，不分配内存
func minit() {
	// The alternate signal stack is buggy on arm and arm64.
	// The signal handler handles it directly.
	if GOARCH != "arm" && GOARCH != "arm64" {
		minitSignalStack()
	}
	minitSignalMask()
}

// Called from dropm to undo the effect of an minit.
//go:nosplit
func unminit() {
	// The alternate signal stack is buggy on arm and arm64.
	// See minit.
	if GOARCH != "arm" && GOARCH != "arm64" {
		unminitSignals()
	}
}

//go:nosplit
func osyield() {
	usleep(1)
}

const (
	_NSIG        = 32
	_SI_USER     = 0 /* empirically true, but not what headers say */
	_SIG_BLOCK   = 1
	_SIG_UNBLOCK = 2
	_SIG_SETMASK = 3
	_SS_DISABLE  = 4
)

//extern SigTabTT runtime·sigtab[];

type sigset uint32

var sigset_all = ^sigset(0)

//go:nosplit
//go:nowritebarrierrec
func setsig(i uint32, fn uintptr) {
	var sa usigactiont
	sa.sa_flags = _SA_SIGINFO | _SA_ONSTACK | _SA_RESTART
	sa.sa_mask = ^uint32(0)
	if fn == funcPC(sighandler) {
		if iscgo {
			fn = funcPC(cgoSigtramp)
		} else {
			fn = funcPC(sigtramp)
		}
	}
	*(*uintptr)(unsafe.Pointer(&sa.__sigaction_u)) = fn
	sigaction(i, &sa, nil)
}

// sigtramp 是当接收到信号后从 lib 的回调
// 它由 C 的调用约定进行调用
func sigtramp()
func cgoSigtramp()

//go:nosplit
//go:nowritebarrierrec
func setsigstack(i uint32) {
	var osa usigactiont
	sigaction(i, nil, &osa)
	handler := *(*uintptr)(unsafe.Pointer(&osa.__sigaction_u))
	if osa.sa_flags&_SA_ONSTACK != 0 {
		return
	}
	var sa usigactiont
	*(*uintptr)(unsafe.Pointer(&sa.__sigaction_u)) = handler
	sa.sa_mask = osa.sa_mask
	sa.sa_flags = osa.sa_flags | _SA_ONSTACK
	sigaction(i, &sa, nil)
}

//go:nosplit
//go:nowritebarrierrec
func getsig(i uint32) uintptr {
	var sa usigactiont
	sigaction(i, nil, &sa)
	return *(*uintptr)(unsafe.Pointer(&sa.__sigaction_u))
}

// setSignaltstackSP sets the ss_sp field of a stackt.
//go:nosplit
func setSignalstackSP(s *stackt, sp uintptr) {
	*(*uintptr)(unsafe.Pointer(&s.ss_sp)) = sp
}

//go:nosplit
//go:nowritebarrierrec
func sigaddset(mask *sigset, i int) {
	*mask |= 1 << (uint32(i) - 1)
}

func sigdelset(mask *sigset, i int) {
	*mask &^= 1 << (uint32(i) - 1)
}

//go:linkname executablePath os.executablePath
var executablePath string

func sysargs(argc int32, argv **byte) {
	// 跳过 argv, envv 与第一个字符串为路径
	n := argc + 1
	for argv_index(argv, n) != nil {
		n++
	}
	executablePath = gostringnocopy(argv_index(argv, n+1))

	// 移除 "executable_path=" 前缀（OS X 10.11 之后存在）
	const prefix = "executable_path="
	if len(executablePath) > len(prefix) && executablePath[:len(prefix)] == prefix {
		executablePath = executablePath[len(prefix):]
	}
}
