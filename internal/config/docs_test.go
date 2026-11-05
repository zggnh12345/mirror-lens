package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// 结构体里每个 yaml 键都必须有说明，新增字段漏注释会被这里拦下。
func TestEveryFieldHasDoc(t *testing.T) {
	for p := range yamlPaths(reflect.TypeOf(Config{})) {
		if Docs(p) == "" {
			t.Errorf("字段 %s 缺少说明", p)
		}
	}
}

// 生成的模板里，每个键上方都要有注释。
func TestTemplateEveryKeyCommented(t *testing.T) {
	out, err := marshalConfig(Default())
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	var check func(*yaml.Node, string)
	check = func(n *yaml.Node, prefix string) {
		if n == nil {
			return
		}
		if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
			check(n.Content[0], prefix)
			return
		}
		if n.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			p := k.Value
			if prefix != "" {
				p = prefix + "." + k.Value
			}
			if k.HeadComment == "" && k.LineComment == "" {
				t.Errorf("模板里的键 %s 没有注释", p)
			}
			check(v, p)
		}
	}
	check(&doc, "")
}

// 手写注释不能被自动注释覆盖。
func TestAnnotateKeepsExistingComment(t *testing.T) {
	src := "cache:\n    # 我自己写的说明\n    max_gb: 50\n"
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	Annotate(&doc)
	out, err := yaml.Marshal(&doc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "# 我自己写的说明") {
		t.Fatalf("原注释被覆盖：\n%s", out)
	}
}
