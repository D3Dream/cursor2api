package cursor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cursor2api/internal/schema"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// testRegistry 加载真实 FDS（Grep* 消息结构以它为准）。
func testRegistry(t *testing.T) *schema.Registry {
	t.Helper()
	reg, err := schema.Load(filepath.Join("..", "..", "schema", "cursor_fds.json"))
	if err != nil {
		t.Fatalf("Load FDS: %v", err)
	}
	return reg
}

// fakeRg 在 PATH 里造一个假的 rg：按参数行为输出固定内容或以指定码退出。
func fakeRg(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "rg")
	if err := os.WriteFile(p, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

func newGrepArgs(t *testing.T, reg *schema.Registry, fields map[string]any) *dynamicpb.Message {
	t.Helper()
	m, err := reg.New("agent.v1.GrepArgs")
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range fields {
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(k))
		if fd == nil {
			t.Fatalf("GrepArgs no field %s", k)
		}
		switch val := v.(type) {
		case string:
			m.Set(fd, protoreflect.ValueOfString(val))
		case int32:
			m.Set(fd, protoreflect.ValueOfInt32(val))
		}
	}
	return m
}

// rg exit 2（正则/glob 非法）必须报 error，不能被当成"无匹配"成功——
// 否则模型会把"表达式写错了"当成"代码里不存在该模式"。
func TestExecGrepExit2IsError(t *testing.T) {
	reg := testRegistry(t)
	fakeRg(t, "#!/bin/sh\necho 'rg: regex parse error' >&2\nexit 2\n")
	args := newGrepArgs(t, reg, map[string]any{"pattern": "[a-", "path": "."})
	res, err := reg.New("agent.v1.GrepResult")
	if err != nil {
		t.Fatal(err)
	}
	execGrep(args, res)
	if !has(res, "error") {
		t.Fatal("rg exit 2 未走 error 分支")
	}
	em, ok := get(res, "error")
	if !ok {
		t.Fatal("error 子消息不存在")
	}
	msg := getStr(em, "error")
	if msg == "" {
		t.Fatal("error 消息为空（stderr 未被捕获）")
	}
}

// rg exit 1 = 无匹配，是正常空结果（不报错）。
func TestExecGrepExit1IsEmptySuccess(t *testing.T) {
	reg := testRegistry(t)
	fakeRg(t, "#!/bin/sh\nexit 1\n")
	args := newGrepArgs(t, reg, map[string]any{"pattern": "nomatch", "path": "."})
	res, err := reg.New("agent.v1.GrepResult")
	if err != nil {
		t.Fatal(err)
	}
	execGrep(args, res)
	if has(res, "error") {
		if em, ok := get(res, "error"); ok {
			t.Fatalf("rg exit 1 不应报错: %v", getStr(em, "error"))
		}
	}
	if !has(res, "success") {
		t.Fatal("rg exit 1 未走 success 分支")
	}
}

// content 模式必须填充 matches（file + line_number + content），
// 而不是只给一个计数让模型瞎猜。
func TestExecGrepContentMatches(t *testing.T) {
	reg := testRegistry(t)
	fakeRg(t, "#!/bin/sh\nprintf 'main.go:12:hello world\\nutil.go:34:hello again\\n'\nexit 0\n")
	args := newGrepArgs(t, reg, map[string]any{"pattern": "hello", "path": "."})
	res, err := reg.New("agent.v1.GrepResult")
	if err != nil {
		t.Fatal(err)
	}
	execGrep(args, res)
	succ, ok := get(res, "success")
	if !ok {
		t.Fatal("success 子消息不存在")
	}
	fd := succ.Descriptor().Fields().ByName("workspace_results")
	m := succ.Get(fd).Map()
	var gu *dynamicpb.Message
	m.Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
		gu = v.Message().Interface().(*dynamicpb.Message)
		return false
	})
	if gu == nil {
		t.Fatal("workspace_results 为空")
	}
	con, ok := get(gu, "content")
	if !ok {
		t.Fatal("content 子消息不存在")
	}
	if got := getInt64(con, "total_matched_lines"); got != 2 {
		t.Fatalf("total_matched_lines = %d, want 2", got)
	}
	fdM := con.Descriptor().Fields().ByName("matches")
	lst := con.Get(fdM).List()
	if lst.Len() != 2 {
		t.Fatalf("matches 文件组数 = %d, want 2", lst.Len())
	}
	first := lst.Get(0).Message().Interface().(*dynamicpb.Message)
	if getStr(first, "file") != "main.go" {
		t.Fatalf("matches[0].file = %q", getStr(first, "file"))
	}
	fdMM := first.Descriptor().Fields().ByName("matches")
	mm := first.Get(fdMM).List()
	if mm.Len() != 1 {
		t.Fatalf("matches[0].matches 行数 = %d, want 1", mm.Len())
	}
	line := mm.Get(0).Message().Interface().(*dynamicpb.Message)
	if getInt64(line, "line_number") != 12 || getStr(line, "content") != "hello world" {
		t.Fatalf("匹配行内容错误: line=%d content=%q", getInt64(line, "line_number"), getStr(line, "content"))
	}
}

// head_limit 截断并标记。
func TestExecGrepHeadLimit(t *testing.T) {
	reg := testRegistry(t)
	fakeRg(t, "#!/bin/sh\nprintf 'a.go:1:x\\na.go:2:y\\na.go:3:z\\n'\nexit 0\n")
	args := newGrepArgs(t, reg, map[string]any{"pattern": "x", "path": ".", "head_limit": int32(2)})
	res, err := reg.New("agent.v1.GrepResult")
	if err != nil {
		t.Fatal(err)
	}
	execGrep(args, res)
	succ, ok := get(res, "success")
	if !ok {
		t.Fatal("success 子消息不存在")
	}
	fd := succ.Descriptor().Fields().ByName("workspace_results")
	m := succ.Get(fd).Map()
	var gu *dynamicpb.Message
	m.Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
		gu = v.Message().Interface().(*dynamicpb.Message)
		return false
	})
	con, ok := get(gu, "content")
	if !ok {
		t.Fatal("content 子消息不存在")
	}
	if got := getInt64(con, "total_matched_lines"); got != 2 {
		t.Fatalf("head_limit=2 时 matched = %d, want 2", got)
	}
	if getInt64(con, "head_limit_applied") != 2 {
		t.Fatal("head_limit_applied 未标记")
	}
}

// 截断时 String() 附加的 "...[output truncated]" 标记行不得被当成一条结果：
// files 模式曾把它解析成一个假文件名混进列表。
func TestExecGrepTruncationMarkerNotInFiles(t *testing.T) {
	reg := testRegistry(t)
	// 输出超 1MB 触发 cappedWriter 截断（head 先退出，管道退出码为 0=有匹配）
	fakeRg(t, "#!/bin/sh\nyes '/repo/a.go' | head -c 1500000\n")
	args := newGrepArgs(t, reg, map[string]any{"pattern": "x", "path": ".", "output_mode": "files_with_matches"})
	res, err := reg.New("agent.v1.GrepResult")
	if err != nil {
		t.Fatal(err)
	}
	execGrep(args, res)
	succ, ok := get(res, "success")
	if !ok {
		t.Fatal("success 子消息不存在")
	}
	fd := succ.Descriptor().Fields().ByName("workspace_results")
	m := succ.Get(fd).Map()
	var gu *dynamicpb.Message
	m.Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
		gu = v.Message().Interface().(*dynamicpb.Message)
		return false
	})
	fr, ok := get(gu, "files")
	if !ok {
		t.Fatal("files 子消息不存在")
	}
	fdF := fr.Descriptor().Fields().ByName("files")
	lst := fr.Get(fdF).List()
	if lst.Len() == 0 {
		t.Fatal("files 列表为空（1.5MB 输出应有大量条目）")
	}
	for i := 0; i < lst.Len(); i++ {
		if strings.Contains(lst.Get(i).String(), "truncated") {
			t.Fatalf("截断标记混入 files[%d]: %q", i, lst.Get(i).String())
		}
	}
}

// cappedWriter.raw() 必须返回不含截断标记的原始内容（String() 的标记是给模型看的）。
func TestCappedWriterRaw(t *testing.T) {
	w := &cappedWriter{limit: 5}
	w.WriteString("hello world")
	if !strings.Contains(w.String(), "output truncated") {
		t.Fatalf("String() 应含截断标记: %q", w.String())
	}
	if got := w.raw(); got != "hello" {
		t.Fatalf("raw() = %q, want %q", got, "hello")
	}
}

func newLsArgs(t *testing.T, reg *schema.Registry, path string) *dynamicpb.Message {
	t.Helper()
	m, err := reg.New("agent.v1.LsArgs")
	if err != nil {
		t.Fatal(err)
	}
	fd := m.Descriptor().Fields().ByName("path")
	if fd == nil {
		t.Skip("LsArgs 无 path 字段")
	}
	m.Set(fd, protoreflect.ValueOfString(path))
	return m
}

// ls 顶层错误必须显式上报：静默返回空树会让模型把
// "目录不存在/无权限"当成"空目录"，按错误前提作答。
func TestExecLsNonexistentPathIsError(t *testing.T) {
	reg := testRegistry(t)
	args := newLsArgs(t, reg, filepath.Join(t.TempDir(), "no-such-dir"))
	res, err := reg.New("agent.v1.LsResult")
	if err != nil {
		t.Fatal(err)
	}
	execLs(args, res)
	if has(res, "success") {
		t.Fatal("不存在的路径不应走 success 分支")
	}
	em, ok := get(res, "error")
	if !ok {
		t.Fatal("不存在的路径未走 error 分支")
	}
	if getStr(em, "error") == "" {
		t.Fatal("error 消息为空")
	}
}

// ls 目标是普通文件（不是目录）也必须报错。
func TestExecLsFilePathIsError(t *testing.T) {
	reg := testRegistry(t)
	f := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	args := newLsArgs(t, reg, f)
	res, err := reg.New("agent.v1.LsResult")
	if err != nil {
		t.Fatal(err)
	}
	execLs(args, res)
	if !has(res, "error") {
		t.Fatal("对文件执行 ls 未走 error 分支")
	}
}

// 正常目录：success 且树里有文件；隐藏文件不消耗条目预算。
func TestExecLsSuccess(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hidden"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	args := newLsArgs(t, reg, dir)
	res, err := reg.New("agent.v1.LsResult")
	if err != nil {
		t.Fatal(err)
	}
	execLs(args, res)
	succ, ok := get(res, "success")
	if !ok {
		t.Fatal("正常目录未走 success 分支")
	}
	root, ok := get(succ, "directory_tree_root")
	if !ok {
		t.Fatal("directory_tree_root 不存在")
	}
	if getInt64(root, "num_files") != 1 {
		t.Fatalf("num_files = %d, want 1（隐藏文件不计）", getInt64(root, "num_files"))
	}
}
