"""The on-screen text of the race video, one table per language, and each
language's reading-time rule (video.py --check enforces it).

zh: 2.5 s plus 0.25 s per word, a word being a Chinese character or a run
of ASCII letters, digits, '.', '%' or '×'; spaces and punctuation are free.
en: 2.5 s plus 0.06 s per character, spaces and punctuation included.

Names stay as they are in both: engines, scenario ids, flags and versions."""

ZH_FREE = "：，、·；（）()/:,-—…？?!！"


def zh_words(s):
    n, run = 0, False
    for c in s:
        if c.isascii() and (c.isalnum() or c in ".%") or c == "×":
            n += not run
            run = True
        else:
            run = False
            n += not (c.isspace() or c in ZH_FREE)
    return n


def zh_reading(s):
    return 2.5 + 0.25 * zh_words(s)


def en_reading(s):
    return 2.5 + 0.06 * len(s)


ZH = dict(
    reading=zh_reading,
    TX=dict(
        hook1="用 Go 写的 JS 引擎", hook2=[("比 ", "yellow"), ("C", "grey"), (" 写的还快？", "yellow")],
        cmd="$ git ls-files '*.s' '*.c' | wc -l", asm="汇编", asm_note="moejs 自己的代码里",
        cgo_note="引擎和依赖库都不用 C", os_title="一份代码，分别编译到这些系统", os_note="Go 全平台兼容",
        wasm="Wasm",
        gen="通用测试", gen_sub="8 个经典 JS 程序接力",
        gw="AI 网关", gw_sub="每个请求都要经过 JS 插件", gw_in="请求", gw_out="AI 模型", chip="JS 插件",
        plug="插件场景", plug_sub="5 段接力，看谁先跑完",
        plug_head="插件接力", close="险胜 C QuickJS", first="第 {n}",
        col_time="用时",
        subs=["10 万个小请求", "1600 个长对话请求", "160 个请求，每个带 1 张 8 MB 的图",
              "160 个请求，每个带 8 张 1 MB 的图", "一共完成 {n} 个长对话请求"],
        tiles_title="插件场景：大幅领先", tiles_sub="每段 moejs 的速度是 C QuickJS 的几倍",
        old_fast="比上个版本快 {x:.1f}×",
        score="通用测试：也不落后", score_sub="8 个程序的总得分，越高越快", score_vs="moejs 是 C QuickJS 的 {x:.2f} 倍",
        leg5="第 5 段：agent100k，8 个进程各占 1 个 vCPU，到一共完成 {n} 个请求为止",
        freeze="定格", method="测试方法", sec="{x:.2f} 秒",
        method_lines=[
            "通用测试：V8 benchmark suite v7，8 个程序各占 1/8 赛道",
            "通用测试画面按 0.8 倍速播放，时钟是实测秒数",
            "总得分：套件自带的 run.js，单独测量；上个版本：v0.1.0-alpha.11",
            "moejs：当前开发版，-tags purego，CGO_ENABLED=0，Go 1.27.2",
            "Go 1.27.2 的 {targets} 个系统/CPU 组合全部能编译（CGO_ENABLED=0）",
            "插件：new-api 任务插件，结果 SHA-256 与 C QuickJS 一致",
            "插件接力：5 段各占 1/5 赛道，每段用时是这一段单独实测的用时",
            "请求数：small 10 万，agent100k 1600，big8m 160，multi8x1m 160",
            "第 1-4 段：1 个 vCPU，一次一个请求",
            None,  # the fifth leg (leg5), from the recorded 8-core counts
            "第 5 段的倍数：8 个进程各计时 10 秒，比完成的请求数",
            None,  # the lanes line, from the interference checks
            "每场 3 轮取中位数；名次只比画面上的引擎"],
        end_tag="在 Go 程序里运行 JS 插件", end_head="插件场景：大幅领先", end_sub="通用测试：也不落后",
    ),
    # the plugin legs and the V8 programs by plain words (the V8 words follow
    # each benchmark file's header)
    plugin_names=["普通请求", "AI Agent 长对话", "大图请求", "多图请求", "8 核一起跑"],
    v8_names={"Richards": "任务调度", "DeltaBlue": "约束求解", "Crypto": "加密解密", "RayTrace": "光线追踪",
              "EarleyBoyer": "解析与推理", "RegExp": "正则匹配", "Splay": "树操作", "NavierStokes": "流体模拟"},
    # systems the engine builds for, the big ones first: (name, GOOS)
    os_big=[("Windows", "windows"), ("macOS", "darwin"), ("Linux", "linux"), ("Android", "android"),
            ("iOS", "ios"), ("浏览器", "js")],
    os_small=[("FreeBSD", "freebsd"), ("OpenBSD", "openbsd"), ("NetBSD", "netbsd"), ("Solaris", "solaris"),
              ("AIX", "aix"), ("Plan 9", "plan9")],
    old="moejs 上个版本",
    scene_words=dict(fixed="通用测试", score="通用测试", small="第 1 段", agent100k="第 2 段", big8m="第 3 段",
                     multi8x1m="第 4 段"),
    lanes_shared="{scenes}：多个测试同时跑，各占 1 个核", lanes_join="和", lanes_alone="每次只跑一个测试",
)

EN = dict(
    reading=en_reading,
    TX=dict(
        hook1="A JS engine written in Go", hook2=[("Faster than one in ", "yellow"), ("C", "grey", 1), ("?", "yellow", 2)],
        cmd="$ git ls-files '*.s' '*.c' | wc -l", asm="assembly", asm_note="in moejs itself",
        cgo_note="no C in engine or libraries", os_title="One codebase, built for each of these",
        os_note="Builds for every platform Go supports",
        wasm="Wasm",
        gen="General JS", gen_sub="A relay of 8 classic JS programs",
        gw="AI gateway", gw_sub="Each request passes a JS plugin", gw_in="requests", gw_out="AI model",
        chip="JS plugin",
        plug="Plugins", plug_sub="A 5-leg relay: who finishes first?",
        plug_head="Plugin relay", close="Edges C QuickJS", first="{nth}",
        col_time="Time",
        subs=["100,000 small requests", "1,600 long-conversation requests", "160 requests, each with one 8 MB image",
              "160 requests, each with eight 1 MB images", "{n:,} long-conversation requests in total"],
        tiles_title="Plugins: far ahead", tiles_sub="moejs speed vs C QuickJS in each leg",
        old_fast="{x:.1f}× faster than last release",
        score="General JS: not behind", score_sub="Total score of the 8 programs, higher is faster",
        score_vs="moejs scores {x:.2f}× C QuickJS",
        leg5="Leg 5: agent100k, 8 processes on 1 vCPU each, up to {n:,} requests in total",
        freeze="Frozen", method="How it was measured", sec="{x:.2f} s",
        method_lines=[
            "General JS: V8 benchmark suite v7, 8 programs, 1/8 of the track each",
            "The general relay plays at 0.8× speed; the clock shows measured seconds",
            "Score: the suite's own run.js, measured apart; last release: v0.1.0-alpha.11",
            "moejs: latest source, -tags purego, CGO_ENABLED=0, Go 1.27.2",
            "All {targets} GOOS/GOARCH targets of Go 1.27.2 compile with CGO_ENABLED=0",
            "Plugins: new-api's task plugins; result SHA-256 matches C QuickJS",
            "Plugin relay: 5 legs of 1/5 track each, each leg timed in its own run",
            "Requests: small 100,000, agent100k 1,600, big8m 160, multi8x1m 160",
            "Legs 1-4: 1 vCPU, one request at a time",
            None,
            "Leg 5 ratio: requests the 8 processes finished, each timed for 10 s",
            None,
            "Median of 3 rounds per race; places count only the engines shown"],
        end_tag="Run JS plugins inside Go programs", end_head="Plugins: far ahead", end_sub="General JS: not behind",
    ),
    plugin_names=["Small requests", "AI agent chat", "Big image", "Many images", "8 cores at once"],
    v8_names={"Richards": "Task scheduler", "DeltaBlue": "Constraint solver", "Crypto": "Encryption",
              "RayTrace": "Ray tracing", "EarleyBoyer": "Parsing & proof", "RegExp": "Regex matching",
              "Splay": "Tree operations", "NavierStokes": "Fluid simulation"},
    os_big=[("Windows", "windows"), ("macOS", "darwin"), ("Linux", "linux"), ("Android", "android"),
            ("iOS", "ios"), ("Browser", "js")],
    os_small=[("FreeBSD", "freebsd"), ("OpenBSD", "openbsd"), ("NetBSD", "netbsd"), ("Solaris", "solaris"),
              ("AIX", "aix"), ("Plan 9", "plan9")],
    old="previous moejs",
    scene_words=dict(fixed="general JS", score="general JS", small="leg 1", agent100k="leg 2", big8m="leg 3",
                     multi8x1m="leg 4"),
    lanes_shared="{scenes}: several tests ran at once, 1 core each", lanes_join=" and ",
    lanes_alone="One test at a time",
)

LANGS = dict(zh=ZH, en=EN)


def ordinal(n):
    return f"{n}{'th' if 10 <= n % 100 < 20 else {1: 'st', 2: 'nd', 3: 'rd'}.get(n % 10, 'th')}"
