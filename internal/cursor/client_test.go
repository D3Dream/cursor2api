package cursor

import (
	"bytes"
	"strings"
	"testing"
)

// 回归：readLoop 曾把 recover+emit 与 close(events) 拆成两个 defer，
// LIFO 顺序导致先 close 再 emit → send on closed channel 二次 panic，
// panic 防护反而直接炸掉进程。现在 panic 必须转成 EventError 事件，
// events 随后正常关闭，进程存活（进程死了本测试也过不了）。
func TestReadLoopPanicBecomesErrorEvent(t *testing.T) {
	r := &Run{
		events:  make(chan Event, 1),
		closeCh: make(chan struct{}),
		// reg 为 nil：Unmarshal 必然解引用 panic，模拟 schema 缺失类故障
	}
	// 合法帧头（未压缩、2 字节负载）+ 任意负载
	frame := []byte{0x00, 0x00, 0x00, 0x00, 0x02, 0x08, 0x01}
	r.readLoop(bytes.NewReader(frame))

	ev, ok := <-r.events
	if !ok {
		t.Fatal("readLoop 退出前应投递 EventError，而不是直接关闭 channel")
	}
	if ev.Kind != EventError {
		t.Fatalf("kind = %v, want EventError", ev.Kind)
	}
	if ev.Err == nil || !strings.Contains(ev.Err.Error(), "panic") {
		t.Fatalf("err = %v, want panic 描述", ev.Err)
	}
	if _, ok := <-r.events; ok {
		t.Fatal("readLoop 返回后 events 应关闭")
	}
}

// 正常路径防御：帧损坏（超长声明）应得到错误事件而非静默。
func TestReadLoopOversizeFrame(t *testing.T) {
	r := &Run{
		events:  make(chan Event, 1),
		closeCh: make(chan struct{}),
	}
	frame := []byte{0x00, 0x08, 0x00, 0x00, 0x01} // 0x08000001 = 134217729 > 64MB 上限
	r.readLoop(bytes.NewReader(frame))
	ev, ok := <-r.events
	if !ok || ev.Kind != EventError {
		t.Fatalf("超大帧应产生 EventError, got ok=%v kind=%v", ok, ev.Kind)
	}
}
