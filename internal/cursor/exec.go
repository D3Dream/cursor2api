// Legacy local-tool result builders. Production Run handling deliberately does
// not call these functions: built-in Cursor exec requests are forwarded to the
// downstream agent, which owns the workspace and shell. They remain here for
// protocol/result unit tests and for future explicitly isolated workers.
package cursor

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// 资源上限：模型的一次决策就可能触发本地执行，必须防住"读 2GB 日志/
// 跑 yes 喷输出/扫巨型仓库"这类内存炸弹。
const (
	// maxReadFileBytes execRead 单次读取上限（超出截断并标记）。
	maxReadFileBytes = 4 << 20
	// maxToolOutputBytes shell/grep 单次输出上限（超出截断并标注）。
	maxToolOutputBytes = 1 << 20
	// maxLsEntries ls 目录树总条目上限。
	maxLsEntries = 2000
	// grepTimeoutMs 单次 rg 执行超时。
	grepTimeout = 30 * time.Second
)

// cappedWriter 限量收集输出：写满后丢弃剩余部分并标记截断。
// Write 永远返回 len(p)，子进程不会因"写失败"提前出错。
type cappedWriter struct {
	b         strings.Builder
	limit     int
	truncated bool
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if rem := w.limit - w.b.Len(); rem > 0 {
		if len(p) > rem {
			w.b.Write(p[:rem])
			w.truncated = true
		} else {
			w.b.Write(p)
		}
	} else {
		w.truncated = true
	}
	return len(p), nil
}

func (w *cappedWriter) Len() int { return w.b.Len() }

// WriteString 便捷写入（与 Write 同语义，限量截断）。
func (w *cappedWriter) WriteString(s string) { _, _ = w.Write([]byte(s)) }

// String 输出内容，截断时附模型可见的标记。
func (w *cappedWriter) String() string {
	if w.truncated {
		return w.b.String() + "\n...[output truncated]"
	}
	return w.b.String()
}

// raw 输出原始内容（不含截断标记）：逐行解析输出的场景（grep 结果）
// 用 String() 会把 "...[output truncated]" 标记行当成一条结果解析进去。
func (w *cappedWriter) raw() string { return w.b.String() }

// execBuiltin 执行内置工具的 exec 请求。
// 只读：grep_args / read_args / ls_args；写操作（AGENT 模式）：shell/write/delete/edit。
func (r *Run) execBuiltin(name string, args *dynamicpb.Message) (*dynamicpb.Message, string, bool) {
	ecm, err := r.reg.New("agent.v1.ExecClientMessage")
	if err != nil {
		return nil, "", false
	}
	switch name {
	case "grep_args":
		res := sub(ecm, "grep_result")
		execGrep(args, res)
		return ecm, "grep_result", true
	case "read_args":
		res := sub(ecm, "read_result")
		execRead(args, res)
		return ecm, "read_result", true
	case "ls_args":
		res := sub(ecm, "ls_result")
		execLs(args, res)
		return ecm, "ls_result", true
	case "shell_args":
		res := sub(ecm, "shell_result")
		r.execShell(args, res)
		return ecm, "shell_result", true
	case "write_args":
		res := sub(ecm, "write_result")
		execWrite(args, res)
		return ecm, "write_result", true
	case "delete_args":
		res := sub(ecm, "delete_result")
		execDelete(args, res)
		return ecm, "delete_result", true
	case "edit_args":
		// 当前 schema 的 ExecClientMessage 没有 edit_result 字段（编辑经 write/shell
		// 上报），直接 sub 会 fdOf panic。分支保留，显式走拒绝路径。
		if ecm.Descriptor().Fields().ByName("edit_result") == nil {
			return nil, "", false
		}
		res := sub(ecm, "edit_result")
		execEdit(args, res)
		return ecm, "edit_result", true
	}
	return nil, "", false
}

// execGrep 用系统 rg 执行搜索。
func execGrep(args, res *dynamicpb.Message) {
	pattern := getStr(args, "pattern")
	path := getStr(args, "path")
	glob := getStr(args, "glob")
	outputMode := getStr(args, "output_mode")
	if path == "" {
		path = "."
	}
	if pattern == "" {
		// 空 pattern 时 rg 会把 path 当 pattern、从 stdin 读——行为怪异且必然空结果，显式拒绝
		re := sub(res, "error")
		setStr(re, "error", "pattern is required")
		return
	}

	rgArgs := []string{"--no-config", "--color=never", "--with-filename"}
	switch outputMode {
	case "files_with_matches":
		rgArgs = append(rgArgs, "-l")
	case "count":
		rgArgs = append(rgArgs, "-c")
	default: // content
		rgArgs = append(rgArgs, "-n", "--no-heading")
	}
	if glob != "" {
		rgArgs = append(rgArgs, "-g", glob)
	}
	if getStr(args, "case_insensitive") == "true" {
		rgArgs = append(rgArgs, "-i")
	}
	// 上下文行：context 是 -C，context_before/after 单独指定时优先
	if cb := int(getInt64(args, "context_before")); cb > 0 {
		rgArgs = append(rgArgs, "-B", fmt.Sprint(cb))
	}
	if ca := int(getInt64(args, "context_after")); ca > 0 {
		rgArgs = append(rgArgs, "-A", fmt.Sprint(ca))
	}
	if cb := int(getInt64(args, "context")); cb > 0 {
		rgArgs = append(rgArgs, "-C", fmt.Sprint(cb))
	}
	if ty := getStr(args, "type"); ty != "" {
		rgArgs = append(rgArgs, "--type", ty)
	}
	if getStr(args, "multiline") == "true" {
		rgArgs = append(rgArgs, "-U")
	}
	if s := getStr(args, "sort"); s != "" {
		// rg --sort 支持 path/modified/accessed/created/none；非法值 rg 会 exit 2 报错返回
		if getStr(args, "sort_ascending") == "true" {
			rgArgs = append(rgArgs, "--sortr", s)
		} else {
			rgArgs = append(rgArgs, "--sort", s)
		}
	}
	rgArgs = append(rgArgs, "--", pattern, path)

	// 超时 + 输出上限：巨型仓库的一次 content 搜索不能让内存/执行时间失控
	ctx, cancel := context.WithTimeout(context.Background(), grepTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rg", rgArgs...)
	out := &cappedWriter{limit: maxToolOutputBytes}
	errOut := &cappedWriter{limit: 64 << 10}
	cmd.Stdout = out
	cmd.Stderr = errOut
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		re := sub(res, "error")
		setStr(re, "error", "rg timed out after "+grepTimeout.String())
		return
	}
	if err != nil {
		if ee, isExit := err.(*exec.ExitError); isExit {
			// rg 约定：exit 0=有匹配，1=无匹配，2=出错（正则/路径/glob 非法）。
			// exit 2 必须报错——否则模型把"表达式写错了"当成"代码里不存在该模式"
			if ee.ExitCode() != 1 {
				re := sub(res, "error")
				msg := strings.TrimSpace(errOut.String())
				if msg == "" {
					msg = err.Error()
				}
				setStr(re, "error", "rg failed: "+msg)
				return
			}
		} else {
			re := sub(res, "error")
			setStr(re, "error", err.Error())
			return
		}
	}

	headLimit := int(getInt64(args, "head_limit"))
	offset := int(getInt64(args, "offset"))

	succ := sub(res, "success")
	setStr(succ, "pattern", pattern)
	setStr(succ, "path", path)
	setStr(succ, "output_mode", outputMode)

	// 逐行解析必须用 raw()：String() 截断时附加的标记行会被当成
	// 一条结果（files 模式混进假文件名、total_lines 多计一行）
	raw := out.raw()

	// workspace_results: map path -> GrepUnionResult
	fd := fdOf(succ.Descriptor(), "workspace_results")
	m := succ.Mutable(fd).Map()
	gu := dynamicpb.NewMessage(fd.MapValue().Message())
	switch outputMode {
	case "files_with_matches":
		fr := sub(gu, "files")
		var files []string
		for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
			if line != "" {
				files = append(files, line)
			}
		}
		if offset > 0 {
			if offset >= len(files) {
				files = nil
			} else {
				files = files[offset:]
			}
		}
		if headLimit > 0 && len(files) > headLimit {
			files = files[:headLimit]
		}
		fdF := fdOf(fr.Descriptor(), "files")
		for _, f := range files {
			fr.Mutable(fdF).List().Append(protoreflect.ValueOfString(f))
		}
		setInt(fr, "total_files", int32(len(files)))
	case "count":
		cr := sub(gu, "count")
		total := 0
		nFiles := 0
		fdC := cr.Descriptor().Fields().ByName("counts")
		for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
			if line == "" {
				continue
			}
			idx := strings.LastIndex(line, ":")
			if idx < 0 {
				continue
			}
			var n int
			if _, serr := fmt.Sscanf(line[idx+1:], "%d", &n); serr != nil || n == 0 {
				continue
			}
			if fdC != nil {
				fc := dynamicpb.NewMessage(fdC.Message())
				setStr(fc, "file", line[:idx])
				setInt(fc, "count", int32(n))
				cr.Mutable(fdC).List().Append(protoreflect.ValueOfMessage(fc))
			}
			total += n
			nFiles++
		}
		setInt(cr, "total_matches", int32(total))
		if fdC != nil {
			setInt(cr, "total_files", int32(nFiles))
		}
	default:
		con := sub(gu, "content")
		// 解析 rg -n --no-heading 输出：匹配行 "path:line:text"，
		// 上下文行（-B/-A/-C 时）"path-line-text"，组间分隔 "--"
		type fileMatches struct {
			file    string
			matches []protoreflect.Value
		}
		fdMatch := fdOf(con.Descriptor(), "matches")
		gcmDesc := fdMatch.Message().Fields().ByName("matches")
		var groups []*fileMatches
		byFile := map[string]*fileMatches{}
		totalLines, matched := 0, 0
		addLine := func(file string, lineNo int, text string, isCtx bool) bool {
			if headLimit > 0 && matched >= headLimit && !isCtx {
				return false
			}
			g := byFile[file]
			if g == nil {
				g = &fileMatches{file: file}
				byFile[file] = g
				groups = append(groups, g)
			}
			cm := dynamicpb.NewMessage(gcmDesc.Message())
			setInt(cm, "line_number", int32(lineNo))
			const maxLineBytes = 2000 // 单行防御：minified js 一行可达 MB 级
			if len(text) > maxLineBytes {
				text = text[:maxLineBytes]
				setBool(cm, "content_truncated", true)
			}
			setStr(cm, "content", text)
			if isCtx {
				setBool(cm, "is_context_line", true)
			}
			g.matches = append(g.matches, protoreflect.ValueOfMessage(cm))
			if !isCtx {
				matched++
			}
			return true
		}
		truncatedByHead := false
	parseLoop:
		for _, line := range strings.Split(strings.TrimRight(raw, "\n"), "\n") {
			if line == "" || line == "--" {
				continue
			}
			totalLines++
			// 匹配行与上下文行的分隔符不同（: vs -），line number 两侧都是分隔符
			sep := byte(':')
			isCtx := false
			idx1 := strings.IndexByte(line, sep)
			if idx1 > 0 {
				rest := line[idx1+1:]
				idx2 := strings.IndexByte(rest, sep)
				if idx2 < 0 || !isUint(rest[:idx2]) {
					sep = '-'
					isCtx = true
					idx1 = strings.IndexByte(line, sep)
				}
			}
			if idx1 <= 0 {
				continue
			}
			rest := line[idx1+1:]
			idx2 := strings.IndexByte(rest, sep)
			if idx2 < 0 {
				continue
			}
			numStr, text := rest[:idx2], rest[idx2+1:]
			var lineNo int
			if _, serr := fmt.Sscanf(numStr, "%d", &lineNo); serr != nil {
				continue
			}
			file := line[:idx1]
			if offset > 0 && !isCtx {
				offset--
				continue
			}
			if !addLine(file, lineNo, text, isCtx) {
				truncatedByHead = true
				break parseLoop
			}
		}
		if truncatedByHead {
			setInt(con, "head_limit_applied", int32(headLimit))
		}
		for _, g := range groups {
			fm := dynamicpb.NewMessage(fdMatch.Message())
			setStr(fm, "file", g.file)
			lst := fm.Mutable(gcmDesc).List()
			for _, v := range g.matches {
				lst.Append(v)
			}
			con.Mutable(fdMatch).List().Append(protoreflect.ValueOfMessage(fm))
		}
		setInt(con, "total_lines", int32(totalLines))
		setInt(con, "total_matched_lines", int32(matched))
		if out.truncated {
			setBool(con, "client_truncated", true)
		}
	}
	m.Set(protoreflect.ValueOfString(path).MapKey(), protoreflect.ValueOfMessage(gu))
}

// isUint 判断字符串是否全为数字（rg 输出的行号部分）。
func isUint(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// execRead 读文件（支持行范围）。
// 限量读取：模型可能指向超大文件（日志/数据集），全量读入会撑爆内存。
func execRead(args, res *dynamicpb.Message) {
	path := getStr(args, "path")
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			nf := sub(res, "file_not_found")
			setStr(nf, "path", path)
			return
		}
		re := sub(res, "error")
		setStr(re, "path", path)
		setStr(re, "error", err.Error())
		return
	}
	// FIFO/设备/socket 上的 Open/Read 会永久阻塞（无写端的管道永不返回），
	// 而该 goroutine 没有超时兜底——非常规文件直接拒绝，且必须先于 Open
	if !info.Mode().IsRegular() {
		re := sub(res, "error")
		setStr(re, "path", path)
		setStr(re, "error", "not a regular file ("+info.Mode().Type().String()+")")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		re := sub(res, "error")
		setStr(re, "path", path)
		setStr(re, "error", err.Error())
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxReadFileBytes+1))
	if err != nil {
		re := sub(res, "error")
		setStr(re, "path", path)
		setStr(re, "error", err.Error())
		return
	}
	truncated := len(data) > maxReadFileBytes
	if truncated {
		data = data[:maxReadFileBytes]
	}

	content := string(data)
	lines := strings.Split(content, "\n")
	// 注：文件被字节上限截断时，total_lines 是截断后内容的行数
	total := len(lines)

	offset := int(getInt64(args, "offset"))
	limit := int(getUint(args, "limit"))
	if offset > 0 || limit > 0 {
		// offset 是 1-based 起始行（Cursor/Claude Read 工具约定），转 0-based 切片下标
		start := offset - 1
		if start < 0 {
			start = 0
		}
		if start > total {
			start = total
		}
		end := total
		if limit > 0 && start+limit < end {
			end = start + limit
			// 不标 truncated：模型主动请求的行范围由 range_applied 表达，
			// truncated 只表示"文件超出读取上限被强制切断"，混用会让模型误判内容缺失
		}
		lines = lines[start:end]
		content = strings.Join(lines, "\n")
	}

	succ := sub(res, "success")
	setStr(succ, "path", path)
	setStr(succ, "content", content)
	setInt(succ, "total_lines", int32(total))
	fdSize := fdOf(succ.Descriptor(), "file_size")
	succ.Set(fdSize, protoreflect.ValueOfInt64(info.Size())) // 真实大小（非截断后）
	if truncated {
		setBool(succ, "truncated", true)
	}
	setBool(succ, "range_applied", offset > 0 || limit > 0)
}

// execLs 列目录（一层 + 递归树）。
func execLs(args, res *dynamicpb.Message) {
	path := getStr(args, "path")
	if path == "" {
		path = "."
	}
	// 顶层错误必须显式上报：静默返回空树会让模型把
	//"目录不存在/无权限"当成"空目录"，按错误前提作答
	info, err := os.Stat(path)
	if err != nil {
		re := sub(res, "error")
		setStr(re, "path", path)
		setStr(re, "error", err.Error())
		return
	}
	if !info.IsDir() {
		re := sub(res, "error")
		setStr(re, "path", path)
		setStr(re, "error", "not a directory")
		return
	}
	if _, err := os.ReadDir(path); err != nil {
		re := sub(res, "error")
		setStr(re, "path", path)
		setStr(re, "error", err.Error())
		return
	}
	// ignore：条目名精确匹配或 glob 模式（如 "node_modules"、"*.log"）
	var ignore []string
	if fd := args.Descriptor().Fields().ByName("ignore"); fd != nil {
		lst := args.Get(fd).List()
		for i := 0; i < lst.Len(); i++ {
			ignore = append(ignore, lst.Get(i).String())
		}
	}
	root := sub(sub(res, "success"), "directory_tree_root")
	remaining := maxLsEntries
	buildTreeCapped(root, path, 3, ignore, &remaining)
}

// buildTreeCapped 递归建树，remaining 是全树共享的条目预算：
// 巨型目录（node_modules 级）能把响应帧撑到上限之外，必须在收集侧截断。
func buildTreeCapped(node *dynamicpb.Message, path string, depth int, ignore []string, remaining *int) {
	setStr(node, "abs_path", path)
	if depth <= 0 || *remaining <= 0 {
		return
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		// 子目录读取失败（权限/竞态删除）：跳过该分支，顶层失败已在 execLs 拦截
		return
	}
	numFiles := 0
	for _, e := range entries {
		if *remaining <= 0 {
			break
		}
		// 过滤先于预算扣减：隐藏文件/ignore 条目不显示，不该烧配额
		if strings.HasPrefix(e.Name(), ".") || matchIgnore(e.Name(), ignore) {
			continue
		}
		*remaining--
		if e.IsDir() {
			child := appendSub(node, "children_dirs")
			buildTreeCapped(child, filepath.Join(path, e.Name()), depth-1, ignore, remaining)
		} else {
			f := appendSub(node, "children_files")
			setStr(f, "name", e.Name())
			numFiles++
		}
	}
	setInt(node, "num_files", int32(numFiles))
	setBool(node, "children_were_processed", true)
}

// matchIgnore 条目名命中忽略列表（精确名或 glob）。
func matchIgnore(name string, ignore []string) bool {
	for _, pat := range ignore {
		if pat == name {
			return true
		}
		if ok, _ := filepath.Match(pat, name); ok {
			return true
		}
	}
	return false
}

// ---- 写操作（AGENT 模式）----

// execShell 本地执行 shell 命令（在 handleExec 派发的 goroutine 中运行）。
func (r *Run) execShell(args, res *dynamicpb.Message) {
	command := getStr(args, "command")
	workDir := getStr(args, "working_directory")
	timeoutMs := getInt64(args, "timeout")
	if workDir == "" {
		workDir = "."
	}
	if timeoutMs <= 0 {
		timeoutMs = 30000
	}
	if timeoutMs > maxShellTimeoutMs {
		timeoutMs = maxShellTimeoutMs
	}

	cmd := shellCommand(command)
	cmd.Dir = workDir
	setProcGroup(cmd)
	// WaitDelay 兜底：后台孙进程继承输出管道时 Wait 不会被管道卡住，
	// 进程死后最多再等 waitDelay 强制返回（timeout/Close 回收路径不被楔死）
	cmd.WaitDelay = 5 * time.Second
	// 输出限量收集：`yes` 类命令在超时窗口内能攒出 GB 级输出
	stdoutBuf := &cappedWriter{limit: maxToolOutputBytes}
	stderrBuf := &cappedWriter{limit: maxToolOutputBytes}
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	errCh := make(chan error, 1)
	go func() { errCh <- cmd.Run() }()

	timer := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-errCh:
		fillShellResult(res, command, workDir, stdoutBuf.String(), stderrBuf.String(), err)
	case <-timer.C:
		killTree(cmd)
		<-errCh
		to := sub(res, "timeout")
		setStr(to, "command", command)
		setStr(to, "working_directory", workDir)
		setInt(to, "timeout_ms", int32(timeoutMs))
	case <-r.closeCh:
		// Run 已终止（客户端断连/超时）：强杀进程组，避免孤儿命令跑满 timeout。
		// 此时结果已发不出去（流已关），填 error 仅为日志/调试语义正确，不冒充 timeout
		killTree(cmd)
		<-errCh
		re := sub(res, "error")
		setStr(re, "command", command)
		setStr(re, "error", "run terminated while command was running")
	}
}

func fillShellResult(res *dynamicpb.Message, command, workDir, stdout, stderr string, err error) {
	exitCode := 0
	target := "success"
	if err != nil {
		exitCode = 1
		target = "failure"
		if ee, ok := err.(*exec.ExitError); ok {
			if code := ee.ExitCode(); code >= 0 {
				exitCode = code
			}
			// code < 0 = 信号致死，保持 1（与 exit_code 语义一致：非正常退出）
		} else {
			// 非退出类错误（chdir 失败等）：真实原因进 stderr，模型才能自愈
			stderr = err.Error() + "\n" + stderr
		}
	}
	s := sub(res, target)
	setStr(s, "command", command)
	setStr(s, "working_directory", workDir)
	setInt(s, "exit_code", int32(exitCode))
	setStr(s, "stdout", stdout)
	setStr(s, "stderr", stderr)
	setStr(s, "interleaved_output", stdout+stderr)
}

// execWrite 写文件。
func execWrite(args, res *dynamicpb.Message) {
	path := getStr(args, "path")
	text := getStr(args, "file_text")
	var data []byte
	if bFd := args.Descriptor().Fields().ByName("file_bytes"); bFd != nil && args.Has(bFd) {
		data = args.Get(bFd).Bytes()
	} else {
		data = []byte(text)
	}
	err := os.MkdirAll(filepath.Dir(path), 0755)
	if err == nil {
		// 已存在的 FIFO/设备/socket：WriteFile 的 open 会永久阻塞（该 goroutine 无超时），拒绝
		if info, serr := os.Lstat(path); serr == nil && !info.Mode().IsRegular() {
			err = fmt.Errorf("not a regular file (%s)", info.Mode().Type())
		}
	}
	if err == nil {
		err = os.WriteFile(path, data, 0644)
	}
	if err != nil {
		re := sub(res, "error")
		setStr(re, "path", path)
		setStr(re, "error", err.Error())
		return
	}
	succ := sub(res, "success")
	setStr(succ, "path", path)
	setInt(succ, "lines_created", int32(strings.Count(string(data), "\n")+1))
	setInt(succ, "file_size", int32(len(data)))
	if getStr(args, "return_file_content_after_write") == "true" || has(args, "return_file_content_after_write") {
		setStr(succ, "file_content_after_write", string(data))
	}
}

// execDelete 删除文件。
func execDelete(args, res *dynamicpb.Message) {
	path := getStr(args, "path")
	info, err := os.Stat(path)
	if err != nil {
		nf := sub(res, "file_not_found")
		setStr(nf, "path", path)
		return
	}
	if info.IsDir() {
		nf := sub(res, "not_file")
		setStr(nf, "path", path)
		return
	}
	if err := os.Remove(path); err != nil {
		re := sub(res, "error")
		setStr(re, "path", path)
		setStr(re, "error", err.Error())
		return
	}
	succ := sub(res, "success")
	setStr(succ, "path", path)
	setStr(succ, "deleted_file", path)
	fdSize := fdOf(succ.Descriptor(), "file_size")
	succ.Set(fdSize, protoreflect.ValueOfInt64(info.Size()))
}

// execEdit 应用编辑（stream_content 为完整新内容）。
func execEdit(args, res *dynamicpb.Message) {
	path := getStr(args, "path")
	newContent := getStr(args, "stream_content")
	// 同 execWrite：已存在的非常规文件会让 ReadFile/WriteFile 永久阻塞（先于任何 I/O 检查）
	if info, serr := os.Lstat(path); serr == nil {
		if !info.Mode().IsRegular() {
			re := sub(res, "error")
			setStr(re, "path", path)
			errMsg := "not a regular file (" + info.Mode().Type().String() + ")"
			setStr(re, "error", errMsg)
			setStr(re, "model_visible_error", errMsg)
			return
		}
		// before 是全量读入做 diff 统计的：巨型文件会撑爆内存（同 execRead 上限）
		if info.Size() > maxReadFileBytes {
			re := sub(res, "error")
			setStr(re, "path", path)
			errMsg := fmt.Sprintf("file too large to edit (%d bytes > %d limit)", info.Size(), maxReadFileBytes)
			setStr(re, "error", errMsg)
			setStr(re, "model_visible_error", errMsg)
			return
		}
	}
	before, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(newContent), 0644); err != nil {
		re := sub(res, "error")
		setStr(re, "path", path)
		setStr(re, "error", err.Error())
		setStr(re, "model_visible_error", err.Error())
		return
	}
	beforeLines := strings.Split(string(before), "\n")
	afterLines := strings.Split(newContent, "\n")
	succ := sub(res, "success")
	setStr(succ, "path", path)
	setInt(succ, "lines_added", int32(max(0, len(afterLines)-len(beforeLines))))
	setInt(succ, "lines_removed", int32(max(0, len(beforeLines)-len(afterLines))))
	setStr(succ, "before_full_file_content", string(before))
	setStr(succ, "after_full_file_content", newContent)
}
