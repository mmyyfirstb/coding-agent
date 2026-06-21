package terminal

import (
	"errors"
	"io"
	"testing"
)

// scriptedReader 按预设脚本逐次返回 Read 结果，用来复刻「真实 raw tty 经 os.File」的行为：
// 每个事件要么吐一个字节 (has=true)，要么以某个 err 返回 0 字节（超时用 io.EOF，
// 真正结束用其它错误）。脚本耗尽后一律返回 io.EOF。
type scriptedReader struct {
	ev []rdEvent
	i  int
}

type rdEvent struct {
	b   byte
	has bool
	err error
}

func (s *scriptedReader) Read(p []byte) (int, error) {
	if s.i >= len(s.ev) {
		return 0, io.EOF
	}
	e := s.ev[s.i]
	s.i++
	if e.has {
		p[0] = e.b
		return 1, nil
	}
	return 0, e.err
}

// PollingTTYReader 必须把 raw tty 读超时的 (0, io.EOF) 翻译成 (0, nil)；其余结果透传。
func TestPollingTTYReader(t *testing.T) {
	other := errors.New("boom")
	cases := []struct {
		name    string
		inN     int
		inErr   error
		wantN   int
		wantErr error
	}{
		{"超时 EOF→nil", 0, io.EOF, 0, nil},
		{"有数据透传", 1, nil, 1, nil},
		{"其它错误透传", 0, other, 0, other},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := PollingTTYReader(fixedReader{n: c.inN, err: c.inErr})
			n, err := r.Read(make([]byte, 1))
			if n != c.wantN || err != c.wantErr {
				t.Fatalf("得到 (%d,%v)，期望 (%d,%v)", n, err, c.wantN, c.wantErr)
			}
		})
	}
}

// fixedReader 每次 Read 都返回固定的 (n, err)，用于单测适配器本身。
type fixedReader struct {
	n   int
	err error
}

func (f fixedReader) Read(p []byte) (int, error) { return f.n, f.err }

// 回归：raw tty 空闲超时（os.File 把它当 io.EOF）不应让 pump 提前结束整个 REPL。
// 只有真正的设备错误（终端关闭等非 EOF 错误）才应让 pump 收尾。
func TestPump_PollingTTYIdleNotEOF(t *testing.T) {
	r := &scriptedReader{ev: []rdEvent{
		{err: io.EOF},                   // 空闲超时：os.File 行为，不是真结束
		{b: 'x', has: true},             // 随后真有输入
		{b: '\r', has: true},            // 回车
		{err: errors.New("tty closed")}, // 终端关闭：非 EOF 错误，pump 在此收尾
	}}
	ch := make(chan Key, 16)
	go pump(PollingTTYReader(r), ch)

	var ks []Key
	for k := range ch {
		ks = append(ks, k)
	}
	want := []Key{{Kind: KeyRune, Rune: 'x'}, {Kind: KeyEnter}}
	if len(ks) != len(want) {
		t.Fatalf("Key 数=%d，期望 %d：%+v", len(ks), len(want), ks)
	}
	for i := range want {
		if ks[i] != want[i] {
			t.Errorf("第 %d 个=%+v，期望 %+v", i, ks[i], want[i])
		}
	}
}
