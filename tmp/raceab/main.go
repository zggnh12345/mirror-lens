// 实测工具：对真实源做 250ms 与 1ms 起跑的 A/B 拨号对比。
// HOSTS=逗号分隔域名 N=轮数（默认5） go run ./tmp/raceab
package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/safehttp"
)

type res struct {
	host  string
	delay string
	ms    []int
	won   string
	fail  int
}

func main() {
	hosts := strings.Split(os.Getenv("HOSTS"), ",")
	n := 5
	if v := os.Getenv("N"); v != "" {
		n, _ = strconv.Atoi(v)
	}
	for _, raw := range hosts {
		host := strings.TrimSpace(raw)
		if host == "" {
			continue
		}
		var v4, v6 []string
		for _, ip := range []string{"4", "6"} {
			addrs, err := net.LookupIP(host)
			_ = err
			for _, a := range addrs {
				s := a.String()
				if strings.Contains(s, ":") {
					if ip == "6" {
						v6 = append(v6, s)
					}
				} else if ip == "4" {
					v4 = append(v4, s)
				}
			}
		}
		fmt.Printf("HOST %s v4=%d v6=%d\n", host, len(v4), len(v6))
		if len(v6) == 0 || len(v4) == 0 {
			fmt.Println("  SKIP non-dual")
			continue
		}
		for _, d := range []struct {
			name string
			v    time.Duration
		}{{"250ms", 250 * time.Millisecond}, {"1ms", time.Millisecond}} {
			g := safehttp.New(safehttp.Options{DialTimeout: 8 * time.Second, FallbackDelay: d.v})
			var ms []int
			var won string
			fail := 0
			for i := 0; i < n; i++ {
				t0 := time.Now()
				c, err := g.Dialer().Dial("tcp", host+":443")
				el := time.Since(t0).Milliseconds()
				if err != nil {
					fail++
					fmt.Printf("  %s run%d FAIL %v\n", d.name, i, err)
					continue
				}
				won = c.RemoteAddr().String()
				c.Close()
				ms = append(ms, int(el))
			}
			fmt.Printf("  %s ms=%v winner=%s fail=%d\n", d.name, ms, won, fail)
		}
	}
}
