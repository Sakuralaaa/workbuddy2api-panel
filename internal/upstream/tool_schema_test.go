package upstream

import (
	"encoding/json"
	"reflect"
	"testing"
)

// 用例逐条取自插件 tools.test.mjs（WorkBuddy 各模型实测拒收 / 放行的形状）。

func fitParams(t *testing.T, params string) map[string]any {
	t.Helper()
	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"t","description":"d","parameters":` + params + `}}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("out not json: %v", err)
	}
	fn := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	ps, _ := fn["parameters"].(map[string]any)
	return ps
}

func fitProp(t *testing.T, prop string) any {
	t.Helper()
	return fitParams(t, `{"type":"object","properties":{"a":`+prop+`}}`)["properties"].(map[string]any)["a"]
}

func mustJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("bad json %s: %v", s, err)
	}
	return v
}

func TestFitToolsNulPatternAndSurrogateGuard(t *testing.T) {
	ps := fitParams(t, `{"type":"object","properties":{
		"a":{"type":"string","pattern":"^[^\\0]*$"},
		"b":{"type":"array","items":{"type":"string","pattern":"x\\0"}},
		"c":{"type":"string","pattern":"\\\\0|\\01"},
		"g":{"type":"string","pattern":"^[^\\uD800-\\uDFFF]*$"},
		"m":{"type":"string","pattern":"^[^\\u0000\\uD800-\\uDFFF]*$"}}}`)
	props := ps["properties"].(map[string]any)
	pat := func(k string) any { return props[k].(map[string]any)["pattern"] }
	if pat("a") != `^[^\u0000]*$` {
		t.Errorf("a = %v", pat("a"))
	}
	if props["b"].(map[string]any)["items"].(map[string]any)["pattern"] != `x\u0000` {
		t.Errorf("b = %v", props["b"])
	}
	if pat("c") != `\\0|\01` {
		t.Errorf("c = %v", pat("c"))
	}
	if pat("g") != nil {
		t.Errorf("surrogate guard should be dropped: %v", pat("g"))
	}
	if pat("m") != `^[^\u0000\uD800-\uDFFF]*$` {
		t.Errorf("other surrogate pattern changed: %v", pat("m"))
	}
}

func TestFitToolsObjectRoot(t *testing.T) {
	ps := fitParams(t, `{"anyOf":[
		{"type":"object","properties":{"kind":{"const":"a"},"x":{"type":"string"}},"required":["kind","x"]},
		{"type":"object","properties":{"kind":{"const":"b"},"y":{"type":"number"}},"required":["kind"]}]}`)
	want := mustJSON(t, `{"type":"object","properties":{"kind":{"anyOf":[{"const":"a"},{"const":"b"}]},"x":{"type":"string"},"y":{"type":"number"}},"required":["kind"]}`)
	if !reflect.DeepEqual(any(ps), want) {
		t.Errorf("anyOf root = %v", ps)
	}
	ps = fitParams(t, `{"$defs":{"A":{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}},"allOf":[{"$ref":"#/$defs/A"},{"type":"object","properties":{"b":{"type":"string"}}}]}`)
	if _, has := ps["allOf"]; has || ps["type"] != "object" || !reflect.DeepEqual(ps["required"], []any{"a"}) || len(ps["properties"].(map[string]any)) != 2 {
		t.Errorf("allOf root = %v", ps)
	}
	for _, in := range []string{`{"properties":{"x":{"type":"string"}}}`, `{}`, `null`} {
		ps := fitParams(t, in)
		if ps["type"] != "object" || ps["properties"] == nil {
			t.Errorf("%s -> %v", in, ps)
		}
	}
}

func TestFitToolsTupleItems(t *testing.T) {
	ps := fitParams(t, `{"type":"object","properties":{"p":{"type":"array","items":[{"type":"number"},{"type":"string"}],"additionalItems":false},"q":{"type":"array","items":[{"type":"number"}]}}}`)
	props := ps["properties"].(map[string]any)
	if !reflect.DeepEqual(props["p"], mustJSON(t, `{"type":"array","items":{"anyOf":[{"type":"number"},{"type":"string"}]}}`)) {
		t.Errorf("p = %v", props["p"])
	}
	if !reflect.DeepEqual(props["q"].(map[string]any)["items"], mustJSON(t, `{"type":"number"}`)) {
		t.Errorf("q = %v", props["q"])
	}
}

func TestFitToolsEnumsAndShapes(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"enum":["x","y"],"description":"d"}`, `{"enum":["x","y"],"description":"d","type":"string"}`},
		{`{"enum":[1,2]}`, `{"enum":[1,2],"type":"integer"}`},
		{`{"enum":[1,2.5]}`, `{"enum":[1,2.5],"type":"number"}`},
		{`{"enum":[true,null]}`, `{"enum":[true,null],"type":["boolean","null"]}`},
		{`{"enum":[null]}`, `{"enum":[null],"type":"null"}`},
		{`{"enum":["x",1,null],"description":"d"}`, `{"description":"d","anyOf":[{"type":"string","enum":["x"]},{"type":"integer","enum":[1]},{"type":"null"}]}`},
		{`{"type":["string","integer","null"],"enum":["x",1,null]}`, `{"anyOf":[{"type":"string","enum":["x"]},{"type":"integer","enum":[1]},{"type":"null"}]}`},
		{`{"anyOf":[{"minLength":1}],"enum":["x",1]}`, `{"anyOf":[{"minLength":1}],"allOf":[{"anyOf":[{"type":"string","enum":["x"]},{"type":"integer","enum":[1]}]}]}`},
		// 已被收的形状原样保留
		{`{"type":"string","enum":["a"]}`, `{"type":"string","enum":["a"]}`},
		{`{"type":["integer","null"],"enum":[1,null]}`, `{"type":["integer","null"],"enum":[1,null]}`},
		{`{"type":"string","enum":[1]}`, `{"type":"string","enum":[1]}`},
		// 空 enum、非数组 required、未知 type
		{`{"type":"string","enum":[]}`, `{"type":"string"}`},
		{`{"type":"object","properties":{"x":{"type":"string"}},"required":"x"}`, `{"type":"object","properties":{"x":{"type":"string"}}}`},
		{`{"type":"String"}`, `{"type":"string"}`},
		{`{"type":"any"}`, `{}`},
		{`{"type":["Integer","foo","null"]}`, `{"type":["integer","null"]}`},
		// 嵌套
		{`{"type":"array","items":{"enum":["a"]}}`, `{"type":"array","items":{"enum":["a"],"type":"string"}}`},
	}
	for _, c := range cases {
		if got := fitProp(t, c.in); !reflect.DeepEqual(got, mustJSON(t, c.want)) {
			b, _ := json.Marshal(got)
			t.Errorf("%s -> %s, want %s", c.in, b, c.want)
		}
	}
}

func TestFitToolsBooleanSchemas(t *testing.T) {
	ps := fitParams(t, `{"type":"object","additionalProperties":false,"properties":{"a":true,"b":false,"c":{"type":"array","items":true},"d":{"type":"object","additionalProperties":true}}}`)
	want := mustJSON(t, `{"a":{},"b":{"not":{}},"c":{"type":"array","items":{}},"d":{"type":"object","additionalProperties":true}}`)
	if !reflect.DeepEqual(ps["properties"], want) || ps["additionalProperties"] != false {
		t.Errorf("bool schemas = %v", ps)
	}
	// 名叫 enum / type / required 的属性是名字，不是关键字
	in := `{"type":"object","properties":{"enum":{"type":"string"},"type":{"type":"string","enum":["a"]},"required":{"type":"boolean"}},"required":["enum"]}`
	if got := fitParams(t, in); !reflect.DeepEqual(any(got), mustJSON(t, in)) {
		t.Errorf("property names read as keywords: %v", got)
	}
}

func TestFitToolsTypeUnions(t *testing.T) {
	ab := `[{"type":"object","properties":{"mode":{"type":"string","const":"a"}}},{"type":"object","properties":{"mode":{"type":"string","const":"b"}}}]`
	ps := fitParams(t, `{"type":"object","properties":{"x":{"$ref":"#/$defs/X"}},"$defs":{"X":{"oneOf":`+ab+`}}}`)
	x := ps["$defs"].(map[string]any)["X"].(map[string]any)
	if x["type"] != "object" {
		t.Errorf("oneOf of objects not typed: %v", x)
	}
	in := `{"type":"object","properties":{
		"typed":{"type":"object","oneOf":` + ab + `},
		"mixed":{"oneOf":[{"type":"object"},{"type":"string"}]},
		"missing":{"oneOf":[{"type":"object"},{"$ref":"#/$defs/Nope"}]},
		"external":{"oneOf":[{"type":"object"},{"$ref":"other.json#/X"}]},
		"loop":{"oneOf":[{"type":"object"},{"$ref":"#/$defs/A"}]}},
		"$defs":{"A":{"$ref":"#/$defs/B"},"B":{"$ref":"#/$defs/A"}}}`
	m := mustJSON(t, in).(map[string]any)
	if typeUnions(m, m, 0) {
		t.Errorf("typeUnions changed what it must leave: %v", m)
	}
}
