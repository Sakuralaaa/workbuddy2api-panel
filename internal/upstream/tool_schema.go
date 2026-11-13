// tool_schema.go 把客户端的 function 工具参数 schema 改成 WorkBuddy 各模型都收的形状。
//
// 逐条移植 magpie 官方 WorkBuddy 插件（@magpie-community/opencode-workbuddy-auth
// 0.2.34）的 fitTools：fitSchema / fitShape / fitEnum / typeUnions / objectRoot。
// 背景（插件注释，均为真实账号实测）：WorkBuddy AI 对模型转不过去的工具回 400 11133
// 「Invalid request parameters」——DeepSeek 拒 pattern 里的 \0、元组 items；Kimi 拒无 type
// 的 enum、空 enum、true/false 充当 schema、required 不是数组、未知 type（"any"、
// "String"）、无 type 的 oneOf；部分模型要求根是 object。每处改写语义等价（除空 enum——
// 没有值能匹配它——被删掉）。
//
// 没移植的：工具名里 WorkBuddy 不收的字符（. : / 空格 CJK）改成 _ 的重命名——那要求在
// 回包流里把模型的调用名映射回去，本项目的 SSE 透传没有这条回写链路。
package upstream

import (
	"encoding/json"
	"math"
	"reflect"
	"sort"
	"strings"
)

// fitTools 原地修 obj.tools 里每个 function 工具的 parameters。
func fitTools(obj map[string]any) {
	tools, ok := obj["tools"].([]any)
	if !ok {
		return
	}
	for _, raw := range tools {
		t, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		ps, present := fn["parameters"]
		if !present {
			continue
		}
		m, ok := ps.(map[string]any)
		if !ok {
			fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			continue
		}
		fitSchema(m, 0)
		fitShape(m, 0)
		typeUnions(m, m, 0)
		objectRoot(m, m, 0)
	}
}

// fixNulEscape 插件 NUL_ESCAPE：正则里的 \0（NUL）改成 \u0000（DeepSeek 校验器说 \0
// 「not a 'regex'」）；\\（字面反斜杠）跳过，\01 这类八进制转义不动。
func fixNulEscape(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' && i+1 < len(p) {
			if p[i+1] == '\\' {
				b.WriteString(`\\`)
				i++
				continue
			}
			if p[i+1] == '0' && (i+2 >= len(p) || p[i+2] < '0' || p[i+2] > '9') {
				b.WriteString(`\u0000`)
				i++
				continue
			}
		}
		b.WriteByte(p[i])
	}
	return b.String()
}

// fitSchema 插件同名函数：各层 pattern 的 \0、只排除代理对的 pattern、元组 items。
func fitSchema(s any, depth int) bool {
	if depth > 64 {
		return false
	}
	changed := false
	switch n := s.(type) {
	case []any:
		for _, v := range n {
			if fitSchema(v, depth+1) {
				changed = true
			}
		}
		return changed
	case map[string]any:
		if p, ok := n["pattern"].(string); ok && strings.Contains(p, `\0`) {
			if q := fixNulEscape(p); q != p {
				n["pattern"] = q
				changed = true
			}
		}
		if p, ok := n["pattern"].(string); ok && p == `^[^\uD800-\uDFFF]*$` {
			delete(n, "pattern")
			changed = true
		}
		if items, ok := n["items"].([]any); ok {
			var list []any
			for _, v := range items {
				switch v.(type) {
				case map[string]any, []any:
					list = append(list, v)
				}
			}
			switch len(list) {
			case 0:
				n["items"] = map[string]any{}
			case 1:
				n["items"] = list[0]
			default:
				n["items"] = map[string]any{"anyOf": list}
			}
			delete(n, "additionalItems")
			changed = true
		}
		for _, v := range n {
			if fitSchema(v, depth+1) {
				changed = true
			}
		}
	}
	return changed
}

var schemaTypes = map[string]bool{"string": true, "number": true, "integer": true, "boolean": true, "object": true, "array": true, "null": true}

var (
	subOne  = []string{"items", "additionalItems", "unevaluatedItems", "contains", "not", "if", "then", "else", "propertyNames", "additionalProperties", "unevaluatedProperties"}
	subList = []string{"anyOf", "oneOf", "allOf", "prefixItems"}
	subMap  = []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"}
	boolOK  = map[string]bool{"additionalProperties": true, "unevaluatedProperties": true}
)

// jsonEqual 插件的 JSON.stringify(a) === JSON.stringify(b)。
func jsonEqual(a, b any) bool {
	return reflect.DeepEqual(a, b)
}

// valueType enum 值的 JSON Schema 类型。
func valueType(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case []any:
		return "array"
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) {
			return "integer"
		}
		return "number"
	case int, int64:
		return "integer"
	case string:
		return "string"
	case bool:
		return "boolean"
	}
	return "object"
}

// fitEnum 插件同名函数：给 enum 一个类型（值的类型，或它加 "null"）；多类型值拆成每类型
// 一个 enum 的 anyOf；空 / 非数组 enum 删掉。
func fitEnum(s map[string]any) bool {
	raw, present := s["enum"]
	if !present {
		return false
	}
	vals, ok := raw.([]any)
	if !ok || len(vals) == 0 {
		delete(s, "enum")
		return true
	}
	var listed []any
	switch t := s["type"].(type) {
	case []any:
		listed = t
	case string:
		listed = []any{t}
	}
	nonNull := 0
	for _, t := range listed {
		if t != "null" {
			nonNull++
		}
	}
	if nonNull == 1 {
		return false
	}
	var order []string
	byType := map[string][]any{}
	for _, v := range vals {
		t := valueType(v)
		if _, seen := byType[t]; !seen {
			order = append(order, t)
		}
		byType[t] = append(byType[t], v)
	}
	if _, hasInt := byType["integer"]; hasInt {
		if _, hasNum := byType["number"]; hasNum {
			byType["number"] = append(byType["number"], byType["integer"]...)
			delete(byType, "integer")
		}
	}
	_, hasNull := byType["null"]
	delete(byType, "null")
	var keys []string
	for _, t := range order {
		if _, ok := byType[t]; ok {
			keys = append(keys, t)
		}
	}
	if len(keys) <= 1 {
		var want any
		switch {
		case len(keys) == 0:
			want = "null"
		case hasNull:
			want = []any{keys[0], "null"}
		default:
			want = keys[0]
		}
		if jsonEqual(want, s["type"]) {
			return false
		}
		s["type"] = want
		return true
	}
	split := make([]any, 0, len(keys)+1)
	for _, t := range keys {
		split = append(split, map[string]any{"type": t, "enum": byType[t]})
	}
	if hasNull {
		split = append(split, map[string]any{"type": "null"})
	}
	delete(s, "enum")
	delete(s, "type")
	if _, ok := s["anyOf"].([]any); ok {
		allOf, _ := s["allOf"].([]any)
		s["allOf"] = append(append([]any{}, allOf...), map[string]any{"anyOf": split})
	} else {
		s["anyOf"] = split
	}
	return true
}

// subSchema 插件同名函数：key 下的 schema 是布尔值时，true → {}，false → {"not":{}}
// （additionalProperties / unevaluatedProperties 的布尔值保留）。
func subSchema(v any, key string) any {
	b, ok := v.(bool)
	if !ok || boolOK[key] {
		return v
	}
	if b {
		return map[string]any{}
	}
	return map[string]any{"not": map[string]any{}}
}

// fitShape 插件同名函数：type 归一（小写、去未知、去重）、required 必须是字符串数组、
// enum 补类型、布尔 schema 换成对象，逐层递归（properties 下的名字不当关键字读）。
func fitShape(s map[string]any, depth int) bool {
	if s == nil || depth > 64 {
		return false
	}
	changed := false
	if raw, present := s["type"]; present {
		var list []any
		if arr, ok := raw.([]any); ok {
			list = arr
		} else {
			list = []any{raw}
		}
		var uniq []any
		seen := map[string]bool{}
		for _, t := range list {
			str, ok := t.(string)
			if !ok {
				continue
			}
			str = strings.ToLower(str)
			if !schemaTypes[str] || seen[str] {
				continue
			}
			seen[str] = true
			uniq = append(uniq, str)
		}
		var want any
		switch len(uniq) {
		case 0:
			want = nil
		case 1:
			want = uniq[0]
		default:
			want = uniq
		}
		if !jsonEqual(want, raw) {
			if want == nil {
				delete(s, "type")
			} else {
				s["type"] = want
			}
			changed = true
		}
	}
	if raw, present := s["required"]; present {
		if arr, ok := raw.([]any); !ok {
			delete(s, "required")
			changed = true
		} else {
			kept := make([]any, 0, len(arr))
			for _, k := range arr {
				if _, ok := k.(string); ok {
					kept = append(kept, k)
				}
			}
			if len(kept) != len(arr) {
				s["required"] = kept
				changed = true
			}
		}
	}
	if fitEnum(s) {
		changed = true
	}
	for _, k := range subOne {
		v0, present := s[k]
		if !present {
			continue
		}
		v := subSchema(v0, k)
		if !sameRef(v, v0) {
			s[k] = v
			changed = true
		}
		if m, ok := v.(map[string]any); ok && fitShape(m, depth+1) {
			changed = true
		}
	}
	for _, k := range subList {
		arr, ok := s[k].([]any)
		if !ok {
			continue
		}
		for i, v0 := range arr {
			v := subSchema(v0, k)
			if !sameRef(v, v0) {
				arr[i] = v
				changed = true
			}
			if m, ok := v.(map[string]any); ok && fitShape(m, depth+1) {
				changed = true
			}
		}
	}
	for _, k := range subMap {
		m, ok := s[k].(map[string]any)
		if !ok {
			continue
		}
		for name, v0 := range m {
			v := subSchema(v0, k)
			if !sameRef(v, v0) {
				m[name] = v
				changed = true
			}
			if mm, ok := v.(map[string]any); ok && fitShape(mm, depth+1) {
				changed = true
			}
		}
	}
	return changed
}

// sameRef subSchema 是否原样返回（布尔被换掉时返回新对象）。
func sameRef(a, b any) bool {
	_, ab := a.(bool)
	_, bb := b.(bool)
	return ab == bb
}

// localRef 插件同名函数：root 里本地 $ref（"#/$defs/x"、"#/definitions/x"）指向的 schema，
// 找不到返回 s 自己。
func localRef(root map[string]any, s any) any {
	m, ok := s.(map[string]any)
	if !ok {
		return s
	}
	ref, ok := m["$ref"].(string)
	if !ok || !strings.HasPrefix(ref, "#/") {
		return s
	}
	var at any = root
	for _, k := range strings.Split(ref[2:], "/") {
		k = strings.ReplaceAll(strings.ReplaceAll(k, "~1", "/"), "~0", "~")
		mm, ok := at.(map[string]any)
		if !ok {
			return s
		}
		at = mm[k]
	}
	if am, ok := at.(map[string]any); ok {
		return am
	}
	return s
}

// typeUnions 插件同名函数：无 type 的 oneOf，若每个分支自身或经本地 $ref 链都是 object，
// 补 type:"object"（Kimi 拒无 type 的 oneOf，#378）。其他一律不动。
func typeUnions(s any, root map[string]any, depth int) bool {
	m, ok := s.(map[string]any)
	if !ok || depth > 64 {
		return false
	}
	changed := false
	if _, hasType := m["type"]; !hasType {
		if branches, ok := m["oneOf"].([]any); ok && len(branches) > 0 {
			all := true
			for _, b := range branches {
				if !objectBranch(root, b, nil) {
					all = false
					break
				}
			}
			if all {
				m["type"] = "object"
				changed = true
			}
		}
	}
	for _, k := range subOne {
		if typeUnions(m[k], root, depth+1) {
			changed = true
		}
	}
	for _, k := range subList {
		arr, _ := m[k].([]any)
		for _, v := range arr {
			if typeUnions(v, root, depth+1) {
				changed = true
			}
		}
	}
	for _, k := range subMap {
		mm, ok := m[k].(map[string]any)
		if !ok {
			continue
		}
		for _, v := range mm {
			if typeUnions(v, root, depth+1) {
				changed = true
			}
		}
	}
	return changed
}

// objectBranch 插件同名函数：b 自身或沿本地 $ref 是否 type object（$ref 成环则否）。
func objectBranch(root map[string]any, b any, seen []string) bool {
	m, ok := b.(map[string]any)
	if !ok {
		return false
	}
	if t, has := m["type"]; has {
		return t == "object"
	}
	ref, ok := m["$ref"].(string)
	if !ok || len(seen) > 16 {
		return false
	}
	for _, r := range seen {
		if r == ref {
			return false
		}
	}
	t := localRef(root, m)
	if tm, ok := t.(map[string]any); !ok || reflect.ValueOf(tm).Pointer() == reflect.ValueOf(m).Pointer() {
		return false
	}
	return objectBranch(root, t, append(append([]string{}, seen...), ref))
}

// deepClone 插件 structuredClone 的 JSON 树深拷贝。
func deepClone(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if json.Unmarshal(raw, &out) != nil {
		return v
	}
	return out
}

func hasUnion(m map[string]any) bool {
	for _, k := range []string{"allOf", "anyOf", "oneOf"} {
		if _, ok := m[k].([]any); ok {
			return true
		}
	}
	return false
}

// objectRoot 插件同名函数：工具参数的根做成 object——allOf 分支整体合并；anyOf/oneOf 的
// object 分支合并 properties，每个分支都必填的字段保持必填。无 type / 空根即 object。
func objectRoot(ps, root map[string]any, depth int) bool {
	var unions []string
	for _, k := range []string{"allOf", "anyOf", "oneOf"} {
		if _, ok := ps[k].([]any); ok {
			unions = append(unions, k)
		}
	}
	if ps["type"] == "object" && len(unions) == 0 {
		return false
	}
	props, ok := ps["properties"].(map[string]any)
	if !ok {
		props = map[string]any{}
	}
	var required []any
	if r, ok := ps["required"].([]any); ok {
		required = append(required, r...)
	}
	resolve := func(b0 any) map[string]any {
		b, ok := localRef(root, b0).(map[string]any)
		if !ok {
			return nil
		}
		if depth < 8 && hasUnion(b) {
			if c, ok := deepClone(b).(map[string]any); ok {
				b = c
				objectRoot(b, root, depth+1)
			}
		}
		return b
	}
	allOf, _ := ps["allOf"].([]any)
	for _, b0 := range allOf {
		b := resolve(b0)
		if b == nil {
			continue
		}
		bp, _ := b["properties"].(map[string]any)
		for _, k := range sortedKeys(bp) {
			if _, has := props[k]; !has {
				props[k] = bp[k]
			}
		}
		if r, ok := b["required"].([]any); ok {
			required = append(required, r...)
		}
	}
	var branches []map[string]any
	for _, key := range []string{"anyOf", "oneOf"} {
		list, _ := ps[key].([]any)
		for _, b0 := range list {
			b := resolve(b0)
			if b == nil {
				continue
			}
			if _, hasProps := b["properties"]; b["type"] != "object" && !hasProps {
				continue
			}
			branches = append(branches, b)
		}
	}
	var fromOrder []string
	from := map[string][]any{}
	for _, b := range branches {
		bp, _ := b["properties"].(map[string]any)
		for _, k := range sortedKeys(bp) {
			if _, has := props[k]; has {
				continue
			}
			v := bp[k]
			seen := from[k]
			if _, known := from[k]; !known {
				fromOrder = append(fromOrder, k)
			}
			dup := false
			for _, x := range seen {
				if jsonEqual(x, v) {
					dup = true
					break
				}
			}
			if !dup {
				seen = append(seen, v)
			}
			from[k] = seen
		}
	}
	for _, k := range fromOrder {
		list := from[k]
		if len(list) == 1 {
			props[k] = list[0]
		} else {
			props[k] = map[string]any{"anyOf": list}
		}
	}
	if len(branches) > 0 {
		bp, _ := branches[0]["properties"].(map[string]any)
		for _, k := range sortedKeys(bp) {
			every := true
			for _, b := range branches {
				r, ok := b["required"].([]any)
				if !ok || !containsAny(r, k) {
					every = false
					break
				}
			}
			if every {
				required = append(required, k)
			}
		}
	}
	for _, k := range unions {
		delete(ps, k)
	}
	ps["type"] = "object"
	ps["properties"] = props
	var req []any
	seenReq := map[string]bool{}
	for _, k := range required {
		str, ok := k.(string)
		if !ok || seenReq[str] {
			continue
		}
		if _, in := props[str]; !in {
			continue
		}
		seenReq[str] = true
		req = append(req, str)
	}
	if len(req) > 0 {
		ps["required"] = req
	} else {
		delete(ps, "required")
	}
	return true
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func containsAny(list []any, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
