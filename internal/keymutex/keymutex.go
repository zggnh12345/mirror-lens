// Package keymutex 提供按 key 的互斥锁，带引用计数回收。
//
// 直接存 key→*sync.Mutex 只增不减，key 一多内存就涨；这里减到零即删。
// 计数包含等待中的协程，不会出现两把锁同时保护一个 key。
package keymutex

import "sync"

type entry struct {
	mu   sync.Mutex
	refs int
}

// Map 是按键分组的互斥锁，零值不可用，用 New。
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

// Len 返回存活 key 数，仅用于测试与观测。
func (k *Map) Len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.m)
}
