package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/skpjr001/fluid-agent/internal/config"
)

// The JSON Schemas in schema/ are hand-maintained for editors; this test keeps
// them in lock-step with the Go structs the agent actually decodes.

type jsonSchema map[string]any

func loadSchema(t *testing.T, name string) jsonSchema {
	t.Helper()
	b, err := os.ReadFile(repoFile("schema/" + name))
	if err != nil {
		t.Fatal(err)
	}
	var s jsonSchema
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return s
}

func resolve(root, node jsonSchema) jsonSchema {
	if ref, ok := node["$ref"].(string); ok {
		name := strings.TrimPrefix(ref, "#/$defs/")
		return root["$defs"].(map[string]any)[name].(map[string]any)
	}
	return node
}

// jsonFields lists the JSON names of t's fields, flattening embedded structs.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous && f.Tag.Get("json") == "" {
			for k, v := range jsonFields(f.Type) {
				out[k] = v
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

var durationType = reflect.TypeOf(config.Duration(0))
var timeType = reflect.TypeOf(time.Time{})

func structOf(t reflect.Type) (reflect.Type, bool) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	return t, t.Kind() == reflect.Struct && t != timeType && t != durationType
}

func compareSchema(t *testing.T, path string, root, node jsonSchema, typ reflect.Type) {
	node = resolve(root, node)
	if node["additionalProperties"] != false {
		t.Errorf("%s: objects must set additionalProperties: false", path)
	}
	props, _ := node["properties"].(map[string]any)
	fields := jsonFields(typ)
	var missing, extra []string
	for name := range fields {
		if _, ok := props[name]; !ok {
			missing = append(missing, name)
		}
	}
	for name := range props {
		if _, ok := fields[name]; !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("%s: schema lacks %v", path, missing)
	}
	if len(extra) > 0 {
		t.Errorf("%s: schema has unknown %v", path, extra)
	}
	if req, ok := node["required"].([]any); ok {
		for _, r := range req {
			if _, ok := fields[r.(string)]; !ok {
				t.Errorf("%s: required %q is not a field", path, r)
			}
		}
	}
	for name, ft := range fields {
		st, isStruct := structOf(ft)
		p, ok := props[name].(map[string]any)
		if !isStruct || !ok {
			continue
		}
		child := resolve(root, p)
		if child["type"] == "array" {
			child = resolve(root, child["items"].(map[string]any))
		}
		compareSchema(t, path+"."+name, root, child, st)
	}
}

func TestSchemasMatchStructs(t *testing.T) {
	for file, v := range map[string]any{
		"config.schema.json":  config.StaticConfig{},
		"proxy.schema.json":   ProxyConfig{},
		"admin.schema.json":   AdminConfig{},
		"access.schema.json":  AccessConfig{},
		"webhook.schema.json": Webhook{},
		"ingest.schema.json":  IngestRule{},
		"profile.schema.json": Profile{},
	} {
		s := loadSchema(t, file)
		if s["$schema"] != "https://json-schema.org/draft/2020-12/schema" || s["$id"] != config.SchemaBase+file {
			t.Errorf("%s: bad $schema/$id", file)
		}
		compareSchema(t, file, s, s, reflect.TypeOf(v))
	}
}

func TestSchemasAreEmbeddedAndServed(t *testing.T) {
	ta := newTestApp(t, nil)
	code, body := ta.api("GET", "/api/schema/webhook.schema.json", viewerToken, nil)
	if code != 200 || !strings.Contains(string(body), `"target"`) {
		t.Fatalf("%d %s", code, body)
	}
	if code, _ := ta.api("GET", "/api/schema/../main.go", viewerToken, nil); code == 200 {
		t.Fatal("path traversal must not serve files")
	}
}

func TestExamplesReferenceSchemas(t *testing.T) {
	files, _ := filepath.Glob(repoFile("examples/dynamic/*.json"))
	more, _ := filepath.Glob(repoFile("examples/dynamic/*/*.json"))
	for _, f := range append(append(files, more...), repoFile("examples/config.example.json"), repoFile("examples/config.docker.json")) {
		var m map[string]any
		b, _ := os.ReadFile(f)
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if s, _ := m["$schema"].(string); !strings.HasSuffix(s, ".schema.json") {
			t.Errorf("%s: missing $schema", f)
		}
	}
}

func TestSavedFilesCarrySchema(t *testing.T) {
	ta := newTestApp(t, nil)
	code, body := ta.api("POST", "/api/webhooks", editorToken, map[string]any{"id": "w1", "target": "https://t.example"})
	if code != 201 {
		t.Fatalf("%d %s", code, body)
	}
	b, _ := os.ReadFile(filepath.Join(ta.dir, "webhooks", "w1.json"))
	if !strings.Contains(string(b), `"$schema": "`+config.SchemaBase+`webhook.schema.json"`) {
		t.Fatalf("saved file lacks $schema:\n%s", b)
	}
}
