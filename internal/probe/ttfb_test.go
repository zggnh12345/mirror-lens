package probe

import (
	"bytes"
	"io"
	"testing"
	"time"
)

// 首字节时间在第一次读到数据时定下来，之后再读也不改。
func TestTTFBReaderRecordsFirstByte(t *testing.T) {
	slow := &slowReader{r: bytes.NewReader(bytes.Repeat([]byte("x"), 64)), delay: 30 * time.Millisecond}
	tr := &ttfbReader{r: slow, start: time.Now()}

	buf := make([]byte, 8)
	if _, err := tr.Read(buf); err != nil {
		t.Fatal(err)
	}
	first := tr.ms
	if !tr.hit || first < 20 {
		t.Fatalf("第一次读应记下 >=20ms，得到 %d（hit=%v）", first, tr.hit)
	}

	time.Sleep(20 * time.Millisecond)
	if _, err := tr.Read(buf); err != nil {
		t.Fatal(err)
	}
	if tr.ms != first {
		t.Fatalf("第二次读不该改写首字节时间：%d → %d", first, tr.ms)
	}
}

// 一个字节都没读到时保持 0，不能把「没测」记成「很快」。
func TestTTFBReaderEmpty(t *testing.T) {
	tr := &ttfbReader{r: bytes.NewReader(nil), start: time.Now()}
	if n, err := tr.Read(make([]byte, 4)); n != 0 || err != io.EOF {
		t.Fatalf("空 body 应返回 EOF，得到 n=%d err=%v", n, err)
	}
	if tr.hit || tr.ms != 0 {
		t.Fatalf("没读到数据时不该记首字节时间，得到 %d", tr.ms)
	}
}

type slowReader struct {
	r     io.Reader
	delay time.Duration
	done  bool
}

func (s *slowReader) Read(p []byte) (int, error) {
	if !s.done {
		s.done = true
		time.Sleep(s.delay)
	}
	return s.r.Read(p)
}
