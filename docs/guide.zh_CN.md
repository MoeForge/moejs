# moejs 使用指南

每个函数的说明见[包文档](https://pkg.go.dev/github.com/MoeForge/moejs)，完整示例见 [README](../README.zh_CN.md)。

[English](guide.md)

## 运行时与运行时池

### 模块和运行时

`Module` 编译后不可修改，任意多个运行时可以同时加载同一个 `Module`。宿主一般每个插件编译一次，再为它维护一个运行时池。

同一时刻只有一个 goroutine 使用一个 `Runtime`。其他 goroutine 只能调用 `Interrupt` 和 `ClearInterrupt`。

### 把运行时还回池里

请求用完运行时后，宿主先调用 `rt.ReleaseCallData()`，再把运行时还回池里，空闲的运行时随即释放这个请求的参数。`Call`、`ToGo`、`Get`
和 `AppendJSON` 都可能运行 JavaScript，所以要等这几个函数的最后一次调用结束后再释放。

`Call` 把这一步留给宿主。这样一个请求运行多个钩子时，多个钩子共用一次数据转换。不用运行时池的宿主可以跳过这一步。

已经返回的值仍然有效。模块存下的数据会连同它引用的内容一起保持存活，所以模块从某个参数里留下一个对象，可能让整个参数一直存活。`json.RawMessage`
在转换时被复制，所以宿主可以把它的缓冲区留给下一个请求复用。模块在读取之前就存下的 Go map 或切片仍然指向宿主的值，其中的 `json.RawMessage`
字节也算在内，模块保留它的期间，这些值不能改动。

### 内存

在 `ReleaseCallData` 之前，运行时会保留每个请求的输入和输出。输入是 `ParseJSON` 复制的那份文本，对 `FromGo` 来说则是宿主自己的 Go 值，
运行时只引用它们，不复制。例外是 `json.RawMessage`，`FromGo` 会复制它的文本，以及 `FromGo` 通过 `json.Marshal` 文本转换的类型。所以活跃堆
随同时处理的请求数乘以单个请求的大小增长，宿主自己为每个请求保留的数据还要另算。`ParseJSONString`，以及对宿主的 map、切片和字符串调用
`FromGo`，都不会复制输入里不含转义的 ASCII 字符串，比如 base64 数据。非 ASCII 或不短于 64 KiB 的 Go 字符串先保留 UTF-8 原文，非 ASCII 或带转义的 JSON 字符串也同样先保留 JSON 原文。
JavaScript 第一次读取内容时才解码，非 ASCII 的转成 UTF-16，原文也继续保留；读长度不需要解码。设了内存上限时，解析得到的、以及 `FromGo` 为 Go
map 或切片里的值生成的这类字符串，在创建时就按解码后的大小记账；`FromGo` 或 `moejs.String` 单独转换的 Go 字符串不记账。

运行时还会通过引擎的暂存区，也就是解释器的寄存器和帧记录，让已结束调用的值保持可达，直到后面的调用覆盖它们。所以一次调用创建后又丢弃的值，
在调用返回后可能仍留在 Go 堆里；JavaScript 已经无法访问它，内存限制的测量也就看不到它。池化运行时的宿主在两个请求之间调用
`ReleaseCallData`，它会连同请求的数据一起释放这块暂存区。`eventloop` 包的 `Run` 在结束时释放暂存区，所以一个运行时依次跑多个事件循环程序时，
不会保留上一个程序的值。

在默认的 `GOGC=100` 下，Go 进程的常驻内存大约是活跃堆的两倍，因为回收器要等堆在活跃部分之外再增长同样多，才开始下一次回收。goroutine
很多，或者回收周期很长、又和其他任务争抢 CPU 时，常驻内存还会更高。

要限制内存，就把 `GOMEMLIMIT` 设在预计的活跃堆峰值之上并留出充足余量，因为这个上限计入的是 Go 运行时的全部内存，不只是堆，`GOGC`
保持默认值。这样在进程接近上限之前，吞吐量不受影响。`GOMEMLIMIT` 低于活跃堆时，回收器会一直运行，吞吐量大幅下降。调低 `GOGC` 则是在
整个进程范围内用吞吐量换内存。

async 函数调用返回的 promise 在调用返回或抛出之后只保留它的结果，任何时候都不保留这次调用的帧。调用停在 `await` 时，帧、帧里的局部变量和
它捕获的作用域由它等待的那个 promise 保留：如果调用在等一个永远不会兑现、也没有别处引用的 promise（`await new Promise(() => {})`），
即使它自己的 promise 还被持有，这次调用也会被回收。`then` 返回的 promise 在它来源的 promise 兑现之前会保留处理函数。要释放它们，
就丢掉这些 promise，或者兑现它们等待的 promise。

举个例子：8.1 MB 的 JSON 请求体，带五张 base64 图片，同时处理 32 个请求，`GOMAXPROCS=8`，宿主在请求结束前一直保留请求体（Intel i5-13500H，
Linux，go1.26.6，每次运行 10 秒，取 3 次的中位数）。用 `ParseJSON` 时，活跃堆约 500 MB，常驻内存约 1.1 GB。用 `ParseJSONString`
时分别约 330 MB 和 740 MB，每秒处理的请求数是前者的 1.5 倍。同样用 `ParseJSONString`，`GOMEMLIMIT=768MiB` 时每秒请求数不变，
`GOMEMLIMIT=256MiB` 低于活跃堆，每秒请求数下降 38%，`GOGC=25` 让每秒请求数下降 9%，常驻内存降到约 410 MB。

### 中断

任意 goroutine 都可以调用 `Interrupt(v)` 停止正在运行的代码，未完成的调用返回带有 `v` 的 `*InterruptedError`。脚本在下一个循环回边或函数调用处停下，
耗时长的内建函数在执行过程中检查中断。

没有代码运行时到达的中断会停止下一次调用，所以宿主复用运行时之前要调用 `ClearInterrupt`。

由事件循环（下文的 `eventloop`）运行的运行时要通过 `Loop.Interrupt` 中断。单独调用 `Runtime.Interrupt` 能停止正在运行的
JavaScript，但不会唤醒正在睡眠、等待下一个定时器的循环。

### 内存上限

`Options.MemoryLimit` 限制运行时的 JavaScript 保留的内存加上它分配的内存。运行时计的是上次重置以来的分配量，`ReleaseCallData` 会重置计数，
没有运行时池的宿主可以调用 `ResetAllocation`。计数超过上限时，运行时先测量程序保留了多少（见下文），再从这次测量结果开始计：产生垃圾而保留得少的程序，
分配再多也能继续运行。仍然超过上限的调用像被中断一样停下：返回 `*InterruptedError`，其值是 `*MemoryLimitError`，带有上限、已计的字节数和分配处的
JavaScript 调用栈，并且 `errors.Is(err, moejs.ErrMemoryLimit)` 成立。

```go
rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 64 << 20})
res, err := rt.Call(hook, arg)
if errors.Is(err, moejs.ErrMemoryLimit) {
	var mle *moejs.MemoryLimitError
	errors.As(err, &mle)
	log.Printf("plugin used %d bytes, limit %d\n%s", mle.Allocated, mle.Limit, mle.Stack)
}
rt.ClearInterrupt()
rt.ReleaseCallData() // 下一个请求用新的额度
pool.Put(rt)
```

脚本捕获不到这次超限。引擎的分配不会失败，所以停下一个不断分配的脚本只能靠中断；中断不运行任何 `catch` 和 `finally`，并丢弃排队的
promise 任务，否则脚本捕获之后可以继续分配。分配前就知道结果大小的内建函数，在大小超过剩余额度时改为抛出它们在超过引擎限制时抛出的
`RangeError`，脚本可以捕获：`repeat`、`padStart` 和 `padEnd`、`new ArrayBuffer`、类型化数组构造函数、`btoa`、`escape`、`toBase64` 和 `toHex`。

两次测量之间计的是分配量：在运行时测量之前，请求释放的内存不会扣除。计入的有对象、对象属性和元素增长出来的存储、字符串、
`Map` 和 `Set` 的表、缓冲区、`BigInt` 结果、闭包的环境，以及 `eval` 和 `Function` 编译出的代码，各按向 Go 分配器申请的大小大致计算。
rope（两个长字符串拼接的结果）先计它的节点，读取并展平时再计它的内容。`FromGo` 计入它为宿主的 map 和切片建立的包装，但不计宿主的 Go
值本身，因为它直接引用而不复制；`ParseJSON` 计入它复制的文本。几个大小有界的内部缓存不计入，解释器的寄存器栈也不计入：调用深度限制约束它的大小，`ReleaseCallData` 会收缩它。计数与请求实际分配的量相差在一个不大的倍数内，
设置上限时要留出余量。

准备工作不计入第一次请求：`Load` 和 `SetGlobal` 属于准备工作，成功时都会在结束前重置计数（`Load` 返回 `ErrModulePending` 时也会）。
失败的 `Load` 保留计数和 `Stats` 里的超限错误，供宿主读取。在宿主函数里调用的 `SetGlobal` 不重置，否则正在运行的脚本会得到新的额度。
每个请求都设置全局变量的宿主，要在请求的第一次 `Call` 之前设置：在同一请求的两个钩子之间重置，会让这个请求再分配一次上限的量。
重置保留上次的测量结果，所以模块在请求之间保留的内容仍然计入；在请求之间保留运行时的宿主，可以用下文的 `RebaseAllocation` 代替重置。

超限之后运行时仍可复用：`ClearInterrupt` 加 `ReleaseCallData` 之后，它和其他请求结束后的池中运行时一样。不信任模块状态（超限可能让它只更新了一半）的宿主可以丢弃这个运行时。
只清除中断而不重置，额度仍然用完，下一次分配会再次停下运行时。

超限产生的是运行时唯一的待处理中断，所以宿主从另一个 goroutine 调用的 `Interrupt`（比如超时）可能替换它，调用随后返回宿主的错误。
无论如何 `Stats().MemoryLimitHits` 都会计入这次超限，`Stats().LastMemoryLimitError` 保留它的错误直到下次重置或 `RebaseAllocation` 测量。

没有设置上限时运行时不计数，每个本该计数的分配点只多两次读取和两次分支。

重置约束的是一次请求分配多少。跨越许多任务长期运行的程序，比如不断等待工具结果的 agent 或一串定时器，可能每个任务都分配得不多，
整体却一直增长。`RebaseAllocation` 约束这类程序保留的内存：宿主在任务之间、原本调用重置的地方调用它，它测量运行时的 JavaScript
当前保留了多少，并以此作为额度的起点。此后上限计的是程序保留的量加上测量之后新分配的量。`Stats().LiveBytes` 是最近一次测量的结果。

```go
// 在宿主自己的事件循环的两个任务之间
err := rt.RebaseAllocation(func(add func(moejs.Value)) {
	for _, p := range pending { // 只有 Go 代码持有的 promise
		add(p)
	}
})
if err != nil {
	return err // 程序保留的内存超过了上限
}
```

测量遍历从运行时的全局对象、全局绑定、模块和排队的 promise 任务能到达的一切，按上限计算分配的方式计数。与 64 个字符以上的更长字符串共享字节的字符串，
如 `slice`、`split` 的片段或正则表达式的匹配结果，会让那个更长的字符串活着，所以按它计一次；设置了上限时 `JSON.parse` 或
`ParseJSONString` 从长文本中读出的值也一样。只由数据给出、长于 64 字节的属性名（计算属性名、`JSON.parse` 的键）留在运行时自己的
形状里，测量把它们和运行时为对象保留的形状一起计入，运行时销毁时它们也随之释放；更短的属性名可能进入进程内所有运行时共享的形状，
这部分有上界，不计入。编译后代码中的字符串常量由运行它的所有运行时共享，也不计入。

只有 Go 代码持有的值它看不到：`NativeFunc` 闭包捕获的值，以及 `NewPromise` 的 promise（只有它的 settle 函数持有它，等待它的
async 函数也只能经由它到达）。宿主通过传给 `RebaseAllocation` 的函数把这些值交给测量，这个函数只在真正测量时才被调用；也可以注册
一个函数，在每次测量时加入这些值：

```go
// pending 是这个宿主稍后才 settle 的 NewPromise 的 promise。
remove := rt.AddLiveRoots(func(add func(moejs.Value)) {
	mu.Lock() // 只有宿主自己的 goroutine 持有的锁
	defer mu.Unlock()
	for _, p := range pending {
		add(p)
	}
})
defer remove()
```

没有上限时 `AddLiveRoots` 没有开销：这时除了 `Realm().MeasureLive` 不会有测量。注册的函数在使用运行时的 goroutine 上运行，可能在
JavaScript 执行中途被调用（见下文），所以它只调用 `add`：不运行 JavaScript，不创建值，也不等待运行时执行 JavaScript 时被持有的锁。

会回调 JavaScript 的宿主函数，在这次调用期间不能把 JavaScript 值只放在 Go 里，要么注册它们，因为测量可能在它回调的 JavaScript 里进行。
在 Go 里建出一个大值的宿主函数（包括作为 getter 的宿主函数）会越过上限把它返回：建的过程中没有测量能看到它，下一次测量，最迟是下一次
rebase，才会发现程序超过了上限。

注册的函数在被移除之前一直计入它添加的内容，不管 JavaScript 是否还能到达，所以它适合只有 Go 持有的东西：进行中的请求、只有宿主会兑现的
promise。宿主创建的对象的 Go 状态所保留的内容（流的队列、请求体的字节、以 Go 字符串保存的 header 值）改为通过对象报告：对象的负载
（`Object.SetInternal`）实现 `moejs.LiveReporter` 时，到达这个对象的测量会调用它一次，把它报告的值当作对象自己的值遍历，并计入它报告的
Go 内存字节数。这些状态在对象可达时计入；JavaScript 丢弃对象后，下次测量就不再计入，不必等 Go 的垃圾回收。

```go
type queueState struct {
	chunks []moejs.Value // 流保留的 JavaScript 值
	buf    []byte        // 它保留的 Go 字节
}

func (q *queueState) ReportLive(add func(moejs.Value), addBytes func(n int)) {
	for _, c := range q.chunks {
		add(c)
	}
	addBytes(cap(q.buf))
}

o := rt.Realm().NewObject()
o.SetInternal(&queueState{})
```

`ReportLive` 遵守 `AddLiveRoots` 所注册函数的规则：只调用 `add` 和 `addBytes`。

测量要遍历堆，所以有节奏地进行：只有当上次测量以来的分配量达到上限在上次测量结果之上剩余部分的一半
`(MemoryLimit - LiveBytes) / 2` 与上次测量结果的一半二者中较大的那个时，`RebaseAllocation` 才测量，否则立即返回，不分配内存。
保留得少的程序每分配半个上限测量一次；接近上限的程序测量得更频繁，但不会比每分配所保留量的一半更频繁。如果这样在下次测量之前
就有一次分配会超过上限（不论之前有没有测量过），运行时会先用模块的绑定和注册的根测量，只有程序保留的仍然太多时才停下。这种测量在两次
`RebaseAllocation` 或 `ResetAllocation` 之间做一次，此后每当上次测量以来的分配量达到决定 `RebaseAllocation` 测量节奏的那个量时再做一次，
因此一次产生大量垃圾的调用能像跨越多次 rebase 那样继续运行。

测量只在没有代码能把自己建出的东西藏在遍历之外的地方进行：普通 JavaScript 里，以及 `forEach`、`some`、`every`、各个 `find` 方法、
`map`、`filter`、`flatMap`、`reduce`、`sort`、`replace` 和 `replaceAll`、`Array.from`、`JSON.parse` 的 reviver、生成器的 `next`
和宿主函数的回调里，这些函数把建出的东西放在遍历能找到的地方。其他内建函数运行期间，测量会等待：
这些代码可能把建出的东西放在 Go 变量里，比如 `JSON.parse` 正在建的树、`JSON.stringify` 正在写的文本。这期间它的分配最多
可以超过上限达到上限在上次测量结果之上剩余部分的一半，再加上越过这个余量的那一次分配（比如数组增长时的存储），之后运行时会在内建函数
返回后或下一次调用时测量，两个 promise 任务之间也会测量；超过这个余量的内建函数会停下，所以对上限来说太大的文本做 `JSON.parse`
会在这个余量之内停下。解析器在数组或对象闭合之前把元素放在自己的栈上，这些栈增长时就计入上限，所以几百万个数字组成的数组和由对象组成的数组一样会停下。由内建函数调用组成的循环，一旦有一次调用超过上限，就会在每次迭代时测量。一次运行如果以超过上限的分配结束（脚本的最后一条语句，或忽略了计费错误的宿主函数），这次运行本身就会失败，有待处理的测量会先做完：超出的部分不会在没有任何提示的情况下让下一次运行停下；和任何一次超限之后一样，宿主清除中断并重置额度后，下一次运行就从干净的状态开始。`ParseJSON`、`ParseJSONString` 和 `FromGo`
也同样等待。宿主自己在任何调用之外的 Go 代码做的分配，以及它在没有脚本运行处（事件循环任务里，或 Go 直接调用的宿主函数里）调用的 `ChargeMemory`，
会立即测量，和 `RebaseAllocation` 一样：宿主在调用之间持有的东西，由它用 `AddLiveRoots` 注册。遍历用的工作存储在结束后交还给进程，
供任何运行时的下次测量使用：运行时自己不保留。于是接近上限时，程序每分配几兆字节就要付出一次遍历其保留内容的代价：一个分配 64 KiB 垃圾的回调，
在 64 MiB 上限、保留的数据为小对象时，保留 75 % 时的开销约为 1.3 倍，95 % 时 2.5 倍，98 % 时 3.7 倍。测量结果超过上限时，
`RebaseAllocation` 返回 `*MemoryLimitError`（`Live` 为测量结果，`Allocated` 为 0）并计一次超限，但不中断运行时，反正下一次分配也
会超过上限。测量期间的 `Interrupt` 会结束遍历，这次测量不改变任何状态；有待处理的中断时它不测量。

`ResetAllocation`、`ReleaseCallData`、`Load` 和 `SetGlobal` 保留测量结果：程序保留的内容在它们之后仍然计入。按请求重置又保留运行时的
宿主，也在每个请求开始时调用 `RebaseAllocation`，上次测量以来所有请求的分配量决定下次何时测量。

在 `eventloop` 下，循环自己完成这些工作。额度覆盖整个 `Run`（或从 `Start` 到 `Stop`），循环在每个定时器或 immediate 回调、
每个任务之前调用 `RebaseAllocation`，并通过 `AddLiveRoots` 注册待运行的定时器、immediate 和未兑现的 `NewPromise`，`Terminate` 会移除这些注册，
因此换了新循环的运行时不会留住旧循环。保留自己所分配内存
的程序，无论经过多少个回调或多少次运行，都会在接近上限时停下；保留得少的程序，总共分配再多也能继续运行。循环测量时不持有它的
互斥锁，投递任务或 settle promise 的 goroutine 不必等待遍历；没有上限时循环不为此做任何事。超限像中断一样停下循环：`Run` 返回
`*InterruptedError`，`errors.Is(err, moejs.ErrMemoryLimit)` 成立。设置 `eventloop.Options.ResetMemoryPerTask` 后，循环改为像以前的版本那样
在每个回调和任务之前给出新的额度：上限约束的是一个宏任务及它排队的 promise 任务分配多少，而不是程序保留多少。

### 计数器

`rt.Stats()` 返回运行时的计数器，每项都是常数时间读取。宿主在使用该运行时的 goroutine 上读取，可以在请求之间，也可以在请求中的宿主函数里：

- `AllocatedBytes`、`Objects`、`Strings`、`Shapes`：设置上限以来的计数，`RequestAllocatedBytes` 是上次重置或 `RebaseAllocation` 测量以来的。只在设置了上限时计数；
  上限设为 `math.MaxInt64` 可以只计数不限制。
- `LiveBytes`：`RebaseAllocation` 最近一次测量的结果，重置不会清除它；还没有测量时为 0。
- `MemoryLimitHits` 和 `LastMemoryLimitError`，见上文。
- `Interrupts`：`Interrupt` 的调用次数，包括超限。
- `ICEntries`：运行过的函数的内联缓存条目数。
- `PendingJobs`：排队的 promise 任务和微任务。
- `RegisterStackBytes`：解释器寄存器栈的大小。

运行时池可以在收回运行时时导出它们：

```go
st := rt.Stats()
metrics.Observe("plugin_request_bytes", st.RequestAllocatedBytes)
rt.ReleaseCallData()
```

宿主函数可以把它们交给模块：

```go
rt.SetGlobal("memoryUsage", moejs.NativeFunc(func(r *moejs.Realm, this moejs.Value, args []moejs.Value) (moejs.Value, error) {
	return moejs.Int(r.Stats().RequestAllocatedBytes), nil
}))
```

自己为 JavaScript 分配内存的宿主函数也可以计入，调用 `Realm.ChargeMemory(n)`，超过上限后它返回中断的错误。

### 对象属于一个运行时

运行时里的 JavaScript 创建的对象属于这个运行时。使用另一个运行时的对象可能运行那个运行时的代码，所以运行时之间只传 Go 值和 JSON。

`Call`、`CallFunction`、`SetGlobal`、`FromGo` 和 `NewPromise` 的 settle 函数遇到属于另一个运行时的函数、生成器、绑定函数或代理时，
返回 `ErrForeign`。
在 `FromGo` 转换的 Go 容器里读到这样的函数，会抛出消息与 `ErrForeign` 相同的 `TypeError`。

检查覆盖传入的值和 Go 容器的成员，跳过 JavaScript 对象的属性，也跳过另一个运行时里不可调用的代理，即使这个代理放在 Go map 里。
读取这种代理时，它的陷阱在读取它的运行时里运行。

## 代码

### 钩子

`Module.Hook(export, members...)` 一次性解析一个导出函数，或者导出对象下面的函数，例如 `mod.Hook("protocols", "openai", "decodeRequest")`。
绑定是活的：每次 `Call` 都在当前运行时里读取这个导出，再按自有属性逐级找到成员。

路径中途找不到时返回 `ErrHookNotFound`，路径终点的值不是函数时返回 `ErrNotCallable`。这两种情况下 `Has` 都返回 false。

### 函数值

`CallFunction(fn, this, args...)` 调用宿主之前拿到的函数值，比如插件传给宿主函数的回调。它和 `Call` 做同样的检查：
不是函数的值返回 `ErrNotCallable`，属于另一个运行时的函数返回 `ErrForeign`，宿主函数运行之前先检查中断。
调用排入的任务在它返回之前运行；在宿主函数里调用时，这些任务留到最外层调用结束时运行。

### 模块图

导入了其他模块的模块，由宿主先链接，再交给运行时加载。`moejs.Link(entry, resolve)` 向宿主的 `Resolver` 查询每个说明符对应的模块，
同一模块里的同一说明符查询一次，然后一次性链接整个模块图。模块图里的每个模块都来自解析器。

`Link` 返回要加载的 `Module`。它和其他模块一样不可修改，可以共享。它的 `Hook`、`Export` 和 `Exports` 指向入口模块的导出，包括再导出的名称。

解析器收到的 `referrer` 是发起导入的 `*Module`，类型是 `Referrer`。`Referrer` 是密封接口，表示请求模块的代码，取值是 `*Module` 或 `*Script`。

每个运行时实例化并求值链接好的模块图，每个模块求值一次。多个模块图共用的模块，只要解析器每次返回同一个 `*Module`，就只编译一次。

解析失败返回 `*ResolveError`。导入没有解析到唯一的绑定时返回 `*SyntaxError`。两种错误都带有导入方模块里的位置。`Load`
遇到有导入但没有链接过的模块时返回错误。没有导入的模块可以直接交给 `Load`。

```go
entry, err := moejs.Compile("plugin.js", source)
mod, err := moejs.Link(entry, func(referrer moejs.Referrer, specifier string) (*moejs.Module, error) {
	return host.module(specifier) // 每个进程只编译一次
})
err = rt.Load(mod) // 每个运行时
```

### 动态导入

`Options.Importer` 是宿主为 `import()` 和 `import.meta` 提供的实现。它的 `Resolve` 是一个 `Resolver`，收到的 `referrer` 是发起导入的
`*Module`，或者 `RunScript` 运行的 `*Script`。可选的 `Meta` 填写模块的 `import.meta`。`import.meta` 是首次使用时创建的对象，原型为 null。

`import(specifier)` 立即调用 `Resolve` 并返回一个 promise。promise 兑现为模块的命名空间对象，解析、链接或求值失败时以对应的错误拒绝。
没有设置 `Importer` 时，它以 `TypeError` 拒绝。

一个运行时对同一个模块只求值一次，静态导入和动态导入都算在内。`import()` 加载的模块图，每个 `Importer` 链接一次。任意多个运行时可以共用一个
`Importer`，每个运行时只做实例化和求值。任务队列和中断的行为与 `Load`、`Call` 相同。`import()` 和 `import.meta` 的开销只落在用到它们的代码上。

```go
imp := &moejs.Importer{Resolve: func(referrer moejs.Referrer, specifier string) (*moejs.Module, error) {
	return host.module(specifier)
}}
rt := moejs.NewRuntime(moejs.Options{Importer: imp}) // 所有运行时共用一个 imp
```

### TypeScript

`CompileTS(name, source)` 编译一个 TypeScript 模块。解析器边读边丢掉类型语法，做法和 Node 的类型剥离一样，模块按剩下的 JavaScript 运行。
不做类型检查。

解析器删掉的内容：

- 类型注解、类型参数和类型实参，`as`、`satisfies`、`x!` 和 `<T>x`；
- `interface` 和 `type` 声明，`declare` 之后的一切，函数重载签名，以及只含类型的 namespace；
- `import type`、`export type` 和标了 `type` 的说明符；
- `this` 参数，它不计入函数的 `length`；
- 类成员的 TypeScript 修饰符（`public`、`private`、`protected`、`readonly`、`override`、`abstract`、`declare`）、索引签名和没有函数体的方法。
  `declare` 或 `abstract` 字段什么也不产生。只有类型的字段仍是字段，初始值为 `undefined`，和 tsc 面向 ES2022 及以后的输出一致。

有运行时语义的 TypeScript 返回 `*SyntaxError`，错误信息会写明是哪种语法：`declare` 之外的 `enum` 和 `const enum`、带值的 namespace、
构造函数参数属性（`constructor(private x: T)`）、`import x = require("m")`、`export import x = A.B`、当作值使用的别名
`import x = A.B`、`export =`、`export as namespace` 以及 `accessor` 字段。装饰器和 JavaScript 里一样不支持。也不支持 TSX。

和 tsc 一样，编译器会删掉没有任何表达式读取的导入说明符，以及删完后没有绑定的导入声明。`import "m"` 保留。类型里的引用（包括
`typeof x`）不算读取。`T` 只是类型时，`export { T }` 和 `export default T` 也会被删掉。所以模块可以按名字从同一个包导入类型和值，
宿主为这个包提供的模块只需要导出值。

错误和栈追踪指向 TypeScript 源码，不需要 source map。`Function.prototype.toString` 返回函数的 TypeScript 源码，包括类型；
Node 返回的是类型被替换成空白的文本。

编译时间和源码长度成正比，与嵌套深度无关；嵌套超过 10,000 层返回 `*SyntaxError`（"Nesting too deep (the parser allows 10000 levels)"）。
宿主编译不可信的源码时仍应限制长度，也可以限制编译时间：`CompileWith`、`CompileScriptWith` 和 `FunctionOptions` 的 `Context`
接受一个 `context.Context`，它结束后编译随即停止并返回它的错误。解析器、作用域分析和编译器在每个语句列表中每 1,024 条语句、在其他
列表（数组和对象字面量的元素、实参、形参、一串二元运算符，联合、交叉类型和接口、对象类型、元组类型的成员）中每 1,024 项检查一次，所以一条很长的
语句也会同样及时地停下。`Runtime.Interrupt` 不能中止宿主自己调用的编译。

```go
ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
defer cancel()
mod, err := moejs.CompileWith("plugin.ts", src, moejs.CompileOptions{TypeScript: true, Context: ctx})
if errors.Is(err, context.DeadlineExceeded) {
	// 编译超时
}
```

`Resolver` 或 `Importer` 可以按模块选择编译函数，比如按文件扩展名。`CompileTS` 和 `Compile` 编译的模块可以互相链接。

```go
resolve := func(referrer moejs.Referrer, specifier string) (*moejs.Module, error) {
	src, err := host.read(specifier)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(specifier, ".ts") {
		return moejs.CompileTS(specifier, src)
	}
	return moejs.Compile(specifier, src)
}
```

### 脚本

`CompileScript(name, source)` 把经典脚本编译成不可修改的 `*Script`。脚本以 `"use strict"` 指令开头时是严格模式，其余是非严格模式。
`Runtime.RunScript` 在运行时的全局环境里运行它，返回它的完成值。

脚本里的 `var` 和函数声明成为全局对象的属性。`let`、`const` 和 `class` 声明成为全局绑定，之后运行的脚本和已加载的模块都能看到。
声明和已有的全局绑定冲突时，脚本在运行任何代码之前抛出错误。

`SetGlobal` 写入全局对象。脚本声明 `let x` 之后，这个全局词法绑定会遮蔽 `SetGlobal("x", …)` 设置的值（ECMA-262 9.1.1.4.1）。

### 从源码编译函数

`CompileFunction(name, params, body, opts)` 和 `Function` 构造器一样，从参数列表和函数体的源码编译出一个函数；`CompileFunctionTS` 对 TypeScript 做同样的事：

```go
s, err := moejs.CompileFunctionTS("rule.ts", "input: Input, limit?: number",
	"type N = number;\nreturn input.messages.slice(0, (limit ?? 10) as N);",
	moejs.FunctionOptions{})
fn, err := rt.RunScript(s) // 得到函数
res, err := rt.CallFunction(fn, moejs.Undefined(), arg)
```

`params` 是不带括号的参数列表，源码文本是 `function anonymous(params\n) {\nbody\n}`，所以函数体里的 `*SyntaxError` 在第 3 行或之后。
参数和函数体都不能提前结束对方。脚本不声明任何东西：`RunScript` 在全局环境里创建这个函数并返回它，每个运行这个脚本的运行时都得到自己的函数。
`FunctionOptions` 可以要求 async 函数、生成器函数，或两者都要。

`CompileFunction` 的函数和 `Function` 构造器的一样，除非函数体以 `"use strict"` 开头，否则是非严格模式。`CompileFunctionTS` 遵循
`CompileTS` 的规则：类型被擦除，有运行时语义的 TypeScript 返回 `*SyntaxError`，`toString` 返回 TypeScript 源码，函数是严格模式代码，和模块代码一样。

### Eval

导入 `moejs` 包就会安装编译器，`eval`、`Function` 系列构造函数和 `Realm.EvalScript(name, source)` 都用它编译。`NativeFunc` 可以对收到的
`*Realm` 调用 `EvalScript`，从字符串运行经典脚本，作用和 test262 的 `$262.evalScript` 一样。

直接 `eval` 能看到调用方的绑定，包括模块的导入。被求值的代码和它创建的函数，在栈追踪里以发起求值的脚本或模块为来源。这些代码里的 `import()`
也把这个脚本或模块作为 `referrer` 传给 `Resolver`。如果代码来自间接 `eval` 或 `Function` 系列构造函数，而发起调用的脚本或模块既没有用
`import()`、`import.meta`，也没有直接 `eval`，`referrer` 为 nil（见 [TODO.md](../TODO.md)）。

`eval` 和 `with` 的开销只落在用到它们的代码上。

### 动态代码的限制

`Options.MaxDynamicSource` 限制 `eval`、`Function` 系列构造函数和 `Realm.EvalScript` 能编译的源码长度。默认上限是 1 MiB（按 UTF-8 计），
设为负数表示不限制。超过上限的源码在解析之前抛出 `RangeError`。编译的时间和内存都随源码长度线性增长，这个上限同时限制了两者。
编译进行中也可以被中断。

`Options.DisableDynamicCode` 为一个运行时关闭动态代码，这时 `eval`、`Function` 系列构造函数和 `EvalScript` 都抛出 `EvalError`。

这两个选项作用于运行时里的动态代码。宿主自己调用的 `Compile` 和 `CompileScript` 照常编译。

### 代码规模的上限

一个函数（或一个脚本、一个模块的顶层）的字节码至多用 256 个寄存器、65,536 个常量（名字、字符串，以及 -32,768 到 32,767 以外的数）、
65,536 处属性和全局变量访问、65,536 个嵌套函数，每条跳转跨过的指令不超过 32,767 条。超过上限的代码不能编译：`Compile`、
`CompileScript` 等返回指明该上限的 `*SyntaxError`，`eval` 和 `Function` 系列构造函数抛出它。脚本声明的名字与常量分开存放，
所以 100,000 个 `var` 声明的脚本可以运行；但 100,000 个 `var x = …` 不行，因为每次赋值都要用到它的全局名字。
这么大的代码拆成多个函数、脚本或模块就能编译，每一部分各有自己的上限。

`RunScript`、`Load` 和 `Link` 遇到 nil 的 `*Script` 或 `*Module`（编译失败时返回的就是 nil）返回错误。

## 值

### 传入 Go 值

`FromGo` 能转换这些 Go 类型：`nil`、布尔、数字、字符串、`json.Number`、`*big.Int`（转成 BigInt）、`Value`、`NativeFunc`，以及 JSON 形态的容器
`map[string]any`、`[]any`、`map[string]string`、`[]string`、`map[string][]string` 和 `[]map[string]any`。

`FromGo` 惰性转换容器：JavaScript 第一次读取时才转换，每次转换一层，所以钩子只为读到的那部分参数付出开销。JavaScript 写入时改的是 JavaScript
对象，Go 值保持不变。在 JavaScript 还可能读取的期间，宿主必须保持这个值不变。

由 Go map 转换来的对象按排序后的顺序枚举键。

`[]byte` 转换成共享同一段字节的 `ArrayBuffer`，所以 JavaScript 的写入会改到 Go 切片。

底层类型在上面列表中的命名类型按底层类型转换：`type Settings map[string]any` 转成惰性转换的 map，`type Status string` 转成字符串。

`encoding/json` 能写出的其他类型，比如结构体、指针、`map[string]int` 和 `[]map[string]string`，从它们的 `json.Marshal` 文本转换，
由引擎解析一次。结构体标签和 `MarshalJSON` 方法的效果和 `json.Marshal` 相同，`[]byte` 字段变成 base64 字符串。结果是一个快照：在 `FromGo`
运行时生成；放在 Go map 或切片里的值，在 JavaScript 第一次读取所在容器时生成。之后对结构体的修改看不到。

通道、`NativeFunc` 以外的函数、复数，以及 `json.Marshal` 拒绝的值会返回错误。`error`，以及 `json.Marshal` 一个字段都写不出的结构体（比如
`sync.Mutex` 或 `context.Context`）也会返回错误，除非类型有 `MarshalJSON` 或 `MarshalText` 方法：`json.Marshal` 会把它们写成 `{}`，或者写成
说明不了什么的文本。错误请传 `err.Error()`。完全没有字段的结构体是 `{}`。在容器里时，读取这样的成员会抛出 `TypeError`。

`SetGlobal(name, v)` 用同样的方式转换 `v`，所以宿主可以用 `NativeFunc` 组成的 `map[string]any` 安装一组函数。`Function(name, length, fn)`
包装一个 `NativeFunc`，并设置 JavaScript 看到的 `name` 和 `length`。

### 传入 JSON

`ParseJSON(b)` 对这些字节执行 `JSON.parse`，适合宿主以 JSON 形式保存的参数，比如存下的任务数据或请求体。它会先把字节复制成字符串，所以它返回后
宿主就可以复用这些字节。

`ParseJSONString(s)` 解析一个字符串，字符串是有效的 UTF-8 时不做这次复制，结果里的字符串可能和 `s` 共用内存。宿主以 `[]byte` 持有
文本时，可以传入 `unsafe.String(unsafe.SliceData(b), len(b))`，前提是在 `ReleaseCallData` 之前，以及宿主或模块还保留着由这些字节得出的
值的期间，从不修改这些字节。没有任何检查保证这一点，由宿主负责。

`FromGo` 也接受 `json.RawMessage`，可以单独传入，也可以作为 `map[string]any` 或 `[]any` 的成员，JavaScript 拿到的是对其文本执行
`JSON.parse` 的结果。转换用的是文本的副本：单独传入时在 `FromGo` 运行时复制，作为成员时在 JavaScript 第一次读取所在容器时复制。之后宿主
可以复用这些字节。对象或数组的文本在转换时检查，在 JavaScript 第一次读取这个值时解析，所以从不读取它的钩子不用付解析的开销。其他文本立即
解析。无效的文本是 `SyntaxError`：顶层的值由 `FromGo` 返回这个错误，容器里的值在读取成员时抛出。nil 的 `json.RawMessage` 是 `null`。
JavaScript 没有读取过的 `json.RawMessage` 由 `ToGo` 返回为一个带有这段文本的新 `json.RawMessage`。

`json.RawMessage` 的类型化容器，比如 `[]json.RawMessage`、`map[string]json.RawMessage`、`*json.RawMessage` 或结构体字段，和其他类型一样
经过 `json.Marshal`，并立即解析。

钩子一定会读取的文本用 `ParseJSON` 或 `ParseJSONString` 更省：它们只解析一次，`ParseJSON` 也只复制一次。`json.RawMessage` 先复制并检查
文本，钩子读取时才解析，所以适合钩子不一定读取的文本。

### 读出 Go 值

`ToGo` 的转换规则：

- 整数转成 `int64`，其他数字转成 `float64`
- 数组转成 `[]any`
- 对象转成由自有可枚举属性组成的 `map[string]any`
- `Date` 转成 `time.Time`
- BigInt 转成 `*big.Int`
- `ArrayBuffer`、类型化数组和 `DataView` 转成 `[]byte`，内容是它持有或指向的字节的副本

`FromGo` 转换的参数只要没被 JavaScript 修改过，就原样返回最初的 Go 值。

`Get(v, key)` 读取一个属性，会执行 getter。`Export(name)` 读取已加载模块某个导出的当前值。

### 读出 JSON

`AppendJSON` 执行 `JSON.stringify`，把文本追加到字节切片。它的输出和对 `ToGo` 的结果做 `json.Marshal` 有几处不同：值为 `undefined`
的成员会省略，NaN 和 ±Infinity 写成 `null`，键按插入顺序输出，并且会调用 `toJSON`。

`AppendJSON` 按上次输出的长度再加八分之一预留空间，写进切片的剩余容量。宿主把上次的输出传回去（`buf, err = rt.AppendJSON(buf[:0], v)`）时，
有两种情况会分配内存：切片剩余空间不够这个大小，或者值里有可能运行代码的部分。可能运行代码的部分包括 `toJSON` 方法（`Date` 的也算）、getter、
代理、BigInt，以及原型不是 `Object.prototype` 或 `Array.prototype` 的对象或数组，比如类实例、`Map` 和 `Object.create(null)`。遇到这些值时，
输出先写进一个随写入增长的独立缓冲区，最后再追加到切片。

输出最长 2^30−24 字节。

### 严格模式输出 JSON

`AppendJSON` 遵循 `JSON.stringify`，后者会悄悄丢掉一些值：对象里值为 `undefined`、函数或 symbol 的成员会省略，数组里这样的元素写成 `null`，
NaN 和 ±Infinity 写成 `null`。宿主如果宁可失败也不愿发出被改动的数据，可以使用严格模式：

```go
out, err := rt.AppendJSONWith(buf[:0], v, moejs.JSONOptions{Strict: true})
var lossy *moejs.LossyJSONError
if errors.As(err, &lossy) {
	// lossy.Path 是 "$.messages[3].content"，lossy.Value 是 "undefined"
}
```

严格模式遇到上面这些值都会失败；值本身没有 JSON 文本时（`AppendJSON` 会写 `null`）也会失败；检查的是 `toJSON` 返回的值，所以返回
`undefined` 的 `toJSON` 同样失败。JSON 看不到其数据的对象也会失败，`JSON.stringify` 把它们写成 `{}` 或只写出它们自身的可枚举属性：
`Map`、`Set`、`WeakMap`、`WeakSet`、`WeakRef`、`Promise`、`RegExp`、任何种类的 `Error`（包括继承它的类）、`ArrayBuffer`、
`SharedArrayBuffer` 和 `DataView`。这时错误的 `Value` 是 "a Map"、"an Error" 等，`Object` 为 true，消息形如
`moejs: $.cache is a Map, whose contents JSON does not write`。这类对象自己带有 `toJSON` 方法时按该方法的返回值写出；类型化数组的元素
JSON 会写出，所以不受影响。错误的 `Path` 从 `$` 开始，属性写成 `.name` 或 `["name"]`，元素写成 `[i]`；错误消息形如
`moejs: $.messages[3].content is undefined, which JSON leaves out`。出错时切片原样返回。-0 和非严格模式一样写成 `0`（两者相等）。
严格模式不增加 `AppendJSON` 的开销；开启后，`FromGo` 转换来的 Go map 或切片按 JavaScript 看到的值写出，而不是直接从 Go 值写出。

### 写入宿主自己的类型

`Unmarshal(v, target)` 把值写进一个 Go 值，结果和对 `AppendJSON` 的文本做 `json.Unmarshal` 相同。普通对象、数组和没有被修改过的参数直接写进
Go 值，跳过文本和字符串复制。其他值，比如带 `toJSON` 的值、getter，或者自己实现反序列化的目标类型，会经过文本。

`AppendJSON` 会失败的情况下（BigInt、循环引用、抛出异常的 `toJSON`、无法写出的 Go 值、超过长度上限的文本），`target` 的内容保持不变。
`json.Unmarshal` 返回错误时，`target` 里是出错之前已经写入的部分。

`ToGoInto(v, target)` 的结果和错误，与依次执行 `ToGo`、`json.Marshal` 和 `json.Unmarshal` 写进 `target` 相同。已经在用这三步的宿主可以
直接换成它，行为不变。和 `Unmarshal` 一样，普通对象、数组和没有被修改过的参数直接写进 Go 值，不生成 `ToGo` 的 Go map，也不生成文本。getter、
代理、`Date`、`Map`、类型化数组、BigInt、函数，以及自己实现反序列化的目标类型，会改走这三步。`ToGo` 或 `json.Marshal` 会失败的情况下
（抛出异常的 getter、NaN 或 ±Infinity、循环引用），`target` 的内容保持不变。

`ToGo` 和 `AppendJSON` 结果不同的地方，`ToGoInto` 和 `ToGo` 一致：值为 `undefined` 的成员保留为 `null`，-0 保持 -0，NaN 和 ±Infinity
返回错误，不调用 `toJSON`。和 `ToGo` 一样，只有 getter 运行时才会被中断。

目标里的 `json.RawMessage` 收到值中对应部分的 JSON 文本，目标的其余部分仍然直接写入。它可以是字段、指针、map 的值、切片元素或目标本身。
写入的字节和往返时得到的相同：`Unmarshal` 写入 `AppendJSON` 的文本；`ToGoInto` 写入对 `ToGo` 结果做 `json.Marshal` 的文本，键已排序，
`<`、`>` 和 `&` 已转义。和 `json.Unmarshal` 一样，文本放得下时写进 `json.RawMessage` 自己的数组。对于 `ToGoInto`，钩子未读取就返回的
`json.RawMessage` 参数不会被解析：目标收到的是 `json.Marshal` 压缩后的文本。其他自己实现反序列化的类型仍然让整个值走往返。

### 限制结果的大小

`UnmarshalWith`、`ToGoIntoWith` 和 `ToGoWith` 接受 `DecodeOptions`，限制结果的 JSON 文本。`MaxBytes` 限制文本长度，`MaxNodes` 限制其中值的
个数：对象、数组、字符串、数字、布尔值和 null，键不计入。这里的文本是往返时写出的文本：`UnmarshalWith` 用 `AppendJSON` 的文本，另外两个用
对 `ToGo` 结果做 `json.Marshal` 的文本。计数是精确的：文本为 `n` 字节的结果能通过 `MaxBytes: n`，通不过 `MaxBytes: n-1`。字段为零表示
不限制。

超过限制的结果返回一个包装了 `ErrTooLarge` 的错误，`target` 的内容保持不变。不需要运行 JavaScript 就能读取的值，即普通对象、数组和来自
`FromGo` 的参数，在解码任何内容之前计数，不写出文本。钩子未读取就返回的 `json.RawMessage` 参数是 `UnmarshalWith` 的唯一例外：
`AppendJSON` 写出的是它解析成的值的文本，所以要先解析才能计数，除非对它的一次快速扫描已经说明文本超过限制。`ToGoIntoWith` 和 `ToGoWith`
不解析就能计数它的文本。需要运行 JavaScript 才能读取的值，比如 `toJSON`、getter 或代理，在往返写出文本之后计数（`ToGoWith` 则在 `ToGo`
得出结果之后计数），但仍在解码任何内容之前。循环引用和超过 10,000 层的嵌套超过所有限制。

不设限制时，这几个函数就是 `Unmarshal`、`ToGoInto` 和 `ToGo`，开销相同。

### 请求之后还要保留的字符串

`ToGo`、`Unmarshal` 和 `ToGoInto` 从 `ParseJSON` 或 `ParseJSONString` 生成的值或 `json.RawMessage` 参数里返回的字符串，可能和解析的文本
（对 `ParseJSONString` 来说是宿主的字符串，对 `json.RawMessage` 来说是运行时复制的那份文本）共用内存，并让整段文本一直存活。对字符串值
调用 `String()` 得到的字符串也是这样。宿主要长期保留的字符串，先用 `strings.Clone` 复制一份。

### 嵌套深度

数组和对象嵌套超过 10,000 层时，`ParseJSON`、`ParseJSONString`、`ToGo` 和 `AppendJSON` 返回 `RangeError`，`JSON.parse` 和 `JSON.stringify` 抛出 `RangeError`。错误消息说明了原因（"JSON nesting is too deep (more than 10000 levels)"），`errors.Is(err, moejs.ErrTooDeep)` 可以识别它。

## Promise 与错误

### Promise

`NewPromise` 给宿主函数一个可以返回的 promise，同时给出稍后兑现或拒绝它的 Go 函数。`PromiseResult` 读取 promise 的状态和结果。
`SetPromiseRejectionTracker` 报告没有被处理函数接住的拒绝，和 Sobek 的同名接口一样。

`ThrownValue(err)` 返回宿主函数返回 `err` 时抛出的值。宿主用它拒绝 promise，JavaScript 就拿到同样的 `Error`；
这个值作为 `*Exception` 回到宿主时可以解包出 `err`。

settle 函数只能在使用运行时的那个 goroutine 上调用。要从其他 goroutine 兑现 promise，用下面的 `eventloop` 包。

### 错误

JavaScript 抛出的异常以 `*Exception` 返回。`Name()` 和 `Message()` 读取抛出值的 `name` 和 `message` 数据属性，抛出的是原始值时就用这个值本身。
这两个方法都不执行 JavaScript。

宿主函数返回 Go error 时，JavaScript 里会抛出一个 `Error`，消息是这个错误的文本。对应的 `*Exception` 可以解包出原来的 Go error。

中断以 `*InterruptedError` 返回。中断的值是 error 时，它可以解包出这个值，所以调用 `Interrupt(context.Cause(ctx))` 的宿主可以用
`errors.Is` 和 `errors.As` 找到 `context.DeadlineExceeded` 或自己的原因。有语法错误的模块或脚本返回带位置的 `*SyntaxError`。调用过程中的 Go panic（比如宿主函数 panic）以
`*InternalError` 返回，之后运行时仍然可以使用。

在宿主函数里调用的 `Call` 或 `CallFunction` 会把其下发生的 panic 的 `*InternalError` 返回给这个宿主函数。宿主函数把它作为错误返回时，
JavaScript 看到的是一个抛出的 `Error`，和其他 Go error 一样可以被捕获。

`Runtime.StackTrace(exc)` 返回被抛出的 `Error` 的 V8 格式 `stack`，同样不执行 JavaScript。

## 事件循环和定时器

### 运行循环

单独的 `Runtime` 没有定时器。`eventloop` 包提供定时器，以及一个在运行期间独占运行时的事件循环：

```go
loop, err := eventloop.New(rt, eventloop.Options{})
err = loop.Run(func(rt *moejs.Runtime) error {
	_, err := rt.RunScript(script) // 脚本里可以调用 setTimeout
	return err
})
```

`New` 安装全局函数 `setTimeout`、`clearTimeout`、`setInterval`、`clearInterval`、`setImmediate` 和 `clearImmediate`。
`Run` 先调用传入的函数，然后在同一个 goroutine 上运行定时器、immediate 和投递的任务。没有东西让循环继续时 `Run` 返回：
没有 ref 状态的定时器或 immediate，没有未完成的 `Hold`，没有未兑现的 `NewPromise`，也没有未运行的任务。

模块的顶层 `await` 在等定时器时，`Load` 返回 `ErrModulePending`。循环之后运行这个定时器，模块随之执行完，
所以传给 `Run` 的函数把这个错误当作成功。

`Start` 则在新的 goroutine 上运行循环。这样的循环空闲时也不退出，直到 `Stop` 或 `Terminate`。

### 阶段

每轮循环有三个阶段，对应 Node 的 timers、poll 和 check 阶段：

1. 阶段开始时已到期的定时器，按到期时间排序，时间相同的按设置顺序。
2. 阶段开始前投递的任务：`RunOnLoop`、`Post`、`Hold` 的 `done` 函数和 `NewPromise` 的 settle 函数。
3. 阶段开始前设置的 immediate。

一个阶段新加入的工作留到之后的轮次。延迟至少 1 毫秒，所以定时器回调里设置的定时器在之后的轮次运行。
回调排入的 promise 任务和 `queueMicrotask` 回调在它返回时运行，先于下一个回调。没有工作时，循环睡眠到下一个定时器到期或有任务到来。

### 与 Node 的兼容

需要定时器的代码是为 Node.js 写的，所以定时器遵循 Node.js：

- `setTimeout` 和 `setInterval` 返回 `Timeout` 对象，带有 `ref`、`unref`、`hasRef`、`refresh`、`close` 和返回数字 id 的
  `[Symbol.toPrimitive]`。`setImmediate` 返回 `Immediate` 对象，带有 `ref`、`unref` 和 `hasRef`。
- 回调收到额外的参数，`this` 是 `Timeout` 或 `Immediate` 对象。回调不是函数时抛出 `code` 为 `ERR_INVALID_ARG_TYPE` 的
  `TypeError`。字符串不会被当作代码执行。
- 延迟经过 `ToNumber` 转换，再截断为整毫秒。NaN、小于 1 和大于 2147483647 的延迟都按 1 毫秒处理。
- interval 在回调开始的时刻加上延迟重新排期。`refresh` 从现在起重新计算定时器的延迟，已经触发过的 timeout 会再运行一次。
- `clearTimeout` 和 `clearInterval` 接受 `Timeout` 对象，或者读取过之后的 id（数字或字符串）。`clearImmediate` 接受
  `Immediate` 对象。三者都忽略其他值，在定时器自己的回调里调用也有效。
- unref 状态的定时器或 immediate 不会让 `Run` 继续，只在有别的东西让循环继续时运行。

有些细节和 Node 不同。`Timeout` 和 `Immediate` 对象没有自有属性：`JSON.stringify` 得到 `{}`，`constructor.name` 是
`"Object"`。它们的方法在别的对象上调用时抛出 `TypeError`，包括这种对象的 `Proxy`；`Timeout` 没有 `Symbol.dispose` 方法。
定时器按精确的到期时间排序，Node 则比较整毫秒。不提供 `timers/promises`、`AbortSignal` 选项和 `util.promisify`。

### 异步宿主函数

循环运行期间只有它的 goroutine 使用运行时。其他 goroutine 用三种方式把工作交给它：

- `RunOnLoop(fn)` 投递 `fn`。循环终止后返回 false。`eventloop.Post(loop, task)` 对返回 error 的任务做同样的事，循环像处理回调的错误一样处理它
  （交给 `OnError`，或者停止循环）；自己兑现 promise 的任务把这些 promise 的 job 抛出的错误返回给循环，不会丢失。
- `Hold()` 让循环一直运行到它的 `done(fn)` 被调用，`done` 会投递 `fn`。
- 宿主函数在循环上调用 `NewPromise()`，得到一个 promise 和一个 `settle` 函数。`settle(f)` 在循环上运行 `f`：
  返回值兑现 promise，Go error 则以宿主函数的同一个错误会抛出的 `Error`（`ThrownValue`）拒绝它。

`done` 和 `settle` 可以在任意 goroutine 上调用。第一次调用生效，之后的调用不做任何事。`Hold` 要在循环上调用，
也就是在宿主函数或回调里。在别的 goroutine 上调用的 `Hold` 要和一个已经没有别的事可做的 `Run` 赛跑，几乎总是输掉，
结果只能让下一次 `Run` 继续运行。

```go
rt.SetGlobal("fetchJSON", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
	url := moejs.Arg(args, 0).String()
	p, settle := loop.NewPromise()
	go func() {
		body, err := download(url) // 阻塞的工作在循环之外运行
		settle(func(rt *moejs.Runtime) (moejs.Value, error) {
			if err != nil {
				return moejs.Undefined(), err
			}
			return rt.ParseJSON(body)
		})
	}()
	return p, nil
}))
```

### 错误

下面这些错误会停止循环，并由 `Run` 返回：回调抛出的异常、回调之后的任务抛出的第一个异常（比如 `queueMicrotask` 回调里的）、
宿主函数里的 Go panic，以及 `settle` 的失败。Node.js 进程遇到未捕获的异常也会结束。设置了 `Options.OnError` 时，
循环把这些错误交给它，然后继续运行。`Run` 自己的函数返回的错误让 `Run` 立即返回。

中断总是停止循环，而且在循环从队列取出下一个定时器、任务或 immediate 之前停止，队列里的东西都保留下来。`Run` 返回
`*InterruptedError`。`Loop.Interrupt(v)` 中断运行时并唤醒循环；单独调用 `Runtime.Interrupt` 时，睡眠中的循环要等到下一个定时器才醒。
中断会保留，所以宿主再次运行循环之前要调用 `ClearInterrupt`。

宿主任务（`RunOnLoop`、`Post`、`done` 和 `settle` 的函数，以及 `OnError`）里的 panic 不会被恢复。它从 `Run` 向外展开；
`Start` 启动的循环则会让程序退出。当前阶段还没运行的任务和 immediate 留在队列里，等下一次 `Run`。发生 panic 的任务本身丢失，
settle 函数 panic 的 promise 永远不会兑现。

没有处理函数接住的拒绝由宿主通过 `SetPromiseRejectionTracker` 处理。

### 停止和复用

`Stop` 在当前回调结束后停止循环，并等到循环结束。它像 `Run` 一样返回结束这次运行的原因：`nil`、停止循环的错误、中断的
`*InterruptedError` 或 `ErrTerminated`，宿主由此知道 `Start` 启动的循环因为什么错误停止。`StopNoWait` 不等待，所以循环上的回调可以调用它。
`Stop` 和 `Terminate` 不能在循环上调用（传给 `Run` 的函数也算在循环上），否则会等待自己。

`Run` 返回或循环停止后，运行时重新归宿主使用。还没运行的东西保持原样，比如 unref 状态的定时器，或者停止、错误和中断留下的一切；
下一次 `Run` 或 `Start` 会接着运行。宿主在两次之间运行的 JavaScript（比如通过 `Call`）也可以设置新的定时器。

`Terminate` 永久结束循环。它用 `ErrTerminated` 中断运行时，从而停止正在运行的 JavaScript，丢弃所有待运行的定时器、immediate
和任务，并等到循环结束。之后 `Run` 返回 `ErrTerminated`，`RunOnLoop` 返回 false，定时器函数会抛出异常。即使当时没有 JavaScript
在运行，中断也会保留，所以宿主再次使用运行时之前要调用 `ClearInterrupt`。再调用一次 `New` 可以给运行时一个新的循环。

## Web API

### 安装

`webapi` 包安装基于 fetch 的 SDK 需要的 Web API：`fetch`、`Headers`、`Request`、`Response`、`ReadableStream`、
`WritableStream`、`TransformStream`、`TextDecoderStream`、`TextEncoderStream`、两种排队策略、`AbortController`、
`AbortSignal`、`EventTarget`、`Event`、`DOMException`、`URL`、`URLSearchParams` 和 `performance`。它们运行在事件循环上，
I/O 只用 Go 标准库。单独的运行时没有这些对象，由宿主显式安装：

```go
rt := moejs.NewRuntime(moejs.Options{})
loop, err := eventloop.New(rt, eventloop.Options{})
api, err := webapi.Install(loop, webapi.Options{
	Client: &http.Client{Timeout: 10 * time.Minute},
	Allow:  webapi.AllowHosts("api.openai.com", "*.anthropic.com"),
})
defer api.Close()
err = loop.Run(func(rt *moejs.Runtime) error {
	_, err := rt.RunScript(script) // SDK 的代码调用 fetch
	return err
})
```

每个全局对象在 JavaScript 第一次读取它时才创建，运行时对从不使用的那些只付出名字的成本。没装这个包的运行时不受任何影响。
这些全局对象是全局对象上可写、可配置的属性，和 Web IDL 的定义一样；运行时共用冻结的内建对象时也是如此，所以宿主可以删除或替换其中任何一个。

每个运行时调用一次 `Install`。`Close` 停止所有进行中的 fetch，关闭所有没读到结尾的响应体，并停止 `AbortSignal.timeout` 的定时器。
它可以在任意 goroutine 上调用，被它拒绝的 promise 在循环下次运行时兑现。`Close` 是最终的，之后的 fetch 都会拒绝：运行时被丢弃时调用它。
`loop.Terminate` 不会停止进行中的 fetch，所以也要调用 `Close`。

把运行时放进池里复用的宿主改用 context 结束一次运行的网络活动：

```go
ctx, cancel := context.WithCancel(requestCtx)
api.SetContext(ctx) // 这次运行的 fetch 的父 context
err = loop.Run(...)
cancel() // 让这次运行中仍在进行的 fetch 失败，并关闭它们的 body
// 运行时回到池里，下次运行时照常 fetch
```

取消这个 context 会让它之后启动、仍在进行中的 fetch 像网络错误一样失败，错误的 `cause` 是 context 的 cause；
它们的响应体立即关闭，它们让循环保持运行的引用也随之释放。`Options.Context` 仍是每个请求的父 context。

### 权限和客户端

运行时只能访问宿主给它的网络：

- `Options.Client` 发送请求。不提供时每个 `fetch` 都以 `TypeError` 拒绝（全部禁止）。客户端的 `Transport` 决定请求怎么走：
  自定义的 `http.RoundTripper` 可以不经网络、直接在 Go 里响应请求，也可以记录请求，或者加上脚本看不到的凭据。
- `Options.Allow` 在每个请求发送前调用，每次重定向也再调用一次。返回错误则 fetch 被拒绝。钩子看到的是实际发送的请求：
  脚本设置了 `Host` 头时，`req.Host` 就是它。`webapi.AllowHosts` 按主机列表生成这样的钩子：`*.example.com` 匹配
  example.com 的子域名，但不匹配 example.com 本身；`*` 匹配所有主机。脚本设置了 `Host` 头时，除了 URL 的主机，
  这个主机也必须被允许。钩子在请求自己的 goroutine 上运行，所以并发的 fetch 会并发调用它。
- `Allow` 为 nil 时不做任何检查：客户端能访问的每个地址都允许，包括内网、回环地址（`127.0.0.1`、`localhost`）
  和云元数据地址 `169.254.169.254`。运行不受信任的脚本时，请给它主机列表。
- `Options.Context` 是每个请求的 context 的父 context。取消它会让进行中的 fetch 失败，例如宿主关闭时。
  `api.SetContext` 为一次运行的 fetch 增加一个父 context，供把运行时放进池里复用的宿主使用（见“安装”）。

客户端的 `Timeout`、`Jar` 和 `CheckRedirect` 都生效。包会复制客户端来实现每个请求的 `redirect` 模式：`"follow"`（最多 20 次重定向）、
`"manual"`（和 Node.js 一样返回 3xx 响应本身）和 `"error"`。

### 事件循环上的 promise

`fetch`、`text()`、`json()`、`arrayBuffer()`、`bytes()` 以及每次读取响应体，返回的 promise 都在循环上、任务阶段的某个任务里兑现。
一个任务排入的 promise job 在下一个任务之前运行，和浏览器一样。等待响应的 fetch 和等待数据块的读取会让 `Run` 继续运行，
没人读的响应体不会。和 Node.js 一样，`AbortSignal.timeout` 本身不让循环继续运行，使用这个 signal 的 fetch 才会。

### 流式读取

响应体是一个 `ReadableStream`。从流的第一次读取起，一个 goroutine 读取 `http.Response` 的 body，最多比 JavaScript 已取走的
多读一个数据块：它读一个数据块，等流取走，在 JavaScript 处理这一块时读下一块，让等待网络和 JavaScript 的工作重叠。读取时若预读的数据块
已经就绪就立即取走，否则等正在进行的那次读取。读得慢的一方因此仍会通过 TCP 流量控制拖住服务器，内存占用以 socket 缓冲区加每个 body
一个数据块为上限。

```js
const response = await fetch(url, { method: "POST", body, signal });
const reader = response.body.getReader();
for (;;) {
  const { done, value } = await reader.read(); // value 是 Uint8Array
  if (done) break;
  onText(decoder.decode(value, { stream: true }));
}
```

每个数据块都是一个 `Uint8Array`，内容是对 body 的一次读取返回的字节，即已经到达的数据，最多一个读缓冲区。至少填满读缓冲区一半的数据块
直接交出，不复制；更小的数据块在 goroutine 上复制到一个刚好够大的缓冲区里，读缓冲区留给下一次读取。读缓冲区从 16 KiB 开始，读取一直填满时增长到 256 KiB；
设置了 `MemoryLimit` 时最多增长到上限的八分之一，且不小于 4 KiB，这样一次只保留一个数据块的脚本离上限还很远。
`for await (const chunk of response.body)`、`pipeThrough` 和 `pipeTo` 也这样读取。JavaScript 没碰过流的响应，
`text()`、`json()` 等方法在 Go 这一侧一次读完整个 body。被它们读取的 body 就是已使用的：它的流已锁定、已被扰动，
再次读取、`clone()` 或 `text()` 都是 `TypeError`。

交给 JavaScript 的每个字节都在分配之前、在循环上计入运行时的 `MemoryLimit`；在 `eventloop` 下，上限限制的是整次运行跨任务保留和分配的内存，
见上文的内存上限一节。流式读取的数据块在交出它的那个任务里计入；在此之前，只要 JavaScript 还能到达这个流，预读的数据块和读缓冲区
虽然只有 Go 持有，也计入每一次对运行保留内存的测量。整体读取的 body 随到随计：一个 goroutine 最多读到已计入部分之后一个窗口
（最大的读缓冲区）或 body 末尾，循环先计入这些字节，再读下一个窗口。Go 持有的 body 最多比已计入的多一个窗口，比一个窗口小的 body
只花一个任务。已读的字节虽然只有 Go 持有，也计入每一次对运行保留内存的测量，并且只计一次。这个包在循环上的计入属于宿主自己：
将要越过上限时立即测量，和 `RebaseAllocation` 一样。计入失败时关闭 body，读取以带有上限消息的
`RangeError` 拒绝，循环随之停止（`errors.Is(err, moejs.ErrMemoryLimit)`），和 JavaScript 自己分配超过上限时一样。无论额度如何，
比上限还大的 body 都无法交出：`Content-Length` 大于上限的 body 在读取任何字节之前就被拒绝；否则到达上限的那个窗口会多读一个字节，
读取在计入这个窗口之前就以 `RangeError` 拒绝，和知道结果放不下的内置函数抛出的一样，Go 持有的 body 只比上限多一点。使用按任务计算额度的 `eventloop.Options.ResetMemoryPerTask` 时，整体读取的 body 就以此为界。
声明的 `Content-Length` 最多预分配 1 MiB，且不超过上限。gzip 和 deflate 编码的 body 按解码后的字节计算，所以会膨胀的 body 也以同样的方式受限。没有设置 `MemoryLimit` 的宿主可以在自己的
`Transport` 里限制 body 大小，例如用一个把每个响应的 `Body` 包进 `io.LimitReader` 的 `RoundTripper`。`tee()` 和
`clone()` 的分支持有一个分支已读、另一个分支还没读的数据块，和浏览器一样；这些数据块在交出时已经计入。字节流
（包括响应体）的第二个分支拿到的是副本，副本同样计入。

这个包替 JavaScript 保存的很多东西是内存上限的测量走不到的 Go 状态：流排队的数据块、source、sink 和 promise，`AbortSignal` 或
`EventTarget` 的监听器和 reason，以 Go 字符串保存的头部值、URL 和查询参数，由字符串或缓冲区构造的 body 的字节。每个对象的状态在测量到达该对象时
报告自己保存的东西（`moejs.LiveReporter`），所以只要 JavaScript 还能到达这个对象就计入，到达不了就不再计入，不必等 Go 的垃圾回收，
程序也无法把内存藏在这些地方躲开上限。没有对象能通向的进行中的工作，即进行中的 fetch 及等待它的 async 函数、整体读取的 body 和
`AbortSignal.timeout` 的计时器，在运行期间通过 `Runtime.AddLiveRoots` 报告，每次 `Install` 注册一次，`Close` 结束注册；没有上限时这个包
不注册任何东西。这些 Go 字符串在被保存时计入，两个共享同一批字符串的对象（一个 `Headers` 和它的副本）都会计入它们。

gzip 和 deflate 编码的 body 会被解码：请求没有自己设置 `Accept-Encoding` 时由 Go 的 transport 解码，否则由这个包解码。
不解码 Brotli。

### 取消和错误

- `reader.cancel()`、`response.body.cancel()` 和从 `for await` 里 break，都会关闭 Go 的 body 并取消请求的 context。
  body 立即关闭，即使正有一次读取阻塞在上面。因此自定义 `RoundTripper` 的响应 body 必须允许 `Close` 与 `Read` 并发，
  并让这次 `Read` 返回，net/http 的 body 和 `io.Pipe` 都是这样。
- 请求的 `AbortSignal`（`AbortController`、`AbortSignal.timeout`、`AbortSignal.any`）也会取消 context。响应到达之前，
  fetch 以 signal 的 reason 拒绝；之后，响应体的流以这个 reason 出错，正在等待的读取也以它拒绝。默认的 reason 是
  `AbortError` 类型的 `DOMException`，`AbortSignal.timeout` 的是 `TimeoutError`。
- 响应之前的网络错误、被拒绝的请求和重定向错误都以 `TypeError: fetch failed` 拒绝。它的 `cause` 是一个 `Error`，
  内容是 Go 错误的文本；常见情况下还带有 Node 的 `code`（`ECONNREFUSED`、`ECONNRESET`、`ETIMEDOUT`、`ENOTFOUND`）。
- 流式读取期间的错误（例如连接被重置）让流以 `TypeError: terminated` 出错，正在等待的和之后的读取都以它拒绝。
- body 读完、出错、取消、中止以及 `Close` 都会立即关闭 body 并结束请求的 context，不等垃圾回收；读取 body 的 goroutine
  随之停止，它预读的数据块被丢弃。作为最后的保障，JavaScript 丢掉的响应（没读或读了一部分），其 body 会在回收器释放它时关闭。这可能晚很多，宿主不应依赖它：读完或取消 body，并调用 `Close`。

### 冻结的内建对象和 polyfill

这个包的全局对象放在全局对象上，而全局对象保持可扩展，所以它们能和共享的冻结内建对象一起安装。它们的原型属于运行时自己，不冻结。

只补充缺失全局对象的 polyfill 可以和冻结的内建对象一起使用；重新赋值或修改内建对象的 polyfill 不行，因为内建对象的全局绑定被锁定了。
例如在严格模式代码里给 `TextEncoder`、`TextDecoder`、`structuredClone` 或 `queueMicrotask` 赋值会抛出
`Cannot assign to read only property`。这样的 polyfill 应当先检查，保留 moejs 自带的实现：

```js
if (typeof globalThis.TextEncoder === "undefined") {
  globalThis.TextEncoder = TextEncoderPolyfill;
}
```

- 可以和冻结的内建对象一起用的 polyfill：只定义缺失对象的那些，例如 `if (!globalThis.X)` 形式的 URL、AbortController、
  Streams、fetch 和 Headers polyfill，event-target 的 shim，以及 `web-streams-polyfill/ponyfill`。装了 `webapi` 包之后，
  它们发现全局对象已经存在，什么也不做。
- 需要 `Options{MutableIntrinsics: true}` 的 polyfill：替换或扩展内建对象的那些，例如无条件安装的 `TextEncoder`、
  `Promise` 或 `Symbol` 的替代品、全局（非 pure）模式的 `core-js`，以及任何给 `Array.prototype`、`Object.prototype`
  或 `String.prototype` 添加方法的代码。这时每个运行时有自己可修改的内建对象，创建成本约为默认的 25 倍。

使用 `MutableIntrinsics` 时，`Install` 在脚本运行之前就取好包要调用的内建对象（`Promise` 及其 `then` 和 `resolve`、
`Uint8Array`、`TextDecoder` 和 `String.prototype.normalize`），所以脚本之后替换或删除它们不会影响这个包；`Install`
之前就缺失的，需要它的功能会抛出 `TypeError`。仍有一个钩子：`Promise.prototype.then` 会查找被调用的 promise 的
species 构造函数，所以脚本定义的 `Promise[Symbol.species]` getter 或 promise 的 `constructor`，会在包自己的 promise
反应里运行（一次 `pipeTo` 大约调用十几次）。共享的冻结内建对象没有这个钩子。

### 限制

- 不提供 `Blob`、`File` 和 `FormData`。这些类型的 body 是 `TypeError`，`blob()` 和 `formData()` 会拒绝。
- 只支持 `http:` 和 `https:` URL，`data:`、`blob:` 和 `file:` URL 会拒绝。
- 没有 CORS，除客户端的 `Jar` 外没有 cookie，没有缓存，也没有禁止的请求头。请求的 `Host` 头决定请求的主机，
  `Allow` 能看到它，`AllowHosts` 会检查它。
  `mode`、`credentials`、`cache`、`referrer` 和 `integrity` 保留在 `Request` 对象上，但不起作用。
- 长于 4 KiB 的请求方法是 `TypeError`，真实的方法远没有这么长。
- 重定向遵循 Go 的方法规则：301、302 和 303 把 `GET`、`HEAD` 以外的方法都改成 `GET`；Fetch 标准在 301 和 302 时只改 `POST`。
  重定向到另一个源（scheme、主机或端口不同）时去掉 `Authorization`、`Cookie` 和 `Proxy-Authorization`，和 undici 一样。
- 流：`type: "bytes"` 的流接受 `ArrayBufferView` 数据块，没有 `byobRequest`；`getReader({ mode: "byob" })` 抛出 `TypeError`。
  数据源的 `pull` 不返回 promise 时立即算作完成，而不是晚一个微任务。
- `URL` 遵循当前的 URL 标准，包括国际化域名，不报告不导致解析失败的校验错误。
- `performance` 有 `now()`、`timeOrigin` 和 `toJSON()`，没有 mark、measure 和性能条目。

## Node.js 模块

### 安装

`node` 包给运行时提供 `node:fs`、`node:fs/promises` 和 `node:buffer`，也可以用裸名 `fs`、`fs/promises` 和 `buffer`
导入，供读写工作区文件的程序使用，比如编码代理在沙箱里写的程序。它运行在事件循环上，I/O 只用 Go 标准库。

```go
imp := node.Importer(hostImporter) // 宿主没有 Importer 时传 nil
rt := moejs.NewRuntime(moejs.Options{Importer: imp, MemoryLimit: 256 << 20})
loop, err := eventloop.New(rt, eventloop.Options{})
api, err := node.InstallLoop(loop, node.Options{
	FS: &node.FSOptions{
		Root:  "/srv/sandboxes/42", // 宿主上的目录
		Mount: "/workspace",        // JavaScript 看到它的位置
		Cwd:   "/workspace/app",    // 相对路径的基准
		Write: false,               // 默认只读
		Allow: func(op, path string) error {
			if filepath.Base(path) == ".env" {
				return fs.ErrNotExist
			}
			return nil
		},
	},
	Buffer: true, // 全局 Buffer
})
defer api.Close()
```

`node.Importer` 包装宿主的 `Importer`：本包的说明符解析到本包的模块，其余说明符交给宿主的 `Resolve`，所以 `import()` 宿主自己的模块
和以前一样。`node.Resolver` 对传给 `moejs.Link` 的 `Resolver` 做同样的事，供静态导入这些模块的程序使用。模块每个进程编译一次，从各自
的 `import.meta` 读取要导出的内容，由 `Install` 为自己的运行时填好：没有调用 `Install` 的运行时，或者选项里没有 `FS` 的运行时，
`import("node:fs")` 会以 `Error` 失败。`node:buffer` 不需要 `FS`。

`InstallLoop` 把本包装到 `eventloop` 包的循环上；`Install` 接受任何 `node.Loop`（见下）。每个运行时调用其中一个一次，在循环运行之前
或者在循环上调用。`Close` 关闭根目录，还在等 worker 的操作以 `EBADF` 失败；之后文件系统函数都以 `EBADF` 失败。

### 宿主自己的循环

自己驱动事件循环、不用 `eventloop` 包的宿主，给 `Install` 一个 `node.Loop`：

```go
type Loop interface {
	Runtime() *moejs.Runtime
	Post(task func(*moejs.Runtime) error) bool
	Hold() (release func())
}
```

- `Post` 从任意 goroutine 排入一个任务。循环在运行时的 goroutine 上、在其他回调之间像运行回调那样运行它，并像处理回调的错误那样处理
  它的错误：中断（比如内存上限的中断）和异常（比如 `fs.readFile` 的回调抛出的异常）都结束程序，就像未捕获的异常结束 Node.js 那样。
  任务在返回之前运行它排入的 promise 任务，所以循环不必为它运行什么。循环结束后 `Post` 返回 false，任务被丢弃。
- `Hold` 在操作进行期间让循环不要结束：没有别的事可做的循环，只要还有未释放的 hold 就继续等待。它在运行时的 goroutine 上调用；
  `release` 可以在任意 goroutine 上调用，只调用一次，并且在该操作的 `Post` 之后。

每个 promise 和回调形式的操作在开始时取一个 hold，工作完成后投递一个任务，由它兑现 promise 或调用回调。同步函数两者都不需要。
靠 channel 唤醒的循环几行就能实现：`Post` 在互斥锁下追加到队列并通知 channel，`Hold` 计数，循环等待工作时把大于零的计数当作还有
工作要来。本包的测试就在这样的循环上运行。

### 沙箱

每个路径都在 `FSOptions.Root` 之内解析：

- 相对路径接在 `Cwd` 后面。绝对路径必须在 `Mount` 之下（默认是根目录自己在宿主上的路径），不在其下的路径以 `EACCES` 失败；
  `Mount` 与宿主路径不同时，根目录的宿主路径也是这样。
- 路径的其余部分交给操作系统解析，并通过 `os.Root` 限制在根目录内（Linux 的 amd64 和 arm64 上用 `openat2` 加
  `RESOLVE_BENEATH`，一次调用解析整条路径）。`..` 和符号链接按系统的方式解析，即在它前面的链接之后解析；离开根目录的路径以
  `EACCES` 失败。根目录内的链接照常可用，目标是根目录下绝对路径的链接也可以。`realpath`、`glob` 和 `readlink` 返回 `Mount` 下的
  路径。
- 没有 `Write` 时，所有修改文件系统的函数都以 `EROFS` 失败，带 `W_OK` 的 `access` 和带写入 `flag` 的 `readFile` 也一样。
- `Allow(op, path)` 在每次访问之前看到它：`op` 是 `"read"` 或 `"write"`，同 Node.js 的权限模型；`path` 是 `Mount` 下的绝对路径。
  `errors.Is` 能匹配 `fs.ErrNotExist` 的错误让访问以 `ENOENT` 失败，`syscall.Errno` 以该 errno 失败，其他错误以 `EACCES` 失败。它被问到
  什么、能保证什么，见下一节。
- 本包不创建链接：`cp` 复制链接指向的内容。
- 限制是按路径的。宿主在根目录内为根目录外的文件建的硬链接是根目录内的一个路径，程序可以通过它读到那个文件。程序不能创建链接。
- 在 Windows 上，`\` 和 `/` 都是路径分隔符，和 Node.js 一样。
- 在 Linux 上，对本包最近一毫秒内解析过的目录中的名字做 stat、打开或列目录，只需一次系统调用：本包保留该目录的描述符（在根目录之下
  打开），只用它访问这一个名字，且不跟随链接（名字本身是链接时，从根目录出发在根目录之下跟随）。本包自己的 `rename`、`rm`、`rmdir`
  和 `unlink` 在返回之前丢弃这些描述符，所以程序自己的改动（它们出不了根目录）立刻可见。缓存带来的是一个窗口：目录被解析后的至多一
  毫秒内，描述符跟着目录本身走，而不是跟着路径走。如果别的进程在这期间改名或替换了这个目录，程序仍然会在它的新位置访问到它，哪怕
  新位置在根目录之外：别的进程把目录移出根目录、往里面写东西、再移回来，程序就可能读到它写的内容。不用缓存时，每次都重新解析路径，
  唯一的窗口是路径解析和使用之间的那一段，os.Root 路径也有这个窗口。别的进程可能在程序运行时重排目录树时，设置
  `FSOptions.NoDescriptorCache`；这样根目录以下的 `stat` 需要三次系统调用而不是一次。

### Allow 钩子

钩子先按程序写出的路径逐级被问到每个目录，然后被问到路径解析经过的每个名字（包括链接指向的名字），每次都在查看它之前：目录按
`"read"` 问，路径本身按操作的 `op` 问。被拒的目录让整个操作以钩子的错误失败，所以程序得不到它里面的任何信息：哪些名字存在、它们是
什么类型、其中的链接指向哪里，都得不到。只有链接本身和它的目标都被放行时才会跟随链接。`realpath` 也这样问。

钩子还管着整体改动一个目录的操作所涉及的下层内容。带 `recursive` 的 `rm` 对它要删除的每个条目都问 `"write"`，有一个被拒就什么都
不删；对目录做 `rename` 时，会问其下的每个条目，原路径和新路径都问；带 `recursive` 的 `mkdir` 问它要创建的每个目录。

钩子不能保证的：

- 它管的是路径，不是文件。文件的路径随改名而变，按路径写的策略对程序的改名只能管到它写出的那些路径：改名一个祖先目录，会把前缀策略
  拒绝的内容移到它可能放行的路径上，除非策略也拒绝这次改名（改名移动的每个条目都会问钩子）。如果策略必须在程序怎么改名都成立，
  就给它 `Write: false`，或者把 `Root` 收窄到它可以写的区域。
- 它管的是名字，不是列表：对放行的目录做 `readdir` 和 `glob` 会列出其中所有名字，包括被拒的。
- 路径在访问之前解析。程序如果从另一个进行中的操作在两者之间把链接改名到位，可以和钩子竞争；根目录的限制不受影响。
- 在忽略大小写的文件系统上（macOS 和 Windows 默认如此），钩子收到的是程序的拼写，而系统不论大小写都能解析：在这些系统上比较名字
  要忽略大小写。
- 钩子调用得很频繁，路径的每一级目录都调用一次，递归的 `rm` 和 `rename` 更多：要让它快。

### 错误

错误和 Node.js 的一样：一个 `Error`，自身属性依次为 `errno`、`code`、`syscall`、`path`，`rename`、`copyFile` 和 `cp` 还有
`dest`，消息也是 Node 的格式，比如 `ENOENT: no such file or directory, open 'a.txt'`。`path` 是程序传入的原样路径。syscall 名和
消息逐个用例对齐 Node.js 24（对目录 `readFile` 得到 `read` 的 `EISDIR`，没有路径；`realpathSync` 报出失败的那一级路径）：本包的测试
在 Node.js 里跑同样的用例并比较。参数错误抛出 Node 的 `TypeError` 和 `RangeError`，带 `ERR_INVALID_ARG_TYPE` 之类的 `code`。
Node 的 `node:fs` 里本包没有实现的函数抛出 `ERR_METHOD_NOT_IMPLEMENTED`。

### 有哪些

- 读：`readFile`、`readdir`（`withFileTypes`、`recursive`、`encoding`）、`stat` 和 `lstat`（`Stats` 带全部字段、惰性的日期和
  `is*` 方法；`throwIfNoEntry`）、`access` 和 `fs.constants`、`exists` 和 `existsSync`、`realpath`、`readlink`，以及 `glob` 和
  `globSync`（`cwd`、模式或函数形式的 `exclude`、`withFileTypes`、`followSymlinks`）。每个函数都有同步、回调和 promise 三种形式；
  `fs.promises.glob` 返回异步迭代器。
- 写（需要 `Write`）：`writeFile`、`appendFile`、`mkdir`（`recursive`）、`rm`（`recursive`、`force`）、`rmdir`、`unlink`、`rename`、
  `copyFile` 和 `cp`（`recursive`、`force`、`errorOnExist`）。
- `Buffer`，`Uint8Array` 的子类：`from`（`utf8`、`hex`、`base64`、`base64url`、`latin1`/`binary`、`ascii` 或 `utf16le`/`ucs2`
  编码的字符串，数组，类数组，与之共享内存的 `ArrayBuffer`，Buffer 的 JSON）、`alloc`、`allocUnsafe`、`concat`、`isBuffer`、
  `isEncoding`、`byteLength`、`compare`，以及方法 `toString`、`write`、`slice` 和 `subarray`（都共享内存）、`equals`、`compare`、
  `indexOf`、`lastIndexOf`、`includes`、`toJSON`、`fill`、`copy`、`swap16/32/64`，两种字节序的 8、16、32 位整数和 float、
  double 的读写，以及 1 到 6 字节整数的读写。解码按 WHATWG 的 UTF-8 解码器进行，和 Node.js 一样。

### 顺序

`readdir` 返回按字节排序的名字，和 Node.js 在 Unix 上一样（libuv 会排序）。递归列表沿用 Node 的顺序：同步和回调形式是广度优先，
promise 形式是 Node 的栈顺序；和 Node.js 一样，会跟随指向目录的链接。`glob` 返回排好序的匹配；Node.js 按自己的遍历顺序返回，这个顺序没有文档说明。`fs.promises.glob` 和 Node 的 async generator 一样
返回异步迭代器，错误的模式或选项不会直接抛出，而是让它的 `next()` reject。模式与 Node 一样是
minimatch 的：`*`、`?`、字符类、`**`、花括号和 extglob；不匹配点文件，除非模式写出了点；`**` 不跟随链接，除非设置
`followSymlinks`；和 Node.js 一样，反斜杠是路径分隔符而不是转义字符。没有 extglob 的段匹配一个名字的时间与名字长度成线性；有 extglob
的段编译成 Go 正则表达式（就像 minimatch 编译成 RegExp），匹配时间线性、内存有界。RE2 正则无法表达的 `!(...)` 通过尝试名字的各种切分
来匹配，工作量有上限并检查运行时的中断：一个段至多含一个 `!(...)`，且不能在其他 extglob 里面；耗时过长的名字会让遍历失败。模式至多
64 KiB（同 minimatch），一次调用的模式和 `exclude` 模式中所有不同的字符串合计也至多 64 KiB，数组重复同一个字符串多少次都只复制
和编译一次，至多 65536 个。一个模式的花括号展开为它的前 4096 个模式，一次调用的所有模式展开后合计至多 1 MiB：展开的耗时与产出成正比，
一超过 1 MiB 就停下，花括号至多嵌套 1024 层，展开过程中检查运行时的中断。段的解析时间与其长度成线性，连续的普通字符算一个记号，
extglob 至多嵌套 256 层，一次调用至多 65536 个记号。每段以及整次调用编译出的表达式大小都有上限。展开和表达式占用的内存计入内存上限，
表达式之间检查运行时的中断。匹配的工作量（名字的字节数乘以在其上运行的表达式大小）会被计数：随着增长检查运行时的中断和 `Close`，超过上限的
调用以 `TypeError` 失败。超过这些上限的模式或调用是 `TypeError`。

### 事件循环和限制

同步函数在循环上运行，和 Node.js 一样。promise 和回调形式把文件系统工作放到 `FSOptions.Workers` 个 goroutine 组成的池里做
（默认 4 个，与 libuv 的线程池一样大）；超出的操作在队列里等，不各占一个 goroutine。它们在循环的 tasks 阶段的任务里完成，进行中的
操作让 `Run` 保持运行。回调里抛出的异常会像定时器回调的异常一样停止循环。

本包交给 JavaScript 的内容（文件内容、名字和路径）在创建之前就在循环上计入内存上限（`moejs.Options.MemoryLimit`）。大于上限的文件
在读取之前就失败，抛出 `code` 为 `ERR_FS_FILE_TOO_LARGE` 的 `RangeError`，就像 Node 拒绝大于 2 GiB 的文件那样；保留的读取内容
超过上限的程序会带着上限的错误停下。worker 为 JavaScript 读取和列出的内容，同一时刻最多占用一个上限：worker 在读文件之前先从这份预算
里取出文件大小。列表（递归的或 glob 的）在增长时计入上限（或从预算中取），而不是完成后才计入。工作已完成的结果在循环结算它之前
继续占着它的那一份。增长放不下的操作会等待（这是背压），等的是不需要它也会释放的那些：等待循环结算的结果，以及比它先开始的操作。
所以没有操作会等自己，也没有两个操作互相等待；最早开始的操作宁可超出预算，也不等更晚的，而且只有它会超出：更早的操作和待结算的结果
把占用的都释放之后才轮到它，所以 worker 占用的最多是上限再加这一个操作的增长。等待会在运行时中断和 `Close` 时结束。单个
操作本身就需要超过上限（比如一个比上限还大的列表）时，以程序可以捕获的 `RangeError` 失败，同步形式和其他形式都一样，而不是让程序停下。
预算在每条路径上都会释放：循环结算结果时、工作失败时、中断让工作停下时（结果被丢弃，它的 promise 保持 pending，就像中断对其他
promise 那样）、循环被 Terminate 而拒绝结算时，以及 `Close` 时。被中断的程序里，中断到来时还没在 worker 上开始的操作也会被丢弃，而不是为一个
已被宿主停下的程序继续执行，所以宿主清除中断后再次运行这个运行时，不必等它们。

长时间的遍历（递归 `readdir`、`glob`、`cp`、递归 `rm` 以及钩子的遍历）和 `Buffer` 的长循环，会在运行时的中断（`Interrupt`、
`Terminate`、内存上限）和 `Close` 时停下，同步形式和其他形式都一样。

### 与 Node.js 的差异

- 没有流（`createReadStream` 之类）、文件句柄（`fs.promises.open`、`fs.open`）、`watch` 和 `bigint` 形式的 stats。TODO.md
  列出了它们。
- Linux 上的 `birthtimeMs` 等于 `ctimeMs`，和 libuv 不用 `statx` 时一样。
- `cp` 没有 `filter`，复制链接指向的内容而不是链接本身。
- 每个 Buffer 有自己的 `ArrayBuffer`：没有内存池，所以除切片外 `byteOffset` 都是 0，`allocUnsafe` 返回清零的内存。
  `buffer.kMaxLength` 是 moejs 最大的 `ArrayBuffer`，即 2 GiB - 1。
- `fs.constants` 是 Linux 的值。
- base64 的速度约为每秒 1 GB；Node.js 用 SIMD，大输入上约快四倍。

## 隔离

### 共享冻结的内建对象

默认情况下，所有运行时共用一套深度冻结的内建对象，所以每个插件看到的内建对象都一样。这是 Hardened JavaScript（SES `lockdown()`）的模型。
写入 `Array.prototype` 在严格模式代码里抛出 `TypeError`，在非严格模式代码里和写入其他冻结对象一样静默失败。

内建对象的全局绑定也被锁定。`Promise = MyPromise` 在非严格模式下被忽略，在严格模式下抛出错误，`delete globalThis.Array` 返回 false。
插件想用自己的 `Promise` 时，在自己的模块里声明同名绑定。

写入共享内建对象的操作在任何 setter 运行之前就失败，旧式的 `RegExp.input = v` 和 `RegExp.$_ = v` 也一样。`RegExp.$1` 等旧式静态属性读取的是
本运行时自己最近一次的匹配。

`Options{MutableIntrinsics: true}` 给每个运行时单独建一份可修改的内建对象，适合插件会修改内建对象的宿主。这样创建运行时的成本约为默认的 25 倍。

### 按运行时设置时区

`Options.TimeZone` 设置 `Date` 使用的本地时区，它也是 `Intl.DateTimeFormat` 的默认时区；nil 表示 `time.Local`。
`Options.Locale` 设置 `Intl` 的默认区域（见下文“默认区域”）。

## Intl

### 已有的部分

`Intl.getCanonicalLocales`、`Intl.supportedValuesOf`、带区域信息方法（`getWeekInfo` 等）的 `Intl.Locale`、`Intl.Segmenter`、
`Intl.NumberFormat`、`Intl.DateTimeFormat` 和 `Intl.PluralRules`，以及用它们格式化的数字、BigInt、日期和数组的 `toLocaleString`
方法。`Intl.Collator`、`Intl.RelativeTimeFormat`、`Intl.ListFormat`、`Intl.DisplayNames` 和 `Intl.DurationFormat`
暂时还没有，[TODO.md](../TODO.md) 列出了它们。`String.prototype.localeCompare` 不论区域都使用 CLDR root 排序规则。

### 区域

默认区域在任何宿主上都是 `en-US`，不受进程环境影响，除非宿主用 `Options.Locale` 设置了别的区域，所以同一个程序在哪里运行结果都一样。语言标签按
ECMA-402 的要求规范化，使用 CLDR 48 的别名数据；`Intl.Locale` 的 `maximize()` 和 `minimize()` 使用 CLDR 48 的 likely subtags。
可用区域是 CLDR 的区域，每个“语言-文字-地区”区域另有对应的“语言-地区”形式（有 `zh-Hant-TW` 也就有 `zh-TW`）。`"best fit"`
区域匹配与 `"lookup"` 相同。

`Intl.NumberFormat` 和 `Intl.DateTimeFormat` 带有 11 个区域的 CLDR 48 数据：`en`、`zh-Hans`、`zh-Hant`、`ja`、`ko`、`de`、`fr`、
`es`、`pt`、`ru` 和 `it`，以及这些语言的地区区域，每个地区区域与 ICU 一样按 CLDR 的父区域继承：`en-GB` 和 `en-AU` 继承 `en-001`，
`es-MX` 继承 `es-419`，`pt-AO` 继承 `pt-PT`。它们的可用区域是这些语言的 CLDR 区域；`resolvedOptions().locale` 与 Node.js
一样是匹配到的区域，并按它自己的数据格式化：`en-GB` 把 2026 年 10 月 11 日写作 `11/10/2026`，`en-IN` 分组为 `12,34,567`，
`de-CH` 写作 `1'234.5`。与 ICU 一样，`zh-HK` 即 `zh-Hant-HK`，`zh-SG` 即 `zh-Hans-SG`。其他语言的区域回退到默认区域：
`sv-SE` 按 `en-US` 格式化。`Intl.PluralRules` 有每种 CLDR 语言的规则。

数字的字形跟随数字系统（`-u-nu-arab`、`numberingSystem` 选项），前提是它有简单的数字映射，即
`Intl.supportedValuesOf("numberingSystem")` 列出的那些；其他数字系统按 ECMA-402 的允许被忽略。`Intl.DateTimeFormat`
只有公历：其他历法（`-u-ca-japanese`、`calendar` 选项）会被忽略，`resolvedOptions().calendar` 为 `"gregory"`。

与 Node.js 不同的地方，moejs 遵循 ECMA-402 和 UTS #35：

- 值为 `true` 的关键字会去掉这个值：`new Intl.Locale("en-u-ca").calendar` 和 `en-u-co-true` 的 `collation` 都是 `""`，
  Node.js 给出 `"true"`。
- 选项值只按它的关键字的别名规范化：`{calendar: "yes"}` 仍是 `"yes"`（`en-u-ca-yes`），Node.js 把它变成 `true`。
- `maximize()` 会替换未知的文字和地区代码：`zh-Zzzz-ZZ` 变成 `zh-Hans-CN`，Node.js 保持不变。
- `Intl.Locale` 有 `variants` 选项和 getter，Node.js 24 没有。
- `resolvedOptions().timeZone` 是传入的标识符，按数据库里的大小写：`Asia/Kolkata` 保持原样，Node.js 报告为 `Asia/Calcutta`。
  `Intl.supportedValuesOf("timeZone")` 列出主标识符，包括 `UTC` 和 `Etc/GMT±n`。
- 日语的 `hour12: true` 在所有地区都是 0-11 小时制（`h11`），Node.js 只在 `ja-JP` 使用它。
- `Intl.PluralRules` 接受 BigInt（Node.js 拒绝），两个格式化结果相同的值的 `selectRange` 返回起点的类别。
- `getCalendars()` 是 `["gregory"]`（即 `Intl.DateTimeFormat` 支持的历法）；没有数字数据的区域 `getNumberingSystems()` 是
  `["latn"]`，Node.js 对 `ar-EG` 给出 `arab`。

与 Node.js 一样，`format` 在 `formatToParts` 使用 U+202F（英语里 `AM`、`PM` 前的窄不换行空格）的地方写普通空格。

### 默认区域

`Options.Locale` 设置运行时的默认区域，即脚本没有指定区域时 `Intl` 构造函数和 `toLocaleString` 方法使用的区域。它是 `"zh-CN"`
这样的 BCP 47 语言标签，在创建运行时时规范化，并去掉 Unicode 扩展（`-u-` 关键字；ECMA-402 的默认区域不带它们）。空字符串或格式不正确的标签表示
`"en-US"`。之后可以用 `Realm.SetLocale` 修改。

```go
rt := moejs.NewRuntime(moejs.Options{Locale: "zh-CN", TimeZone: shanghai})
// (1234.5).toLocaleString() 是 "1,234.5"；new Date(0).toLocaleString() 是
// "1970/1/1 08:00:00"。
```

### 时区

`Intl.DateTimeFormat` 和 `Date` 的 `toLocaleString` 方法的默认时区是运行时的 `Options.TimeZone`。它为 `nil` 时使用
`time.Local`，时区名取自 `TZ`、`/etc/localtime` 或 `/etc/timezone`，都无法确定时为 `UTC`。

`timeZone` 选项接受任意大小写的 IANA 名称（`asia/tokyo`）、偏移量（`+05:30`）和 `UTC`。moejs 用 Go 的 `time.LoadLocation`
加载有名称的时区，不内嵌时区数据库：Go 会找到宿主的数据库（`/usr/share/zoneinfo`、`$ZONEINFO`）。在没有时区数据库的最小镜像（scratch、
distroless static）上，宿主可以用 `import _ "time/tzdata"` 把 Go 自带的数据库编进程序（约 450 KB）。没有数据库时，`UTC`、
`Etc/GMT±n` 和偏移量照常可用，有名称的时区会抛出 `RangeError`，说明宿主没有时区数据库以及如何补上。
`Intl.supportedValuesOf("timeZone")` 只列出宿主上能加载的时区。它和 `Intl.DateTimeFormat` 都会在 10 秒后重新尝试加载失败的时区，
所以程序运行期间安装的数据库会被用上。

### Segmenter

`Intl.Segmenter` 按 Unicode 17 的 Unicode 标准附录 #29（UAX #29）把文本分成字素簇、词和句子，不做区域定制。字素簇就是用户看到的一个字符：
emoji ZWJ 序列、旗帜、带组合符号的字母、印度系文字的连字。

分词不使用词典。词与词之间不写空格的文字（中文、日文、泰文、老挝文、高棉文、缅甸文）按附录的默认规则切分：每个表意文字自成一个词，一串泰文字母按字符切开。
浏览器和 Node.js 会把这类文本切成词典里的词。由字母、数字、假名或表意文字组成的片段 `isWordLike` 为 true，与 Node.js 相同。

### 开销

创建运行时不会创建 `Intl`。进程里第一个用到它的运行时会构建它，它的数据（CLDR 别名、likely subtags 和区域列表、Unicode
断行属性、各区域的数字和日期格式、时区名称）在第一次使用时解码，每个进程只解码一次；某个区域的数据在第一次用到该区域时解码。不带参数的
`toLocaleString()` 在每个运行时里复用同一个格式化器，所以只需要分配结果。数字、日期和时区数据在程序二进制里约占 460 KB
（其中地区区域占 85 KB）；加上 `Intl.NumberFormat`、`Intl.DateTimeFormat` 和 `Intl.PluralRules` 的代码，链接 moejs 的程序约增大 1 MB。

## 实现

### 16 字节的值

`Value` 占 16 字节，由一个 `unsafe.Pointer` 和一个 `uint64` 组成。数字、布尔、`undefined` 和 `null` 不需要分配内存。指针字里始终是有效指针或 nil，
Go 的垃圾回收器可以安全地扫描它。

### 形状和内联缓存

对象共用一棵不可修改的形状转移树，按属性插入顺序组织。属性访问指令带有内联缓存槽，原型链是否仍然有效由一个计数器判断。由 Go map
转换来的对象按键集合共用形状，所以插件代码读取 `ctx.xxx` 时跨调用保持单态。

### 寄存器字节码虚拟机

虚拟机执行定宽 32 位寄存器指令，每个运行时有一段连续的寄存器栈。函数调用不分配内存。异常通过处理表展开。

### 字符串

字符串有四种形式：ASCII（直接使用 Go string，零拷贝）、UTF-16、rope 和懒字符串。rope 在需要时扁平化；懒字符串保留原来的 UTF-8 或 JSON 文本，第一次读取内容时解码一次。内建属性名是静态 atom。

### 锁

属性访问、函数调用、字符串操作、宿主值转换和正则匹配都无锁运行。不同 goroutine 上的运行时只共用垃圾回收器。

### 正则表达式

moejs 能精确翻译的正则表达式交给 Go 的 `regexp`（RE2，线性时间）执行，其他的由纯 Go 回溯引擎执行。回溯引擎的栈有上限，每 4,096 步检查一次中断。

### 有界缓存

即使每个请求都带来以数据为键的对象（ID、用户输入、宿主 map 的键），池中运行时的缓存也保持在固定上限之内。每个 intern 缓存最多保存 4,096 个名称。
宿主形状缓存最多保存 1,024 个键集合，合计 8,192 个键。缓存满了以后，运行时清空它，再按实际使用重新填充。一个形状强引用前 8 个转移，
其余的转移用弱引用，所以以数据为键的对象被回收时，它们的形状也一起被回收。
