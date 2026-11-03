package pluginbytecode

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
)

var update = flag.Bool("update", false, "record the plugins' bytecode in "+golden)

const golden = "testdata/bytecode.txt"

const header = `# The plugins' bytecode, as moejs.Compile compiles bench/testdata/plugins:
# per plugin, its functions, its code words and the SHA-256 of everything
# the compiler made of it. A change here takes the full gate of
# docs/DESIGN.md §14.2. Record with
#   go test ./pluginbytecode -run TestPluginBytecode -update
`

// TestPluginBytecode compiles each plugin and compares the result with
// the recorded one.
func TestPluginBytecode(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "testdata", "plugins", "*", "plugin.js"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Skip("the plugins are not fetched (bench/testdata/plugins/fetch.sh)")
	}
	var got strings.Builder
	for _, p := range paths {
		key := filepath.Base(filepath.Dir(p))
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		// What moejs.Compile does, under the name the bench gives it.
		mod, err := syntax.ParseModule(key+".js", string(src), syntax.Options{})
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		fn, err := compiler.CompileModule(mod)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		h := sha256.New()
		d := dumper{w: h, seen: map[uintptr]bool{}}
		d.value("", reflect.ValueOf(fn))
		funcs, words := count(fn)
		fmt.Fprintf(&got, "%s functions=%d code=%d sha256=%x\n", key, funcs, words, h.Sum(nil))
	}
	if *update {
		if err := os.WriteFile(golden, []byte(header+got.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	data, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for line := range strings.Lines(string(data)) {
		if !strings.HasPrefix(line, "#") {
			want.WriteString(line)
		}
	}
	if got.String() != want.String() {
		t.Errorf("the plugins' bytecode is not the recorded one, so this change takes the full gate of docs/DESIGN.md §14.2 (record it with go test ./pluginbytecode -run TestPluginBytecode -update)\ngot:\n%swant:\n%s", got.String(), want.String())
	}
}

func count(fn *bytecode.Function) (funcs, words int) {
	funcs, words = 1, len(fn.Code)
	for _, c := range fn.Children {
		f, w := count(c)
		funcs += f
		words += w
	}
	return funcs, words
}

// dumper writes every field of a value that is not zero, with its path, in
// declaration order, following each pointer once. A zero field writes
// nothing, so a field added to the bytecode changes no record until the
// compiler sets it.
type dumper struct {
	w    io.Writer
	seen map[uintptr]bool
}

func (d *dumper) value(path string, v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		if d.seen[v.Pointer()] {
			fmt.Fprintf(d.w, "%s=seen\n", path)
			return
		}
		d.seen[v.Pointer()] = true
		d.value(path, v.Elem())
	case reflect.Interface:
		if v.IsNil() {
			return
		}
		fmt.Fprintf(d.w, "%s.type=%s\n", path, v.Elem().Type())
		d.value(path, v.Elem())
	case reflect.Struct:
		t := v.Type()
		for i := range t.NumField() {
			d.value(path+"."+t.Field(i).Name, v.Field(i))
		}
	case reflect.Slice, reflect.Array:
		if v.Len() == 0 {
			return
		}
		fmt.Fprintf(d.w, "%s.len=%d\n", path, v.Len())
		for i := range v.Len() {
			d.value(fmt.Sprintf("%s[%d]", path, i), v.Index(i))
		}
	case reflect.Map:
		if v.Len() == 0 {
			return
		}
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
		fmt.Fprintf(d.w, "%s.len=%d\n", path, v.Len())
		for _, k := range keys {
			d.value(fmt.Sprintf("%s[%v]", path, k), v.MapIndex(k))
		}
	case reflect.Bool:
		if v.Bool() {
			fmt.Fprintf(d.w, "%s=true\n", path)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if v.Int() != 0 {
			fmt.Fprintf(d.w, "%s=%d\n", path, v.Int())
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if v.Uint() != 0 {
			fmt.Fprintf(d.w, "%s=%d\n", path, v.Uint())
		}
	case reflect.Float32, reflect.Float64:
		if b := math.Float64bits(v.Float()); b != 0 {
			fmt.Fprintf(d.w, "%s=%#x\n", path, b)
		}
	case reflect.String:
		// Length first, then the bytes: every function's SourceInfo holds
		// the whole source, which quoting would only slow down.
		if v.Len() != 0 {
			fmt.Fprintf(d.w, "%s=%d:", path, v.Len())
			io.WriteString(d.w, v.String())
			io.WriteString(d.w, "\n")
		}
	default:
		panic(fmt.Sprintf("%s: cannot record a %s", path, v.Kind()))
	}
}
