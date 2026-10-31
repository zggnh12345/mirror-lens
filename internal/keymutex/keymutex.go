// Package keymutex 提供按 key 的互斥锁，带引用计数回收。
//
// 直接用 sync.Map 存 key→*sync.Mutex 的话，锁对象只增不减：请求量一大、
// key 一多，内存就跟着涨。这里在 Unlock 时把引用计数减到零的条目删掉；
// 计数包含「正在等锁的协程」，所以不会出现 A 还拿着旧锁、B 已经拿到新锁的情况。
package keymutex

import "sync"

type entry struct {
	mu   sync.Mutex
	refs int
}

// Map 是一组按 key 区分的互斥锁，零值不可用，用 New。
type Map struct {
	mu sync.Mutex
	m  map[string]*entry
}

func New() *Map {
	return &Map{m: make(map[string]*entry)}
}

// Lock 锁住 key，返回解锁函数。解锁函数只能调用一次。
func (k *Map) Lock(key string) func() {
	k.mu.Lock()
	e := k.m[key]
	if e == nil {
		e = &entry{}
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

// Len 返回当前存活的 key 数，仅用于测试与观测。
func (k *Map) Len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.m)
}
