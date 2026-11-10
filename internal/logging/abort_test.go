package logging

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
)

func TestIsAbort(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{context.Canceled, true},
		{net.ErrClosed, true},
		{fmt.Errorf("dial: %w", context.Canceled), true},
		{errors.New("read tcp 10.0.0.1:1->10.0.0.2:2: use of closed network connection"), true},
		{errors.New("read tcp: connection reset by peer"), true},
		{errors.New("dial tcp 1.2.3.4:443: i/o timeout"), false},
		{errors.New("unexpected EOF"), false},
	}
	for _, c := range cases {
		if got := IsAbort(c.err); got != c.want {
			t.Errorf("IsAbort(%v)=%v want %v", c.err, got, c.want)
		}
	}
}
