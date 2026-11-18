package store

import (
	"errors"
	"strings"

	"github.com/zggnh12345/mirror-lens/internal/registryhost"
)

// starRaceKeyPrefix 是 Host 级竞速总开关在 settings 表里的键前缀。
//
// 3.5.16 的 Host 总开关不新建 hosts 表：一个 Host 一个设置项，缺省即关闭。
// 这样升级后行为与基线完全一致（没有任何设置项 = 所有 Host 关闭），也不需要
// 迁移脚本去猜哪些 Host 该开。
const starRaceKeyPrefix = "registry_star_race:"

// ErrStarRaceHost 表示 Host 不能作为竞速开关的键。
var ErrStarRaceHost = errors.New("竞速开关的 Host 不合法")

// StarRaceKey 由 Host 生成设置键；Host 走 registryhost 规范化，
// docker.io / registry-1.docker.io / index.docker.io 收敛成同一个键。
func StarRaceKey(rawHost string) (string, error) {
	h, err := registryhost.Normalize(rawHost)
	if err != nil {
		return "", err
	}
	if h == "" {
		return "", ErrStarRaceHost
	}
	return starRaceKeyPrefix + h, nil
}

// StarRaceEnabled 判断某 Host 是否开启公开 Manifest 竞速。
// 没有设置项一律按关闭处理（只认显式 "1"）。
func (s *Store) StarRaceEnabled(rawHost string) bool {
	key, err := StarRaceKey(rawHost)
	if err != nil {
		return false
	}
	return s.GetSetting(key, "0") == "1"
}

// StarRaceHosts 返回当前显式开启竞速的 Host 集合（键为规范化 Host）。
func (s *Store) StarRaceHosts() map[string]bool {
	out := map[string]bool{}
	for k, v := range s.AllSettings() {
		if !strings.HasPrefix(k, starRaceKeyPrefix) || v != "1" {
			continue
		}
		h := strings.TrimPrefix(k, starRaceKeyPrefix)
		if h == "" {
			continue
		}
		out[h] = true
	}
	return out
}

// SetStarRace 写入 Host 级竞速总开关。
//
// 写库前做与写入路径一致的 Host 校验：只有规范化后仍在回源白名单内的 Host
// 才允许出现开关项，避免设置表里堆一堆用不上的键。
func (s *Store) SetStarRace(rawHost string, on bool) error {
	h, err := registryhost.Check(rawHost)
	if err != nil {
		return err
	}
	if h == "" {
		return ErrStarRaceHost
	}
	v := "0"
	if on {
		v = "1"
	}
	return s.SetSetting(starRaceKeyPrefix+h, v)
}
