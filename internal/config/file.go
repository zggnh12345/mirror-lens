package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const templateHeader = "# mirror-lens 配置：这里写的每个键都会生效；改完需要重启容器。\n" +
	"# 面板保存时会把当前生效值整份写回本文件（注释和未识别的键会保留）。\n" +
	"# 留空的字符串表示按 data 目录自动推导或使用内置默认值。\n"

func marshalConfig(c *Config) ([]byte, error) {
	var n yaml.Node
	if err := n.Encode(c); err != nil {
		return nil, fmt.Errorf("序列化配置失败：%w", err)
	}
	Annotate(&n)
	out, err := yaml.Marshal(&n)
	if err != nil {
		return nil, fmt.Errorf("序列化配置失败：%w", err)
	}
	return append([]byte(templateHeader), out...), nil
}

// renameFunc 是测试可替换的钩子，用于模拟 rename 失败。
var renameFunc = os.Rename

// SetRenameFunc 仅供测试注入 rename 失败；返回恢复函数。
func SetRenameFunc(fn func(old, new string) error) (restore func()) {
	prev := renameFunc
	renameFunc = fn
	return func() { renameFunc = prev }
}

// SaveFile 原子写入配置文件：同目录临时文件、0600、fsync、rename、目录 fsync。
// 返回值 committed 表示 rename 是否已发生：一旦为 true，磁盘内容已经切换，
// 即使目录同步失败，调用方也应把新配置视为已生效。
func SaveFile(path string, data []byte) (committed bool, err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return false, fmt.Errorf("创建临时文件失败：%w", err)
	}
	tmp := f.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = f.Close()
		}
		_ = os.Remove(tmp)
	}()

	// CreateTemp 默认 0600，这里显式设置，避免受 umask 影响。
	if err := f.Chmod(0o600); err != nil {
		return false, fmt.Errorf("设置临时文件权限失败：%w", err)
	}
	if _, err := f.Write(data); err != nil {
		return false, fmt.Errorf("写入临时文件失败：%w", err)
	}
	if err := f.Sync(); err != nil {
		return false, fmt.Errorf("同步临时文件失败：%w", err)
	}
	if err := f.Close(); err != nil {
		return false, fmt.Errorf("关闭临时文件失败：%w", err)
	}
	if err := renameFunc(tmp, path); err != nil {
		return false, fmt.Errorf("替换配置文件失败：%w", err)
	}
	renamed = true

	d, err := os.Open(dir)
	if err != nil {
		return true, fmt.Errorf("配置已替换，但目录无法打开，持久化未确认：%w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return true, fmt.Errorf("配置已替换，但目录同步失败，持久化未确认：%w", err)
	}
	return true, nil
}

// WriteTemplate 在文件不存在时写入一份完整配置（含全部默认值）。
// 已存在则不覆盖，返回 wrote=false。
func WriteTemplate(path string, c *Config) (wrote bool, err error) {
	if _, statErr := os.Stat(path); statErr == nil {
		return false, nil
	} else if !os.IsNotExist(statErr) {
		return false, fmt.Errorf("检查配置文件失败：%w", statErr)
	}
	out, err := marshalConfig(c)
	if err != nil {
		return false, err
	}
	if _, err := SaveFile(path, out); err != nil {
		return false, err
	}
	return true, nil
}
