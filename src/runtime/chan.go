// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// This file contains the implementation of Go channels.

// Invariants:
//  At least one of c.sendq and c.recvq is empty,
//  except for the case of an unbuffered channel with a single goroutine
//  blocked on it for both sending and receiving using a select statement,
//  in which case the length of c.sendq and c.recvq is limited only by the
//  size of the select statement.
//
// For buffered channels, also:
//  c.qcount > 0 implies that c.recvq is empty.
//  c.qcount < c.dataqsiz implies that c.sendq is empty.

import (
	"runtime/internal/atomic"
	"runtime/internal/math"
	"unsafe"
)

const (
	// 用来设置内存最大对齐值，对应就是64位系统下cache line的大小
	maxAlign  = 8
	//  向上进行 8 字节内存对齐，假设hchan{}大小是14，hchanSize是16。
	hchanSize = unsafe.Sizeof(hchan{}) + uintptr(-int(unsafe.Sizeof(hchan{}))&(maxAlign-1))
	debugChan = false
)

type hchan struct {
	// chan 中的数据量
	qcount   uint
	// chan 循环队列的长度，底层是数组
	dataqsiz uint
	// chan 指向底层缓冲数组的指针
	buf      unsafe.Pointer
	// chan 中元素大小
	elemsize uint16
	// chan 是否被关闭，非0表示关闭
	closed   uint32
	// chan 中元素类型
	elemtype *_type
	// 生产队列可发送的元素在数组中的索引，即从此处开始写入
	sendx    uint
	// 消费队列可接收的元素在数组中的索引，即从此处开始消费
	recvx    uint
	// 等待接收数据的goroutine队列，消费队列
	recvq    waitq
	// 等待发送数据的goroutine队列，生产队列
	sendq    waitq

	// lock protects all fields in hchan, as well as several
	// fields in sudogs blocked on this channel.
	//
	// Do not change another G's status while holding this lock
	// (in particular, do not ready a G), as this can deadlock
	// with stack shrinking.
	lock mutex
	// 锁定保护 HCHAN 中的所有字段，以及此通道上阻止的 Sudog 中的多个字段。
	// 在保持此锁时不要更改另一个 G 的状态（特别是不要准备 G），因为这可能会导致堆栈收缩而死锁。
}

// goroutine 的生产队列或消费队列
type waitq struct {
	first *sudog // 指向goroutine队列的第一个
	last  *sudog // 指向goroutine队列的最后一个
}

//go:linkname reflect_makechan reflect.makechan
func reflect_makechan(t *chantype, size int) *hchan {
	return makechan(t, size)
}

func makechan64(t *chantype, size int64) *hchan {
	if int64(int(size)) != size {
		panic(plainError("makechan: size out of range"))
	}

	return makechan(t, int(size))
}

func makechan(t *chantype, size int) *hchan {
	elem := t.elem

	// compiler checks this but be safe.
	if elem.size >= 1<<16 { // 元素类型判断
		throw("makechan: invalid channel element type")
	}
	// todo hchanSize?
	if hchanSize%maxAlign != 0 || elem.align > maxAlign { // 元素对齐方式判断
		throw("makechan: bad alignment")
	}

	// chan大小乘以元素大小，得到要分配的内存大小，需要判断乘积是溢出，以及是否超过了最大内存分配等
	mem, overflow := math.MulUintptr(elem.size, uintptr(size))
	if overflow || mem > maxAlloc-hchanSize || size < 0 {
		panic(plainError("makechan: size out of range"))
	}

	// Hchan does not contain pointers interesting for GC when elements stored in buf do not contain pointers.
	// buf points into the same allocation, elemtype is persistent.
	// SudoG's are referenced from their owning thread so they can't be collected.
	// TODO(dvyukov,rlh): Rethink when collector can move allocated objects.
	// 当存储在 buf 中的元素不包含指针时，Hchan 不包含对 GC 感兴趣的指针。BUF点到相同的分配中，elemtype是持久的。
	// SudoG 是从其所属线程引用的，因此无法收集它们。
	// 如果 hchan 结构体中不含指针，GC 就不会扫描 chan 中的元素
	var c *hchan
	switch {
	case mem == 0:
		// 当chan为无缓冲或元素为空结构体时，需要分配的内存为0，仅分配需要存储chan的内存
		// Queue or element size is zero.
		c = (*hchan)(mallocgc(hchanSize, nil, true))
		// Race detector uses this location for synchronization.
		c.buf = c.raceaddr()
	case elem.ptrdata == 0:
		// Elements do not contain pointers.
		// Allocate hchan and buf in one call.
		// 元素不包含指针。在一次调用中分配 hchan 和 buf。
		// 当通道数据元素不含指针，hchan和buf内存空间调用mallocgc一次性分配完成，hchanSize用来分配通道的内存，mem用来分配buf的内存
		c = (*hchan)(mallocgc(hchanSize+mem, nil, true))
		// c 为指向 hchan 的指针，再加上该结构的大小 hchanSize，即可得到指向 buf的指针，它们在内存上是连续的
		c.buf = add(unsafe.Pointer(c), hchanSize)
	default:
		// Elements contain pointers.
		// 通道中的元素包含指针，分别创建 chan 和 buf 的内存空间
		c = new(hchan)
		c.buf = mallocgc(mem, elem, true)
	}

	c.elemsize = uint16(elem.size) // 元素大小
	c.elemtype = elem // 元素类型
	c.dataqsiz = uint(size) // chan 的容量
	lockInit(&c.lock, lockRankHchan) // todo ？

	if debugChan {
		print("makechan: chan=", c, "; elemsize=", elem.size, "; dataqsiz=", size, "\n")
	}
	return c
}

// chanbuf(c, i) is pointer to the i'th slot in the buffer.
// 获取 buf 中第 i 个位置的元素
func chanbuf(c *hchan, i uint) unsafe.Pointer {
	return add(c.buf, uintptr(i)*uintptr(c.elemsize))
}

// full reports whether a send on c would block (that is, the channel is full).
// It uses a single word-sized read of mutable state, so although
// the answer is instantaneously true, the correct answer may have changed
// by the time the calling function receives the return value.
// Full 报告在 C 上的发送是否会阻塞（即通道已满）。
// 它使用单个字大小的读取可变状态，因此尽管答案是即时正确的，但在调用函数收到返回值时，正确答案可能已经改变。
// full 函数检测 channel 缓冲区是否已满，主要分为两种情况:
// 如果 channel 没有缓冲区，查看是否存在接收者
// 如果 channel 有缓冲区, 比较元素数量和缓冲区长度是否一致
func full(c *hchan) bool {
	// c.dataqsiz is immutable (never written after the channel is created)
	// so it is safe to read at any time during channel operation.
	// c.dataqsiz 是不可变的（在创建通道后永远不会写入），因此在通道操作期间随时读取是安全的。
	if c.dataqsiz == 0 { // 无缓冲，且没有消费队列
		// Assumes that a pointer read is relaxed-atomic.
		return c.recvq.first == nil
	}
	// Assumes that a uint read is relaxed-atomic.
	// 有缓冲，且缓冲区大小和chan中实际元素个数相等，即满了
	return c.qcount == c.dataqsiz
}

// entry point for c <- x from compiled code
//go:nosplit
// 编译代码中 C <- X 的入口点
func chansend1(c *hchan, elem unsafe.Pointer) {
	chansend(c, elem, true, getcallerpc())
}

/*
 * generic single channel send/recv
 * If block is not nil,
 * then the protocol will not
 * sleep but return if it could
 * not complete.
 *
 * sleep can wake up with g.param == nil
 * when a channel involved in the sleep has
 * been closed.  it is easiest to loop and re-run
 * the operation; we'll see that it's now closed.
 */
// 一般情况下，单向的接收或发送通道，，但如果无法完成则返回。
// 当休眠中涉及的通道关闭时，休眠可以使用 g.param == nil 唤醒。循环并重新运行操作最容易;我们将看到它现在已经关闭。
// 返回 false 表示写入失败
func chansend(c *hchan, ep unsafe.Pointer, block bool, callerpc uintptr) bool {
	// 如果 block 为 false，协议将不允许被阻塞，不等于非缓冲
	if c == nil {
		// chan 为 nil
		// 非阻塞模式下直接返回
		if !block {
			return false
		}
		// 阻塞模式下，直接阻塞当前写入协程
		// 调用gopark将当前Goroutine休眠，关闭 nil 的 chan 会panic，故当前Goroutine会一直休眠，陷入死锁
		gopark(nil, nil, waitReasonChanSendNilChan, traceEvGoStop, 2)
		throw("unreachable")
	}

	if debugChan {
		print("chansend: chan=", c, "\n")
	}

	if raceenabled {
		racereadpc(c.raceaddr(), callerpc, funcPC(chansend))
	}

	// Fast path: check for failed non-blocking operation without acquiring the lock.
	//
	// After observing that the channel is not closed, we observe that the channel is
	// not ready for sending. Each of these observations is a single word-sized read
	// (first c.closed and second full()).
	// Because a closed channel cannot transition from 'ready for sending' to
	// 'not ready for sending', even if the channel is closed between the two observations,
	// they imply a moment between the two when the channel was both not yet closed
	// and not ready for sending. We behave as if we observed the channel at that moment,
	// and report that the send cannot proceed.
	//
	// It is okay if the reads are reordered here: if we observe that the channel is not
	// ready for sending and then observe that it is not closed, that implies that the
	// channel wasn't closed during the first observation. However, nothing here
	// guarantees forward progress. We rely on the side effects of lock release in
	// chanrecv() and closechan() to update this thread's view of c.closed and full().

	// 快速路径：在不获取锁的情况下检查失败的非阻塞操作。
	// 在观察到通道未关闭后，我们观察到通道尚未准备好发送。这些观察中的每一个都是单个字大小的读取（第一个c.closed和第二个full（））。
	// 由于闭合通道无法从“准备发送”过渡到“未准备好发送”，因此即使通道在两个观测值之间关闭，它们也意味着当通道尚未关闭且尚未准备好发送时，两者之间存在一个时刻。
	// 我们的行为就好像我们当时观察了通道，并报告发送无法继续。如果读取在这里重新排序是可以的：如果我们观察到通道尚未准备好发送，然后观察到它没有关闭，这意味着在第一次观察期间通道没有关闭。
	// 然而，这里没有任何东西能保证向前推进。我们依靠 chanrecv（） 和 closechan（） 中锁释放的副作用来更新这个线程对 c.closed 和 full（） 的看法。
	// 非阻塞模式，且chan没有关闭，但已经满了
	if !block && c.closed == 0 && full(c) {
		return false
	}

	// 执行到此处说明是以下3种情况中的某一种或两种
	// 1，阻塞模式，block==true；
	// 2，chan 已经关闭；
	// 3，管道非满的情况
	// 3.1 无缓冲管道，且接收队列不为空；
	// 3.2 缓冲管道，但缓冲管道未满
	var t0 int64
	if blockprofilerate > 0 {
		t0 = cputicks()
	}

	lock(&c.lock)
	// 2，chan 已经关闭；
	if c.closed != 0 { // todo 向一个关闭的通道写入数据会panic
		unlock(&c.lock)
		panic(plainError("send on closed channel"))
	}

	// 执行到此处，说明管道是未关闭的，阻塞模式或管道非满
	// 从接收者队列recvq中取出一个接收者，接收者不为空情况下，直接将数据传递给该接收者
	// 3.1 无缓冲管道，且接收队列不为空；即使非阻塞能写则写
	if sg := c.recvq.dequeue(); sg != nil {
		// Found a waiting receiver. We pass the value we want to send
		// directly to the receiver, bypassing the channel buffer (if any).
		// todo 非常细节，找到一个等待的接收器。我们将要发送的值直接传递给接收器，绕过通道缓冲区（如果有的话）。
		send(c, sg, ep, func() { unlock(&c.lock) }, 3)
		return true
	}

	// 3.2 缓冲管道，但没有接收者，且缓冲管道未满；即使非阻塞能写则写
	if c.qcount < c.dataqsiz {
		// Space is available in the channel buffer. Enqueue the element to send.
		// 找到最新能够写入元素的位置
		qp := chanbuf(c, c.sendx)
		if raceenabled {
			racenotify(c, c.sendx, nil)
		}
		// 将要写入的元素的值拷贝到该处
		typedmemmove(c.elemtype, qp, ep)
		c.sendx++ // 写入的位置往后移
		if c.sendx == c.dataqsiz { // 如果等于数组长度，则跳转到首位（循环队列）
			c.sendx = 0
		}
		c.qcount++ // chan 中的元素个数加一
		unlock(&c.lock)
		return true
	}

	// todo 执行到此处，说明如果是无缓冲管道则没有接收者，是缓冲管道则已经满了，下方 block 为 true 下方 if 无法执行？
	if !block {
		unlock(&c.lock)
		return false
	}

	// Block on the channel. Some receiver will complete our operation for us.
	// channel 满了，发送方会被阻塞。接下来会构造一个 sudog
	// 获取当前发送数据的 goroutine
	// 然后绑定到一个 sudog 结构体 (包装为运行时表示)
	gp := getg()// 获取当前 goroutine 的指针
	mysg := acquireSudog() // 返回一个sudog
	// 获取 sudog 结构体
	// 并且设置相关字段 (包括当前的 channel，是否是 select 等)
	mysg.releasetime = 0
	if t0 != 0 {
		mysg.releasetime = -1
	}
	// No stack splits between assigning elem and enqueuing mysg
	// on gp.waiting where copystack can find it.
	mysg.elem = ep
	mysg.waitlink = nil
	mysg.g = gp
	mysg.isSelect = false
	mysg.c = c
	gp.waiting = mysg
	gp.param = nil
	// 当前 goroutine 进入发送等待队列
	c.sendq.enqueue(mysg)
	// Signal to anyone trying to shrink our stack that we're about
	// to park on a channel. The window between when this G's status
	// changes and when we set gp.activeStackChans is not safe for
	// stack shrinking.
	atomic.Store8(&gp.parkingOnChan, 1)
	// 挂起当前 goroutine, 进入休眠 (等待接收)
	gopark(chanparkcommit, unsafe.Pointer(&c.lock), waitReasonChanSend, traceEvGoBlockSend, 2)
	// Ensure the value being sent is kept alive until the
	// receiver copies it out. The sudog has a pointer to the
	// stack object, but sudogs aren't considered as roots of the
	// stack tracer.

	// 确保发送的值保持活动状态，直到接收方将其复制出来。sudog 具有指向堆栈对象的指针，但 sudog 不被视为堆栈跟踪器的根。
	KeepAlive(ep)

	// someone woke us up.

	// 从这里开始被唤醒了（channel 有机会可以发送了）
	if mysg != gp.waiting {
		throw("G waiting list is corrupted")
	}
	gp.waiting = nil
	gp.activeStackChans = false
	closed := !mysg.success
	gp.param = nil
	if mysg.releasetime > 0 {
		blockevent(mysg.releasetime-t0, 2)
	}
	// 取消 sudog 和 channel 绑定关系
	mysg.c = nil
	releaseSudog(mysg) // 去掉 mysg 上绑定的 channel
	if closed {
		if c.closed == 0 {
			throw("chansend: spurious wakeup")
		}
		// 被唤醒后，管道关闭了，todo 向一个关闭的管道发送数据会panic
		panic(plainError("send on closed channel"))
	}
	return true
}

// send processes a send operation on an empty channel c.
// The value ep sent by the sender is copied to the receiver sg.
// The receiver is then woken up to go on its merry way.
// Channel c must be empty and locked.  send unlocks c with unlockf.
// sg must already be dequeued from c.
// ep must be non-nil and point to the heap or the caller's stack.
// send 处理空通道 C 上的发送操作。发送方发送的值 ep 被复制到接收方 sg。
// 然后接收器被唤醒，继续它的快乐之路。通道 c 必须为空并锁定。send 通过 unlockf 解锁 c。
// SG 必须已从 C 中取消排队，EP 必须为非 nil 并指向堆或调用方的堆栈。
func send(c *hchan, sg *sudog, ep unsafe.Pointer, unlockf func(), skip int) {
	if raceenabled {
		if c.dataqsiz == 0 {
			racesync(c, sg)
		} else {
			// Pretend we go through the buffer, even though
			// we copy directly. Note that we need to increment
			// the head/tail locations only when raceenabled.
			racenotify(c, c.recvx, nil)
			racenotify(c, c.recvx, sg)
			c.recvx++
			if c.recvx == c.dataqsiz {
				c.recvx = 0
			}
			c.sendx = c.recvx // c.sendx = (c.sendx+1) % c.dataqsiz
		}
	}
	// sg.elem 指向接收到的值存放的位置，如 val <- ch，指的就是 &val
	if sg.elem != nil {
		// 直接拷贝内存（从发送者到接收者）
		sendDirect(c.elemtype, sg, ep)
		sg.elem = nil
	}
	gp := sg.g
	unlockf()
	gp.param = unsafe.Pointer(sg)
	sg.success = true
	if sg.releasetime != 0 {
		sg.releasetime = cputicks()
	}
	// 唤醒接收的 goroutine. skip 和打印栈相关
	// 调用 goready 函数将接收方 goroutine 唤醒并标记为可运行状态
	// 并把其放入发送方所在处理器 P 的 runnext 字段等待执行
	// runnext 字段表示最高优先级的 goroutine
	goready(gp, skip+1)
}

// Sends and receives on unbuffered or empty-buffered channels are the
// only operations where one running goroutine writes to the stack of
// another running goroutine. The GC assumes that stack writes only
// happen when the goroutine is running and are only done by that
// goroutine. Using a write barrier is sufficient to make up for
// violating that assumption, but the write barrier has to work.
// typedmemmove will call bulkBarrierPreWrite, but the target bytes
// are not in the heap, so that will not help. We arrange to call
// memmove and typeBitsBulkBarrier instead.
// 向一个非缓冲型的 channel 发送数据、从一个无元素的（非缓冲型或缓冲型但空）的 channel
// 接收数据，都会导致一个 goroutine 直接操作另一个 goroutine 的栈
// 由于 GC 假设对栈的写操作只能发生在 goroutine 正在运行中并且由当前 goroutine 来写
// 所以这里实际上违反了这个假设。可能会造成一些问题，所以需要用到写屏障来规避
func sendDirect(t *_type, sg *sudog, src unsafe.Pointer) {
	// src is on our stack, dst is a slot on another stack.

	// Once we read sg.elem out of sg, it will no longer
	// be updated if the destination's stack gets copied (shrunk).
	// So make sure that no preemption points can happen between read & use.

	// src 在当前 goroutine 的栈上，dst 是另一个 goroutine 的栈

	// 直接进行内存"搬迁"
	// 如果目标地址的栈发生了栈收缩，当我们读出了 sg.elem 后
	// 就不能修改真正的 dst 位置的值了
	dst := sg.elem
	// 因此需要在读和写之前加上一个屏障
	typeBitsBulkBarrier(t, uintptr(dst), uintptr(src), t.size)
	// No need for cgo write barrier checks because dst is always
	// Go memory.
	memmove(dst, src, t.size)
}

func recvDirect(t *_type, sg *sudog, dst unsafe.Pointer) {
	// dst is on our stack or the heap, src is on another stack.
	// The channel is locked, so src will not move during this
	// operation.
	src := sg.elem
	typeBitsBulkBarrier(t, uintptr(dst), uintptr(src), t.size)
	memmove(dst, src, t.size)
}

// 关闭 channel 后，对于等待接收者而言，会收到一个相应类型的零值。对于等待发送者，会直接 panic。
// 所以，在不了解 channel 还有没有接收者的情况下，不能贸然关闭 channel。
// close 函数先上一把大锁，接着把所有挂在这个 channel 上的 sender 和 receiver 全都连成一个 sudog 链表，再解锁。最后，再将所有的 sudog 全都唤醒。
// 唤醒之后，该干嘛干嘛。sender 会继续执行 chansend 函数里 goparkunlock 函数之后的代码，很不幸，检测到 channel 已经关闭了，panic。receiver 则比较幸运，进行一些扫尾工作后，
func closechan(c *hchan) {
	if c == nil { // todo 关闭一个空的 chan 会 panic
		panic(plainError("close of nil channel"))
	}
	// 加锁，这个锁的粒度比较大
	// 会持续到释放完所有的 sudog 才解锁
	lock(&c.lock)
	if c.closed != 0 { // todo 关闭一个已经关闭的 chan 会 panic
		unlock(&c.lock)
		panic(plainError("close of closed channel"))
	}

	if raceenabled {
		callerpc := getcallerpc()
		racewritepc(c.raceaddr(), callerpc, funcPC(closechan))
		racerelease(c.raceaddr())
	}
	// 设置 channel 状态为已关闭
	c.closed = 1
	// 用于存放发送+接收队列中的所有 goroutine
	var glist gList

	// 将接收队列中所有 goroutine 加入 gList 列表
	for {
		// 如果此通道的接收数据协程队列不为空（这种情况下，缓冲队列必为空），
		// 此队列中的所有协程将被依个弹出，并且每个协程将接收到此通道的元素类型的一个零值，然后恢复至运行状态
		sg := c.recvq.dequeue()
		if sg == nil {
			// 出队的 sudog 为 nil，说明发送队列为空，直接跳出循环
			break
		}
		// 如果 elem 不为空，说明此 receiver 未忽略接收数据
		// 给它赋一个相应类型的零值
		if sg.elem != nil {
			typedmemclr(c.elemtype, sg.elem)
			sg.elem = nil
		}
		if sg.releasetime != 0 {
			sg.releasetime = cputicks()
		}
		// 取出 goroutine
		gp := sg.g
		gp.param = unsafe.Pointer(sg)
		sg.success = false // todo import 因为关闭而唤醒，设为false
		if raceenabled {
			raceacquireg(gp, c.raceaddr())
		}
		// 将 sg 对应的 goroutine 添加到 glist 列表
		glist.push(gp)
	}

	// 将发送队列中所有 goroutine 加入 gList 列表
	// todo 如果存在，这些 goroutine 将会 panic，在写入处引发panic
	for {
		// 如果此通道的发送数据协程队列不为空，此队列中的所有协程将被依个弹出，并且每个协程中都将产生一个恐慌（因为向已关闭的通道发送数据）。
		sg := c.sendq.dequeue()
		if sg == nil {
			// 出队的 sudog 为 nil，说明发送队列为空，直接跳出循环
			break
		}
		// 忽略发送协程的值
		sg.elem = nil
		if sg.releasetime != 0 {
			sg.releasetime = cputicks()
		}
		gp := sg.g
		gp.param = unsafe.Pointer(sg)
		sg.success = false // todo import 因为关闭而唤醒，设为false
		if raceenabled {
			raceacquireg(gp, c.raceaddr())
		}
		// 将 sg 对应的 goroutine 添加到 glist 列表
		glist.push(gp)
	}
	// 解锁
	unlock(&c.lock)

	// 准备好所有 G，现在我们已经删除了通道锁。
	for !glist.empty() {
		gp := glist.pop()
		gp.schedlink = 0
		// 唤醒所有线程
		// 接收队列里的协程获取零值，继续后续执行
		// todo 发送队列里的协程，触发panic
		goready(gp, 3)
		// 	唤醒发送和接收协程，发送协程从 chansend 中的 gopark 后开始执行；接收协程从 chanrecv 中的 gopark 后开始执行
	}
}

// 无缓冲区且没有发送方
// 有缓冲区但没有数据
func empty(c *hchan) bool {
	// c.dataqsiz is immutable.
	if c.dataqsiz == 0 {
		// 无缓冲 channel 并且没有发送方正在阻塞
		return atomic.Loadp(unsafe.Pointer(&c.sendq.first)) == nil
	}
	// 有缓冲 channel 并且缓冲区没有数据
	return atomic.Loaduint(&c.qcount) == 0
}

// entry points for <- c from compiled code
//go:nosplit
// <- c 代码的编译入口
func chanrecv1(c *hchan, elem unsafe.Pointer) {
	chanrecv(c, elem, true)
}

//go:nosplit
func chanrecv2(c *hchan, elem unsafe.Pointer) (received bool) {
	_, received = chanrecv(c, elem, true)
	return
}

// chanrecv receives on channel c and writes the received data to ep.
// ep may be nil, in which case received data is ignored.
// If block == false and no elements are available, returns (false, false).
// Otherwise, if c is closed, zeros *ep and returns (true, false).
// Otherwise, fills in *ep with an element and returns (true, true).
// A non-nil ep must point to the heap or the caller's stack.
// 如果 channel == nil, 非阻塞模式直接返回，阻塞模式，休眠当前 goroutine
// 如果 channel 已经关闭或者缓冲区没有等待接收的数据，直接返回
// 如果 channel 发送队列不为空, 说明没有缓冲区或缓冲区已满
// 		从发送队列获取第一个发送者协程
// 		如果是无缓冲区，直接从发送 goroutine 拷贝数据到接收数据的地址
// 否则，缓冲区已满，从接收队列头部的 goroutine 开始接收数据，并将数据添加到发送队列尾部的 goroutine
// 如果 channel 缓冲区有数据，直接从缓冲区读取数据
// 如果以上条件都不满足，就获取一个新的 sudog 结构体并放入 channel 的接收队列，同时挂起当前发送数据的 goroutine, 进入休眠 (等待发送方发送数据)
func chanrecv(c *hchan, ep unsafe.Pointer, block bool) (selected, received bool) {
	if debugChan {
		print("chanrecv: chan=", c, "\n")
	}

	if c == nil {
		// 非阻塞模式下直接返回
		if !block {
			return
		}
		// 调用gopark将当前Goroutine休眠，调用gopark时候，将传入unlockf设置为nil，当前Goroutine会一直休眠
		gopark(nil, nil, waitReasonChanReceiveNilChan, traceEvGoStop, 2)
		throw("unreachable")
	}

	// 非阻塞模式并且接收数据操作会阻塞
	// empty 函数返回 true 的情况:
	//    1. 无缓冲 channel 并且没有发送方正在阻塞
	//    2. 有缓冲 channel 并且缓冲区没有数据
	if !block && empty(c) {
		// 判断是否关闭
		if atomic.Load(&c.closed) == 0 {
			// 非阻塞、无数据、且未关闭，直接返回
			// 因为 channel 关闭后就无法再打开，所以只要 channel 未关闭，上述方法都是原子操作 (看到的结果都是一样的)
			return
		}

		// channel 已经关闭，重新检查 channel 是否存在等待接收的数据
		if empty(c) {
			// 通道不可逆地关闭和为空
			if raceenabled {
				raceacquire(c.raceaddr())
			}
			// 没有任何等待接收的数据，清理 ep 指针中的数据
			if ep != nil {
				typedmemclr(c.elemtype, ep)
			}
			return true, false
		}
	}

	var t0 int64
	if blockprofilerate > 0 {
		t0 = cputicks()
	}

	lock(&c.lock)

	// channel 已经关闭，且没有数据
	if c.closed != 0 && c.qcount == 0 {
		if raceenabled {
			raceacquire(c.raceaddr())
		}
		// 解锁
		unlock(&c.lock)
		if ep != nil {
			// 清理 ep 指针中的数据
			typedmemclr(c.elemtype, ep)
		}
		return true, false
	}

	// channel 未关闭，或关闭了但是还有数据

	// 还有阻塞的发送者协程，说明没有缓冲区或是缓冲区已满
	if sg := c.sendq.dequeue(); sg != nil {
		// 从发送队列获取第一个发送者协程
		// 如果是无缓冲区，直接从发送 goroutine 拷贝数据到接收数据的地址
		// 否则，缓冲区已满，从接收队列头部的 goroutine 开始接收数据，并将数据添加到发送队列尾部的 goroutine
		recv(c, sg, ep, func() { unlock(&c.lock) }, 3)
		return true, true
	}

	// 没有阻塞的发送者协程，但是channel里面还有数据
	if c.qcount > 0 {
		// 根据可消费数据的索引直接获取到要消费的数据的地址
		qp := chanbuf(c, c.recvx)
		if raceenabled {
			racenotify(c, c.recvx, nil)
		}
		if ep != nil {
			// 直接从缓冲区的地址上拷贝数据到接收数据的地址
			typedmemmove(c.elemtype, ep, qp)
		}
		// 清除已经消费的数据
		typedmemclr(c.elemtype, qp)
		// 消费索引往后移
		c.recvx++
		if c.recvx == c.dataqsiz {
			c.recvx = 0
		}
		// 元素数量减一
		c.qcount--
		unlock(&c.lock)
		return true, true
	}

	// 没有等待的发送者协程，缓冲区没有数据，且非阻塞的，直接返回
	if !block {
		unlock(&c.lock)
		return false, false
	}

	// 没有等待的发送者协程，缓冲区没有数据，且阻塞的
	// 获取当前接收的协程 goroutine
	// 然后绑定到一个 sudog 结构体 (包装为运行时表示)
	gp := getg()
	// 获取 sudog 结构体，并设置相关参数
	mysg := acquireSudog()
	mysg.releasetime = 0
	if t0 != 0 {
		mysg.releasetime = -1
	}
	mysg.elem = ep // 设置接收数据的地址
	mysg.waitlink = nil
	gp.waiting = mysg
	mysg.g = gp // 设置 goroutine
	mysg.isSelect = false // 设置是否 select
	mysg.c = c // 设置当前的 channel
	gp.param = nil
	c.recvq.enqueue(mysg) // 进入接收队列等待
	// Signal to anyone trying to shrink our stack that we're about
	// to park on a channel. The window between when this G's status
	// changes and when we set gp.activeStackChans is not safe for
	// stack shrinking.
	atomic.Store8(&gp.parkingOnChan, 1)
	// 挂起当前 goroutine, 进入休眠 (等待发送方发送数据)，阻塞中
	gopark(chanparkcommit, unsafe.Pointer(&c.lock), waitReasonChanReceive, traceEvGoBlockRecv, 2)

	// someone woke us up
	// 因为某种原因而被唤醒，重新获取gp
	if mysg != gp.waiting {
		throw("G waiting list is corrupted")
	}
	gp.waiting = nil
	gp.activeStackChans = false
	if mysg.releasetime > 0 {
		blockevent(mysg.releasetime-t0, 2)
	}
	// todo 被唤醒的原因，true，因为写入了数据，false，因为关闭了管道
	success := mysg.success
	gp.param = nil
	// 取消 sudog 和 channel 绑定关系
	mysg.c = nil
	// 释放 sudog
	releaseSudog(mysg)
	return true, success
}

// recv processes a receive operation on a full channel c.
// There are 2 parts:
// 1) The value sent by the sender sg is put into the channel
//    and the sender is woken up to go on its merry way.
// 2) The value received by the receiver (the current G) is
//    written to ep.
// For synchronous channels, both values are the same.
// For asynchronous channels, the receiver gets its data from
// the channel buffer and the sender's data is put in the
// channel buffer.
// Channel c must be full and locked. recv unlocks c with unlockf.
// sg must already be dequeued from c.
// A non-nil ep must point to the heap or the caller's stack.
func recv(c *hchan, sg *sudog, ep unsafe.Pointer, unlockf func(), skip int) {
	// 还有阻塞的发送者协程，说明没有缓冲区或是缓冲区已满
	if c.dataqsiz == 0 {
		// 无缓冲区
		if raceenabled {
			racesync(c, sg)
		}
		if ep != nil {
			// 直接从发送者接收数据
			recvDirect(c.elemtype, sg, ep)
		}
	} else {
		// 缓冲区已满
		// 从消费索引处获取数据的指针
		qp := chanbuf(c, c.recvx)
		if raceenabled {
			racenotify(c, c.recvx, nil)
			racenotify(c, c.recvx, sg)
		}
		// copy data from queue to receiver
		if ep != nil {
			// 将消费索引处的数据拷贝到接收数据的指针
			typedmemmove(c.elemtype, ep, qp)
		}

		// 因为缓冲区已经满了，所以生产索引和消费索引是同一个位置
		// 直接将发送者协程的数据拷贝到消费索引处
		typedmemmove(c.elemtype, qp, sg.elem)
		// 消费索引加一
		c.recvx++
		if c.recvx == c.dataqsiz {
			c.recvx = 0
		}
		// 缓冲区是满的，两者相等，元素数量不变
		c.sendx = c.recvx // c.sendx = (c.sendx+1) % c.dataqsiz
	}
	// 发送者协程的数据指针置空
	sg.elem = nil
	gp := sg.g
	// 解锁
	unlockf()
	gp.param = unsafe.Pointer(sg)
	// 因为写入值成功而被唤醒
	sg.success = true
	if sg.releasetime != 0 {
		sg.releasetime = cputicks()
	}
	// 调用 goready 函数将接收方 goroutine 唤醒并标记为可运行状态
	// 并把其放入发送方所在处理器 P 的 runnext 字段等待执行
	goready(gp, skip+1)
}

func chanparkcommit(gp *g, chanLock unsafe.Pointer) bool {
	// There are unlocked sudogs that point into gp's stack. Stack
	// copying must lock the channels of those sudogs.
	// Set activeStackChans here instead of before we try parking
	// because we could self-deadlock in stack growth on the
	// channel lock.
	gp.activeStackChans = true
	// Mark that it's safe for stack shrinking to occur now,
	// because any thread acquiring this G's stack for shrinking
	// is guaranteed to observe activeStackChans after this store.
	atomic.Store8(&gp.parkingOnChan, 0)
	// Make sure we unlock after setting activeStackChans and
	// unsetting parkingOnChan. The moment we unlock chanLock
	// we risk gp getting readied by a channel operation and
	// so gp could continue running before everything before
	// the unlock is visible (even to gp itself).
	unlock((*mutex)(chanLock))
	return true
}

// compiler implements
//
//	select {
//	case c <- v:
//		... foo
//	default:
//		... bar
//	}
//
// as
//
//	if selectnbsend(c, v) {
//		... foo
//	} else {
//		... bar
//	}
//
// select case 编译时，发送数据为非阻塞，即非阻塞型
// todo must import, 没有default时，select case 编译成 chansend1(c *hchan, elem unsafe.Pointer)，即阻塞型
func selectnbsend(c *hchan, elem unsafe.Pointer) (selected bool) {
	return chansend(c, elem, false, getcallerpc())
}

// compiler implements
//
//	select {
//	case v, ok = <-c:
//		... foo
//	default:
//		... bar
//	}
//
// as
//
//	if selected, ok = selectnbrecv(&v, c); selected {
//		... foo
//	} else {
//		... bar
//	}
// select case 编译时，接收数据为非阻塞，即非阻塞型
// todo must import, 没有default时，select case 编译成 chanrecv1(c *hchan, elem unsafe.Pointer)，即阻塞型
// 3. 所有case都未ready，且没有default语句
//   3.1 将当前协程加入到所有channel的等待队列
//   3.2 当将协程转入阻塞，等待被唤醒
func selectnbrecv(elem unsafe.Pointer, c *hchan) (selected, received bool) {
	return chanrecv(c, elem, false)
}

//go:linkname reflect_chansend reflect.chansend
func reflect_chansend(c *hchan, elem unsafe.Pointer, nb bool) (selected bool) {
	return chansend(c, elem, !nb, getcallerpc())
}

//go:linkname reflect_chanrecv reflect.chanrecv
func reflect_chanrecv(c *hchan, nb bool, elem unsafe.Pointer) (selected bool, received bool) {
	return chanrecv(c, elem, !nb)
}

//go:linkname reflect_chanlen reflect.chanlen
func reflect_chanlen(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.qcount)
}

//go:linkname reflectlite_chanlen internal/reflectlite.chanlen
func reflectlite_chanlen(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.qcount)
}

//go:linkname reflect_chancap reflect.chancap
func reflect_chancap(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.dataqsiz)
}

//go:linkname reflect_chanclose reflect.chanclose
func reflect_chanclose(c *hchan) {
	closechan(c)
}

func (q *waitq) enqueue(sgp *sudog) {
	sgp.next = nil
	x := q.last
	if x == nil {
		sgp.prev = nil
		q.first = sgp
		q.last = sgp
		return
	}
	sgp.prev = x
	x.next = sgp
	q.last = sgp
}

// 从协程的等待队列中出列
func (q *waitq) dequeue() *sudog {
	for {
		// 获取队列中的首个协程
		sgp := q.first
		if sgp == nil {
			// 为空则直接返回
			return nil
		}
		y := sgp.next
		if y == nil {
			// 如果该协程下个协程为空，则整个队列都为空
			q.first = nil
			q.last = nil
		} else {
			// 否则将下个协程的前置指针置空
			y.prev = nil
			// 将下个赋给首位
			q.first = y
			// 将要出队的协程的后置指针置空，切断与其他协程的联系
			sgp.next = nil // mark as removed (see dequeueSudog)
		}

		// if a goroutine was put on this queue because of a
		// select, there is a small window between the goroutine
		// being woken up by a different case and it grabbing the
		// channel locks. Once it has the lock
		// it removes itself from the queue, so we won't see it after that.
		// We use a flag in the G struct to tell us when someone
		// else has won the race to signal this goroutine but the goroutine
		// hasn't removed itself from the queue yet.
		// 如果一个 goroutine 因为选择而被放到这个队列上，那么在被不同情况唤醒的 goroutine 和它抓取通道锁之间有一个小窗口。
		// 一旦它有了锁，它就会将自己从队列中删除，所以之后我们不会看到它。
		// 我们在 G 结构中使用一个标志来告诉我们其他人何时赢得了比赛来发出这个 goroutine 的信号，但 goroutine 还没有将自己从队列中删除。
		if sgp.isSelect && !atomic.Cas(&sgp.g.selectDone, 0, 1) {
			continue
		}

		return sgp
	}
}

func (c *hchan) raceaddr() unsafe.Pointer {
	// Treat read-like and write-like operations on the channel to
	// happen at this address. Avoid using the address of qcount
	// or dataqsiz, because the len() and cap() builtins read
	// those addresses, and we don't want them racing with
	// operations like close().
	// 将通道上的类似读和类似写操作视为在此地址发生。
	// 避免使用 qcount 或 dataqsiz 的地址，因为 len（） 和 cap（） 内置会读取这些地址，我们不希望它们与 close（） 等操作竞争。
	return unsafe.Pointer(&c.buf)
}

func racesync(c *hchan, sg *sudog) {
	racerelease(chanbuf(c, 0))
	raceacquireg(sg.g, chanbuf(c, 0))
	racereleaseg(sg.g, chanbuf(c, 0))
	raceacquire(chanbuf(c, 0))
}

// Notify the race detector of a send or receive involving buffer entry idx
// and a channel c or its communicating partner sg.
// This function handles the special case of c.elemsize==0.
func racenotify(c *hchan, idx uint, sg *sudog) {
	// We could have passed the unsafe.Pointer corresponding to entry idx
	// instead of idx itself.  However, in a future version of this function,
	// we can use idx to better handle the case of elemsize==0.
	// A future improvement to the detector is to call TSan with c and idx:
	// this way, Go will continue to not allocating buffer entries for channels
	// of elemsize==0, yet the race detector can be made to handle multiple
	// sync objects underneath the hood (one sync object per idx)
	qp := chanbuf(c, idx)
	// When elemsize==0, we don't allocate a full buffer for the channel.
	// Instead of individual buffer entries, the race detector uses the
	// c.buf as the only buffer entry.  This simplification prevents us from
	// following the memory model's happens-before rules (rules that are
	// implemented in racereleaseacquire).  Instead, we accumulate happens-before
	// information in the synchronization object associated with c.buf.
	if c.elemsize == 0 {
		if sg == nil {
			raceacquire(qp)
			racerelease(qp)
		} else {
			raceacquireg(sg.g, qp)
			racereleaseg(sg.g, qp)
		}
	} else {
		if sg == nil {
			racereleaseacquire(qp)
		} else {
			racereleaseacquireg(sg.g, qp)
		}
	}
}
